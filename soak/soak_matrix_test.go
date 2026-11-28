//go:build soak

package tail

// soak_matrix_test.go is a soak matrix for PR #1 ("fix(polling): drain
// rotated files before reopening"). Its build tag keeps it out of normal
// builds here; run.sh copies it into the package directory, so that it builds
// unchanged against master and against the PR head, and it uses only the
// public TailFile API plus the in-package errStopAtEOF sentinel.
//
// The polling watcher is the only watcher exercised: Podman tails its events
// log with Poll: true and the PR is scoped to the polling watcher. The inotify
// watcher is out of scope.
//
// Every scenario is an independent subtest with its own temporary directory
// and its own tail. The subtests run concurrently (TAIL_SOAK_SEQ=1 serialises
// them). Each writes one JSON record to TAIL_SOAK_OUT; compare the records of
// two module copies (master and PR head) to get the matrix. Nothing here
// fails a test because lines were lost or a stop hung: that is what the
// records report.
//
// Environment:
//
//	TAIL_SOAK_OUT    output directory for the JSON records (default ./soak-out)
//	TAIL_SOAK_POLL   watch.POLL_DURATION (default 250ms, the library default;
//	                 overrides the 5ms that TestMain in tail_test.go forces)
//	TAIL_SOAK_ONLY   comma-separated scenario ids to run (default all)
//	TAIL_SOAK_SCALE  multiplies the iteration counts (default 1; smoke: 0.02)
//	TAIL_SOAK_SEQ    "1" runs the scenarios one after another
//	TAIL_SOAK_DUMP   "1" also writes the delivered lines to <id>.lines.tsv

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nxadm/tail/ratelimiter"
	"github.com/nxadm/tail/watch"
)

const (
	soakWait       = 3 * time.Second  // bound for one expected line
	soakNoticeWait = 5 * time.Second  // bound for a rate-limit notice
	soakStopWait   = 5 * time.Second  // bound for Stop and StopAtEOF
	soakHangBound  = 2 * time.Second  // bound that declares a stop hung (S12)
	soakGateMax    = 30 * time.Second // safety release of a gated consumer
	soakHangAbort  = 40               // abort a scenario after this many hangs
)

// soakSettle separates two rounds of a scenario. Without it the next round
// starts within microseconds of the last delivery, which is not how rotations
// happen and which races the tail's own bookkeeping after a delivery.
const soakSettle = 50 * time.Millisecond

var soakScale = 1.0

// soakCount scales an iteration count, keeping at least one iteration.
func soakCount(base int) int {
	n := int(float64(base)*soakScale + 0.999)
	if n < 1 {
		n = 1
	}
	return n
}

func soakMs(d time.Duration) float64 { return float64(d) / 1e6 }

func soakErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------- recording

// soakLine is one delivered line as the consumer saw it.
type soakLine struct {
	Text   string
	At     time.Time
	Notice bool // the rate-limit notice (Line.Err != nil)
}

type soakState int

const (
	soakFound soakState = iota
	soakTimeout
	soakClosed
)

// soakGate stops the consumer after the first line matching pred, until the
// gate is released. The tail is then blocked handing over the next line.
type soakGate struct {
	pred func(*Line) bool
	hit  chan struct{}
	open chan struct{}
	once sync.Once
}

func (g *soakGate) release() { g.once.Do(func() { close(g.open) }) }

// soakCons reads tail.Lines into a record list with arrival times.
type soakCons struct {
	mu     sync.Mutex
	cond   *sync.Cond
	recs   []soakLine
	closed bool
	gates  []*soakGate
	g      *soakGate
	dead   chan struct{}
}

func soakConsume(lines chan *Line) *soakCons {
	c := &soakCons{dead: make(chan struct{})}
	c.cond = sync.NewCond(&c.mu)
	go c.run(lines)
	return c
}

func (c *soakCons) run(lines chan *Line) {
	for {
		l, ok := <-lines
		if !ok {
			c.mu.Lock()
			c.closed = true
			c.cond.Broadcast()
			c.mu.Unlock()
			close(c.dead)
			return
		}
		now := time.Now()
		c.mu.Lock()
		c.recs = append(c.recs, soakLine{Text: l.Text, At: now, Notice: l.Err != nil})
		var g *soakGate
		if c.g != nil && c.g.pred(l) {
			g, c.g = c.g, nil
		}
		c.cond.Broadcast()
		c.mu.Unlock()
		if g != nil {
			close(g.hit)
			select {
			case <-g.open:
			case <-time.After(soakGateMax):
			}
		}
	}
}

func (c *soakCons) armGate(pred func(*Line) bool) *soakGate {
	g := &soakGate{pred: pred, hit: make(chan struct{}), open: make(chan struct{})}
	c.mu.Lock()
	c.gates = append(c.gates, g)
	c.g = g
	c.mu.Unlock()
	return g
}

// releaseAll opens every gate, armed or already hit.
func (c *soakCons) releaseAll() {
	c.mu.Lock()
	gates := c.gates
	c.gates, c.g = nil, nil
	c.mu.Unlock()
	for _, g := range gates {
		g.release()
	}
}

// wait returns the first record at cursor or later that satisfies pred.
func (c *soakCons) wait(cursor int, timeout time.Duration, pred func(*soakLine) bool) (int, soakState) {
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := time.AfterFunc(timeout, c.cond.Broadcast)
	defer timer.Stop()
	for {
		for i := cursor; i < len(c.recs); i++ {
			if pred(&c.recs[i]) {
				return i, soakFound
			}
		}
		cursor = len(c.recs)
		if c.closed {
			return cursor, soakClosed
		}
		if !time.Now().Before(deadline) {
			return cursor, soakTimeout
		}
		c.cond.Wait()
	}
}

func (c *soakCons) waitText(cursor int, text string, timeout time.Duration) (int, soakState) {
	return c.wait(cursor, timeout, func(l *soakLine) bool { return l.Text == text })
}

func (c *soakCons) snapshot() []soakLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]soakLine(nil), c.recs...)
}

func (c *soakCons) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recs)
}

// quiet returns once no line arrived for d, bounded by max.
func (c *soakCons) quiet(d, max time.Duration) {
	deadline := time.Now().Add(max)
	for {
		_, st := c.wait(c.len(), d, func(*soakLine) bool { return true })
		if st != soakFound || !time.Now().Before(deadline) {
			return
		}
	}
}

func soakWaitDead(c *soakCons) {
	select {
	case <-c.dead:
	case <-time.After(3 * time.Second):
	}
}

// soakLogger keeps what the tail logs; the drain and the reopen log here.
type soakLogger struct {
	*log.Logger
	mu   sync.Mutex
	msgs []string
	ch   chan string
}

func soakNewLogger() *soakLogger {
	return &soakLogger{Logger: log.New(io.Discard, "", 0), ch: make(chan string, 256)}
}

func (l *soakLogger) Printf(format string, v ...interface{}) {
	s := fmt.Sprintf(format, v...)
	l.mu.Lock()
	l.msgs = append(l.msgs, s)
	l.mu.Unlock()
	select {
	case l.ch <- s:
	default:
	}
}

func (l *soakLogger) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if strings.Contains(m, substr) {
			n++
		}
	}
	return n
}

// wait returns once the tail logged a message containing substr.
func (l *soakLogger) wait(substr string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case m := <-l.ch:
			if strings.Contains(m, substr) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// ---------------------------------------------------------------- accounting

// soakAccount compares the delivered lines with the expected sequence, whose
// texts are unique. delivered counts the expected lines that came at least
// once, missing the ones that never came, duplicates further copies of an
// expected line, unexpected lines that were never written for this scenario,
// and misordered expected lines that came out of turn (a lower rotation index
// after a higher one, by first arrival).
func soakAccount(expected []string, recs []soakLine) (delivered, missing, duplicates, misordered, unexpected, notices int, missingSample, unexpectedSample []string) {
	want := make(map[string]int, len(expected))
	for i, e := range expected {
		want[e] = i
	}
	seen := make(map[string]bool, len(expected))
	var order []int
	for _, r := range recs {
		if r.Notice {
			notices++
			continue
		}
		i, ok := want[r.Text]
		if !ok {
			unexpected++
			if len(unexpectedSample) < 10 {
				unexpectedSample = append(unexpectedSample, r.Text)
			}
			continue
		}
		if seen[r.Text] {
			duplicates++
			continue
		}
		seen[r.Text] = true
		order = append(order, i)
	}
	for _, e := range expected {
		if !seen[e] && len(missingSample) < 10 {
			missingSample = append(missingSample, e)
		}
	}
	delivered = len(seen)
	missing = len(expected) - delivered
	for k := 1; k < len(order); k++ {
		if order[k] < order[k-1] {
			misordered++
		}
	}
	return
}

type soakLat struct {
	N    int     `json:"n"`
	Max  float64 `json:"max"`
	P90  float64 `json:"p90"`
	Mean float64 `json:"mean"`
}

func (l soakLat) String() string {
	return fmt.Sprintf("n=%d max=%.1f p90=%.1f mean=%.1f", l.N, l.Max, l.P90, l.Mean)
}

type soakStat struct{ vals []float64 }

func (s *soakStat) add(v float64) { s.vals = append(s.vals, v) }

// json summarises the samples: max, p90 (smallest value above 90% of the
// samples) and mean, in the unit the samples were added in.
func (s *soakStat) json() soakLat {
	var out soakLat
	if len(s.vals) == 0 {
		return out
	}
	v := append([]float64(nil), s.vals...)
	sort.Float64s(v)
	for _, x := range v {
		out.Mean += x
	}
	out.N, out.Mean, out.Max = len(v), out.Mean/float64(len(v)), v[len(v)-1]
	out.P90 = v[(len(v)*9+9)/10-1]
	return out
}

// ---------------------------------------------------------------- results

type soakResult struct {
	Scenario   string                 `json:"scenario"`
	Name       string                 `json:"name"`
	Version    string                 `json:"version,omitempty"`
	GoVersion  string                 `json:"go_version"`
	PollMs     float64                `json:"poll_ms"`
	Iterations int                    `json:"iterations"`
	Expected   int                    `json:"expected"`
	Delivered  int                    `json:"delivered"`
	Missing    int                    `json:"missing"`
	Duplicates int                    `json:"duplicates"`
	Misordered int                    `json:"misordered"`
	Unexpected int                    `json:"unexpected"`
	Notices    int                    `json:"notices"`
	Hangs      int                    `json:"hangs"`
	Deaths     int                    `json:"deaths"`
	DeathErr   string                 `json:"death_err,omitempty"`
	TailErr    string                 `json:"tail_err,omitempty"`
	Aborted    bool                   `json:"aborted"`
	WallMs     float64                `json:"wall_ms"`
	Extra      map[string]interface{} `json:"extra,omitempty"`

	// Recs are the delivered lines; kept for TAIL_SOAK_DUMP, not serialised.
	Recs []soakLine `json:"-"`
}

func soakNewResult() *soakResult {
	return &soakResult{Extra: map[string]interface{}{}}
}

func (res *soakResult) account(exp []string, recs []soakLine) {
	d, m, dup, mis, un, n, missS, unexpS := soakAccount(exp, recs)
	res.Expected, res.Delivered, res.Missing, res.Duplicates, res.Misordered, res.Unexpected, res.Notices =
		len(exp), d, m, dup, mis, un, n
	res.Recs = recs
	if missS != nil {
		res.Extra["missing_sample"] = missS
	}
	if unexpS != nil {
		res.Extra["unexpected_sample"] = unexpS
	}
}

func (res *soakResult) line() string {
	s := fmt.Sprintf("%s %s [%s] poll=%.0fms: %d/%d lines delivered, missing %d, dup %d, misordered %d, unexpected %d, notices %d, hangs %d, deaths %d, iters %d, wall %.0fms",
		res.Scenario, res.Name, res.Version, res.PollMs, res.Delivered, res.Expected, res.Missing, res.Duplicates,
		res.Misordered, res.Unexpected, res.Notices, res.Hangs, res.Deaths, res.Iterations, res.WallMs)
	keys := make([]string, 0, len(res.Extra))
	for k := range res.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s += fmt.Sprintf("\n    %s: %v", k, res.Extra[k])
	}
	return s
}

// ---------------------------------------------------------------- file helpers

func soakWrite(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func soakAppend(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func soakRename(t *testing.T, old, new string) {
	t.Helper()
	if err := os.Rename(old, new); err != nil {
		t.Fatal(err)
	}
}

func soakRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// soakStart tails path from the beginning with the polling watcher.
func soakStart(t *testing.T, path string, cfg Config, lg *soakLogger) *Tail {
	t.Helper()
	cfg.Follow, cfg.ReOpen, cfg.Poll, cfg.Logger = true, true, true, lg
	tail, err := TailFile(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tail
}

func soakExpectLine(c *soakCons, text string, timeout time.Duration) bool {
	_, st := c.waitText(0, text, timeout)
	return st == soakFound
}

// soakStop stops the tail, bounded by soakStopWait; hung reports that Stop did
// not return in time.
func soakStop(tail *Tail) (ms float64, hung bool) {
	t0 := time.Now()
	done := make(chan struct{})
	go func() {
		tail.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(soakStopWait):
		hung = true
	}
	return soakMs(time.Since(t0)), hung
}

// soakFDs counts the descriptors of this process.
func soakFDs() int {
	d, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(d)
}

// soakFDsIn counts the descriptors that refer to files under dir, including
// unlinked ones.
func soakFDsIn(dir string) int {
	d, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range d {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err == nil && strings.HasPrefix(target, dir) {
			n++
		}
	}
	return n
}

// soakGoroutineIn reports whether one goroutine's stack trace contains every
// text in want and any text in any, but none in none. any may be empty. The
// runtime frames are elided from the traces (Go 1.24 and later), so a wait
// state is matched on the header, e.g. "[select]" or "[chan send]".
func soakGoroutineIn(want, any, none []string) bool {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	if n == len(buf) {
		return false // truncated: cannot tell
	}
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(g, w)
		}
		if len(any) > 0 {
			has := false
			for _, w := range any {
				has = has || strings.Contains(g, w)
			}
			ok = ok && has
		}
		for _, w := range none {
			ok = ok && !strings.Contains(g, w)
		}
		if ok {
			return true
		}
	}
	return false
}

func soakWaitBlocked(want, any, none []string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		if soakGoroutineIn(want, any, none) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- scenarios

// soakRun is one scenario over one file.
type soakRun struct {
	t    *testing.T
	dir  string
	path string
	lg   *soakLogger
	tail *Tail
	c    *soakCons
	cur  int
	exp  []string
	res  *soakResult
	t0   time.Time
}

func soakNew(t *testing.T) *soakRun {
	dir := t.TempDir()
	return &soakRun{t: t, dir: dir, path: filepath.Join(dir, "log"), lg: soakNewLogger(),
		res: soakNewResult(), t0: time.Now()}
}

// alive is false once the tail died or the scenario hit its hang budget.
func (r *soakRun) alive() bool { return r.res.Deaths == 0 && r.res.Hangs < soakHangAbort }

func (r *soakRun) start(cfg Config) {
	r.tail = soakStart(r.t, r.path, cfg, r.lg)
	r.c = soakConsume(r.tail.Lines)
}

func (r *soakRun) fail(st soakState) {
	if st == soakClosed {
		r.res.Deaths++
		r.res.DeathErr = soakErrString(r.tail.Err())
	} else {
		r.res.Hangs++
	}
}

// expect waits for the line text. False means a hang or a dead tail, which is
// recorded.
func (r *soakRun) expect(text string) bool {
	idx, st := r.c.waitText(r.cur, text, soakWait)
	if st == soakFound {
		r.cur = idx + 1
		return true
	}
	r.cur = idx
	r.fail(st)
	return false
}

// soakLines builds one line p+name per name.
func soakLines(p string, names ...string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(p)
		b.WriteString(n)
		b.WriteByte('\n')
	}
	return b.String()
}

// expAdd adds p+name per name to the expected lines.
func (r *soakRun) expAdd(p string, names ...string) {
	for _, n := range names {
		r.exp = append(r.exp, p+n)
	}
}

// blockedDrain appends old, renames the tailed file to path+suffix and creates
// the replacement with repl. Every line of this round starts with p; the
// consumer stops reading after the first one, which leaves the tail blocked
// in the drain. hit reports whether that line came.
func (r *soakRun) blockedDrain(p, suffix, old, repl string) (g *soakGate, hit bool) {
	g = r.c.armGate(func(l *Line) bool { return strings.HasPrefix(l.Text, p) })
	soakAppend(r.t, r.path, old)
	soakRename(r.t, r.path, r.path+suffix)
	soakWrite(r.t, r.path, repl)
	select {
	case <-g.hit:
		return g, true
	case <-r.c.dead:
		r.fail(soakClosed)
	case <-time.After(soakWait):
		r.res.Hangs++
	}
	return g, false
}

// finish waits for the tail to settle, stops it and fills in the counts.
func (r *soakRun) finish() *soakResult {
	if r.c != nil {
		if r.res.Deaths == 0 {
			r.c.releaseAll()
			r.c.quiet(1500*time.Millisecond, 10*time.Second)
			stopMs, hung := soakStop(r.tail)
			if hung {
				r.res.Hangs++
			}
			r.res.Extra["stop_ms"] = stopMs
		}
		r.c.releaseAll()
		soakWaitDead(r.c)
		r.res.TailErr = soakErrString(r.tail.Err())
		r.res.Aborted = !r.alive()
		r.res.WallMs = soakMs(time.Since(r.t0))
		r.res.account(r.exp, r.c.snapshot())
	}
	return r.res
}

// soakPause is the pause between append and rename in S03: an LCG seeded with
// 1 (x = x*6364136223846793005 + 1442695040888963407), pause = (x >> 33) mod 301 ms.
var soakRandState = uint64(1)

func soakPause(int) time.Duration {
	soakRandState = soakRandState*6364136223846793005 + 1442695040888963407
	return time.Duration(soakRandState>>33%301) * time.Millisecond
}

// soakRotate runs n isolated rotations. Each appends five lines, waits pause
// (nil for none), renames the file, waits createGap and creates the
// replacement with one line. A rotation is over once the tail delivers the
// replacement's line; only then does the next one start.
func soakRotate(t *testing.T, n int, pause func(int) time.Duration, createGap time.Duration) *soakResult {
	r := soakNew(t)
	ren := make([]time.Time, n+1)
	soakWrite(t, r.path, "g0:a\n")
	r.start(Config{})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		for i := 1; i <= n && r.alive(); i++ {
			r.res.Iterations = i
			soakAppend(t, r.path, fmt.Sprintf("r%d:a\nr%d:b\nr%d:c\nr%d:d\nr%d:e\n", i, i, i, i, i))
			if pause != nil {
				time.Sleep(pause(i))
			}
			ren[i] = time.Now()
			soakRename(t, r.path, r.path+"."+strconv.Itoa(i))
			if createGap > 0 {
				time.Sleep(createGap)
			}
			soakWrite(t, r.path, fmt.Sprintf("g%d:a\n", i))
			for _, s := range []string{"a", "b", "c", "d", "e"} {
				r.exp = append(r.exp, fmt.Sprintf("r%d:%s", i, s))
			}
			r.exp = append(r.exp, fmt.Sprintf("g%d:a", i))
			r.expect(fmt.Sprintf("g%d:a", i))
			time.Sleep(soakSettle)
		}
	}
	res := r.finish()

	// Latency per rotation, from the rename to the arrival of the last of the
	// five pre-rename lines and of the replacement's first line. Rotations
	// whose five lines all arrived before the rename (the poll fell into the
	// pause) are counted separately and excluded from the latency samples.
	first := map[string]time.Time{}
	for _, l := range r.c.snapshot() {
		if _, ok := first[l.Text]; !ok {
			first[l.Text] = l.At
		}
	}
	var lastOld, firstNew soakStat
	drained, before := 0, 0
	for i := 1; i <= res.Iterations; i++ {
		var last time.Time
		ok := true
		for _, s := range []string{"a", "b", "c", "d", "e"} {
			at, has := first[fmt.Sprintf("r%d:%s", i, s)]
			ok = ok && has
			if at.After(last) {
				last = at
			}
		}
		if at, has := first[fmt.Sprintf("g%d:a", i)]; has {
			firstNew.add(soakMs(at.Sub(ren[i])))
		}
		if !ok {
			continue
		}
		drained++
		if !last.After(ren[i]) {
			before++
			continue
		}
		lastOld.add(soakMs(last.Sub(ren[i])))
	}
	res.Extra["rotations_with_all_5_old_lines"] = drained
	res.Extra["rotations_old_lines_delivered_before_rename"] = before
	res.Extra["rename_to_last_old_line_ms"] = lastOld.json()
	res.Extra["rename_to_first_replacement_line_ms"] = firstNew.json()
	return res
}

// S04: the consumer stalls for 300 ms after the first line of a rotation and
// the file is rotated a second time during that stall.
func soakS04(t *testing.T) *soakResult {
	r := soakNew(t)
	soakWrite(t, r.path, "g0:a\n")
	r.start(Config{})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		for i := 1; i <= soakCount(30) && r.alive(); i++ {
			r.res.Iterations = i
			p := fmt.Sprintf("s4:%d:", i)
			r.expAdd(p, "a1", "a2", "a3", "b1", "b2", "c1", "c2")
			g, hit := r.blockedDrain(p, ".1."+strconv.Itoa(i), soakLines(p, "a1", "a2", "a3"), soakLines(p, "b1", "b2"))
			if hit {
				time.Sleep(300 * time.Millisecond)
			}
			soakRename(t, r.path, r.path+".2."+strconv.Itoa(i))
			soakWrite(t, r.path, soakLines(p, "c1", "c2"))
			g.release()
			r.expect(p + "c2")
			time.Sleep(soakSettle)
		}
	}
	return r.finish()
}

// S05: the replacement is deleted during the drain and recreated 500 ms later.
func soakS05(t *testing.T) *soakResult {
	r := soakNew(t)
	soakWrite(t, r.path, "g0:a\n")
	r.start(Config{})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		for i := 1; i <= soakCount(30) && r.alive(); i++ {
			r.res.Iterations = i
			p := fmt.Sprintf("s5:%d:", i)
			r.expAdd(p, "a1", "a2", "a3", "b1", "b2", "d1", "d2")
			g, hit := r.blockedDrain(p, ".old."+strconv.Itoa(i), soakLines(p, "a1", "a2", "a3"), soakLines(p, "b1", "b2"))
			if hit {
				soakRemove(t, r.path)
				time.Sleep(500 * time.Millisecond)
				soakWrite(t, r.path, soakLines(p, "d1", "d2"))
			}
			g.release()
			r.expect(p + "d2")
			time.Sleep(soakSettle)
		}
	}
	return r.finish()
}

// S06: the file is deleted with three unread lines and recreated 500 ms later.
func soakS06(t *testing.T) *soakResult {
	r := soakNew(t)
	soakWrite(t, r.path, "g0:a\n")
	r.start(Config{})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		for i := 1; i <= soakCount(30) && r.alive(); i++ {
			r.res.Iterations = i
			p := fmt.Sprintf("s6:%d:", i)
			soakAppend(t, r.path, soakLines(p, "a1", "a2", "a3"))
			soakRemove(t, r.path)
			time.Sleep(500 * time.Millisecond)
			soakWrite(t, r.path, soakLines(p, "d1", "d2"))
			r.expAdd(p, "a1", "a2", "a3", "d1", "d2")
			r.expect(p + "d2")
			time.Sleep(soakSettle)
		}
	}
	return r.finish()
}

// soakS07 truncates the file in place and, once the tail logged its reopen,
// writes two new lines. settle pauses between the reopen and the write.
// Without the pause the write races the tail's first read after the reopen;
// when the tail wins that race it keeps the poller that reported the
// truncation, whose last seen size is zero, and a truncation that follows
// within one poll interval goes unnoticed (S07f).
func soakS07(t *testing.T, settle bool) *soakResult {
	r := soakNew(t)
	soakWrite(t, r.path, "g0:a\n")
	r.start(Config{})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		for i := 1; i <= soakCount(50) && r.alive(); i++ {
			r.res.Iterations = i
			p := fmt.Sprintf("s7:%d:", i)
			if err := os.Truncate(r.path, 0); err != nil {
				t.Fatal(err)
			}
			if !r.lg.wait("Re-opening truncated", soakWait) {
				r.res.Hangs++
			}
			if settle {
				time.Sleep(soakSettle)
			}
			soakWrite(t, r.path, soakLines(p, "a", "b"))
			r.expAdd(p, "a", "b")
			r.expect(p + "b")
			time.Sleep(soakSettle)
		}
	}
	return r.finish()
}

// S08: Stop and StopAtEOF while the tail is blocked in the drain (the consumer
// took one line of the rotation and stopped reading). Reports the latency to
// return and the descriptors of the tailed file during the drain and after
// the stop.
func soakS08(t *testing.T) *soakResult {
	res := soakNewResult()
	t0 := time.Now()
	n := soakCount(30)
	var exp []string
	var recs []soakLine
	for _, kind := range []string{"Stop", "StopAtEOF"} {
		var latUs, fdDuring, fdAfter, fdDelta soakStat
		hungN, blocked, gateOld, gateNew, afterStop, reopened, errEOF := 0, 0, 0, 0, 0, 0, 0
		for i := 1; i <= n && res.Hangs < soakHangAbort; i++ {
			dir := t.TempDir()
			path := filepath.Join(dir, "log")
			lg := soakNewLogger()
			p := fmt.Sprintf("s8:%s:%d:", kind, i)
			soakWrite(t, path, p+"0\n")
			exp = append(exp, p+"0", p+"w1", p+"w2", p+"w3", p+"b1", p+"b2")
			base := soakFDs()
			tail := soakStart(t, path, Config{}, lg)
			c := soakConsume(tail.Lines)
			if !soakExpectLine(c, p+"0", soakWait) {
				res.Hangs++
				soakStop(tail)
				continue
			}
			g := c.armGate(func(l *Line) bool { return strings.HasPrefix(l.Text, p) })
			soakAppend(t, path, soakLines(p, "w1", "w2", "w3"))
			soakRename(t, path, path+".old")
			soakWrite(t, path, soakLines(p, "b1", "b2"))
			select {
			case <-g.hit:
			case <-c.dead:
				res.Deaths++
				res.DeathErr = soakErrString(tail.Err())
				continue
			case <-time.After(soakWait):
				res.Hangs++
				g.release()
				soakStop(tail)
				continue
			}
			// The tail is now blocked handing over its next line.
			if soakWaitBlocked([]string{"tail.(*Tail).sendLine"}, []string{"[select]"}, nil, time.Second) {
				blocked++
			}
			fdDuring.add(float64(soakFDsIn(dir)))
			t1 := time.Now()
			done := make(chan error, 1)
			go func() {
				if kind == "Stop" {
					done <- tail.Stop()
					return
				}
				done <- tail.StopAtEOF()
			}()
			check := func(err error) {
				if kind == "StopAtEOF" && err == errStopAtEOF {
					errEOF++
				}
			}
			hung := false
			select {
			case err := <-done:
				check(err)
			case <-time.After(soakStopWait):
				hung, hungN = true, hungN+1
				res.Hangs++
				g.release()
				select {
				case err := <-done:
					check(err)
				case <-time.After(soakStopWait):
				}
			}
			latUs.add(float64(time.Since(t1).Nanoseconds()) / 1e3)
			if !hung {
				fdAfter.add(float64(soakFDsIn(dir)))
				fdDelta.add(float64(soakFDs() - base))
			}
			g.release()
			soakWaitDead(c)
			rs := c.snapshot()
			seen := false
			for _, l := range rs {
				if seen {
					if !l.Notice {
						afterStop++
					}
					continue
				}
				if l.Text != p+"0" && strings.HasPrefix(l.Text, p) {
					seen = true
					if strings.HasSuffix(l.Text, ":w1") {
						gateOld++
					} else {
						gateNew++
					}
				}
			}
			recs = append(recs, rs...)
			if lg.count("Re-opening moved") > 0 {
				reopened++
			}
		}
		res.Extra[kind] = map[string]interface{}{
			"n": n, "hung": hungN,
			"latency_us":                 latUs.json(),
			"blocked_in_sendline":        blocked,
			"gate_line_from_old_file":    gateOld,
			"gate_line_from_replacement": gateNew,
			"lines_delivered_after_stop": afterStop,
			"reopened_before_stop":       reopened,
			"err_stopateof":              errEOF,
			"file_fds_during_drain":      fdDuring.json(),
			"file_fds_after_stop":        fdAfter.json(),
			"process_fd_delta":           fdDelta.json(),
		}
	}
	res.Iterations = n
	res.WallMs = soakMs(time.Since(t0))
	res.account(exp, recs)
	return res
}

// soakS09 runs CompleteLines with the old file's last line partial at the
// rename and completed on the old inode: right after the rename (S09a) or
// gap later, once the tail has moved on (S09b).
func soakS09(t *testing.T, n int, completeFirst bool, gap time.Duration) *soakResult {
	r := soakNew(t)
	soakWrite(t, r.path, "g0:a\npar0")
	r.start(Config{CompleteLines: true})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		for i := 1; i <= n && r.alive(); i++ {
			r.res.Iterations = i
			old := r.path + "." + strconv.Itoa(i)
			complete := func() { soakAppend(t, old, fmt.Sprintf("tial%d\n", i)) }
			soakRename(t, r.path, old)
			if completeFirst {
				complete()
			}
			soakWrite(t, r.path, fmt.Sprintf("g%d:a\npar%d", i, i))
			if !completeFirst {
				time.Sleep(gap)
				complete()
			}
			r.exp = append(r.exp, fmt.Sprintf("par%dtial%d", i-1, i), fmt.Sprintf("g%d:a", i))
			r.expect(fmt.Sprintf("g%d:a", i))
			time.Sleep(soakSettle)
		}
	}
	return r.finish()
}

// S10: MaxLineSize 10; an 18-byte line without newline is in the snapshot and
// its last seven bytes are appended to the old inode after the snapshot.
func soakS10(t *testing.T) *soakResult {
	r := soakNew(t)
	soakWrite(t, r.path, "g0:a\n")
	r.start(Config{MaxLineSize: 10})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		for i := 1; i <= soakCount(50) && r.alive(); i++ {
			r.res.Iterations = i
			part := fmt.Sprintf("s10:%03d:yy%08d", i, i) // 18 bytes
			old := r.path + "." + strconv.Itoa(i)
			g := r.c.armGate(func(l *Line) bool { return l.Text == part[:10] || l.Text == fmt.Sprintf("g%d:a", i) })
			soakAppend(t, r.path, part)
			soakRename(t, r.path, old)
			soakWrite(t, r.path, fmt.Sprintf("g%d:a\n", i))
			r.exp = append(r.exp, part[:10], part[10:], fmt.Sprintf("g%d:a", i))
			select {
			case <-g.hit:
			case <-r.c.dead:
				r.fail(soakClosed)
			case <-time.After(soakWait):
				r.res.Hangs++
			}
			soakAppend(t, old, "zzzzzzz\n")
			g.release()
			r.expect(fmt.Sprintf("g%d:a", i))
			time.Sleep(soakSettle)
		}
	}
	return r.finish()
}

// S11: a leaky bucket of two lines overflows on the third line, which is
// still delivered, and the tail then cools off for one second. Five lines are
// appended to the old file and three to the replacement; the expected lines
// are the three old ones, the three replacement ones and a probe written
// after the second cooloff.
func soakS11(t *testing.T) *soakResult {
	r := soakNew(t)
	soakWrite(t, r.path, "g0:a\n")
	r.start(Config{RateLimiter: ratelimiter.NewLeakyBucket(2, 100*time.Millisecond)})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		for i := 1; i <= soakCount(5) && r.alive(); i++ {
			r.res.Iterations = i
			p := fmt.Sprintf("s11:%d:", i)
			soakAppend(t, r.path, soakLines(p, "w1", "w2", "w3", "w4", "w5"))
			soakRename(t, r.path, r.path+"."+strconv.Itoa(i))
			soakWrite(t, r.path, soakLines(p, "b1", "b2", "b3"))
			r.expAdd(p, "w1", "w2", "w3", "b1", "b2", "b3", "probe")
			if !r.expect(p + "b2") {
				continue
			}
			// The third replacement line overflows the bucket: one notice
			// follows, then a cooloff of one second.
			idx, st := r.c.wait(r.cur, soakNoticeWait, func(l *soakLine) bool { return l.Notice })
			if st == soakFound {
				r.cur = idx + 1
			} else {
				r.cur = idx
				r.fail(st)
			}
			time.Sleep(1200 * time.Millisecond)
			soakAppend(t, r.path, p+"probe\n")
			r.expect(p + "probe")
			// 130 ms: the bucket is empty again, and the next round does not
			// start on a poll tick (a 250 ms pause let the tick fall between
			// the append and the rename, which master reports as a change).
			time.Sleep(130 * time.Millisecond)
		}
	}
	return r.finish()
}

// S12: Stop and StopAtEOF while the tail is blocked handing over the rate
// limit notice. soakHangBound declares a stop hung; the notice is then taken
// so that the tail can finish.
func soakS12(t *testing.T) *soakResult {
	res := soakNewResult()
	t0 := time.Now()
	n := soakCount(10)
	var exp []string
	var recs []soakLine
	for _, kind := range []string{"Stop", "StopAtEOF"} {
		var latUs soakStat
		hungN, blocked, errEOF := 0, 0, 0
		for i := 1; i <= n && res.Hangs < soakHangAbort; i++ {
			dir := t.TempDir()
			path := filepath.Join(dir, "log")
			p := fmt.Sprintf("s12:%s:%d:", kind, i)
			soakWrite(t, path, soakLines(p, "a", "b"))
			exp = append(exp, p+"a", p+"b")
			tail := soakStart(t, path, Config{RateLimiter: ratelimiter.NewLeakyBucket(1, time.Hour)}, soakNewLogger())
			c := soakConsume(tail.Lines)
			g := c.armGate(func(l *Line) bool { return l.Text == p+"b" })
			if !soakExpectLine(c, p+"b", soakWait) {
				res.Hangs++
				soakStop(tail)
				continue
			}
			// The second line overflows the bucket: the notice is next.
			if soakWaitBlocked([]string{"tail.(*Tail).tailFileSync"}, []string{"[select]", "[chan send]"},
				[]string{"tail.(*Tail).sendLine"}, time.Second) {
				blocked++
			}
			t1 := time.Now()
			done := make(chan error, 1)
			go func() {
				if kind == "Stop" {
					done <- tail.Stop()
					return
				}
				done <- tail.StopAtEOF()
			}()
			check := func(err error) {
				if kind == "StopAtEOF" && err == errStopAtEOF {
					errEOF++
				}
			}
			select {
			case err := <-done:
				check(err)
			case <-time.After(soakHangBound):
				hungN++
				res.Hangs++
				g.release()
				select {
				case err := <-done:
					check(err)
				case <-time.After(soakStopWait):
				}
			}
			latUs.add(float64(time.Since(t1).Nanoseconds()) / 1e3)
			g.release()
			soakWaitDead(c)
			recs = append(recs, c.snapshot()...)
		}
		res.Extra[kind] = map[string]interface{}{
			"n": n, "hung": hungN, "hang_bound_ms": soakMs(soakHangBound),
			"latency_us": latUs.json(), "blocked_in_notice_send": blocked, "err_stopateof": errEOF,
		}
	}
	res.Iterations = n
	res.WallMs = soakMs(time.Since(t0))
	res.account(exp, recs)
	return res
}

// S13: 300 rotations as fast as possible, no waiting in between.
func soakS13(t *testing.T) *soakResult {
	r := soakNew(t)
	n := soakCount(300)
	soakWrite(t, r.path, "g0:a\n")
	r.start(Config{})
	r.exp = []string{"g0:a"}
	if r.expect("g0:a") {
		t0 := time.Now()
		for i := 1; i <= n; i++ {
			soakAppend(t, r.path, fmt.Sprintf("r%d:a\n", i))
			soakRename(t, r.path, r.path+"."+strconv.Itoa(i))
			soakWrite(t, r.path, fmt.Sprintf("g%d:a\n", i))
			r.exp = append(r.exp, fmt.Sprintf("r%d:a", i), fmt.Sprintf("g%d:a", i))
		}
		r.res.Iterations = n
		r.res.Extra["burst_ms"] = soakMs(time.Since(t0))
	}
	return r.finish()
}

// ---------------------------------------------------------------- entry point

type soakScenario struct {
	id   string
	name string
	run  func(*testing.T) *soakResult
}

func soakScenarios() []soakScenario {
	return []soakScenario{
		{"S01", "300_isolated_rotations_rename_and_create_back_to_back", func(t *testing.T) *soakResult {
			return soakRotate(t, soakCount(300), nil, 0)
		}},
		{"S02", "300_isolated_rotations_replacement_created_300ms_after_rename", func(t *testing.T) *soakResult {
			return soakRotate(t, soakCount(300), nil, 300*time.Millisecond)
		}},
		{"S03", "300_isolated_rotations_random_0-300ms_pause_seed_1", func(t *testing.T) *soakResult {
			return soakRotate(t, soakCount(300), soakPause, 0)
		}},
		{"S04", "slow_consumer_rotated_again_during_drain", soakS04},
		{"S05", "replacement_deleted_during_drain_recreated_after_500ms", soakS05},
		{"S06", "deleted_with_unread_lines_recreated_after_500ms", soakS06},
		{"S07", "in_place_truncation", func(t *testing.T) *soakResult { return soakS07(t, true) }},
		{"S07f", "in_place_truncation_written_right_after_reopen", func(t *testing.T) *soakResult {
			return soakS07(t, false)
		}},
		{"S08", "stop_and_stopateof_during_blocked_drain", soakS08},
		{"S09a", "completelines_old_last_line_completed_right_after_rename", func(t *testing.T) *soakResult {
			return soakS09(t, soakCount(50), true, 0)
		}},
		{"S09b", "completelines_old_last_line_completed_600ms_after_rename", func(t *testing.T) *soakResult {
			return soakS09(t, soakCount(30), false, 600*time.Millisecond)
		}},
		{"S10", "maxlinesize_10_long_line_across_snapshot", soakS10},
		{"S11", "rate_limit_cooloff_during_drain", soakS11},
		{"S12", "stop_and_stopateof_during_blocked_rate_limit_notice", soakS12},
		{"S13", "300_back_to_back_rotations_no_wait", soakS13},
	}
}

func TestSoakMatrix(t *testing.T) {
	poll := 250 * time.Millisecond
	if v := os.Getenv("TAIL_SOAK_POLL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("TAIL_SOAK_POLL: %v", err)
		}
		poll = d
	}
	// TestMain in tail_test.go forces 5ms; the soak states its own value.
	watch.POLL_DURATION = poll

	out := os.Getenv("TAIL_SOAK_OUT")
	if out == "" {
		out = "soak-out"
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	only := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("TAIL_SOAK_ONLY"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			only[id] = true
		}
	}
	if v := os.Getenv("TAIL_SOAK_SCALE"); v != "" {
		if _, err := fmt.Sscanf(v, "%g", &soakScale); err != nil {
			t.Fatalf("TAIL_SOAK_SCALE: %v", err)
		}
	}
	seq := os.Getenv("TAIL_SOAK_SEQ") == "1"

	t.Logf("soak matrix: %s, POLL_DURATION=%v, scale=%g, out=%s", runtime.Version(), poll, soakScale, out)
	for _, sc := range soakScenarios() {
		sc := sc
		if len(only) > 0 && !only[sc.id] {
			continue
		}
		t.Run(sc.id+"_"+sc.name, func(t *testing.T) {
			if !seq {
				t.Parallel()
			}
			res := sc.run(t)
			res.Scenario, res.Name = sc.id, sc.name
			res.Version = os.Getenv("TAIL_SOAK_VERSION")
			res.GoVersion = runtime.Version()
			res.PollMs = soakMs(poll)
			b, err := json.MarshalIndent(res, "", " ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, sc.id+".json"), b, 0o644); err != nil {
				t.Fatal(err)
			}
			if os.Getenv("TAIL_SOAK_DUMP") != "" {
				var b strings.Builder
				for i, l := range res.Recs {
					text := l.Text
					if l.Notice {
						text = "NOTICE " + text
					}
					fmt.Fprintf(&b, "%d\t%s\t%s\n", i, l.At.Format("15:04:05.000000"), text)
				}
				if err := os.WriteFile(filepath.Join(out, sc.id+".lines.tsv"), []byte(b.String()), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%s", res.line())
		})
	}
}
