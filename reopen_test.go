package tail

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nxadm/tail/ratelimiter"
	"github.com/nxadm/tail/watch"
	"gopkg.in/tomb.v1"
)

// manualWatcher hands the first FileChanges to the test, so the test decides
// when the tail sees the first move. Later calls go to a polling watcher.
type manualWatcher struct {
	first chan *watch.FileChanges
	used  bool
	poll  *watch.PollingFileWatcher
}

func (w *manualWatcher) BlockUntilExists(t *tomb.Tomb) error { return w.poll.BlockUntilExists(t) }

func (w *manualWatcher) ChangeEvents(*tomb.Tomb, int64) (*watch.FileChanges, error) {
	return nil, errors.New("not used: the tail calls ChangeEventsFile")
}

func (w *manualWatcher) ChangeEventsFile(t *tomb.Tomb, f *os.File, pos int64) (*watch.FileChanges, error) {
	if w.used {
		if f == nil {
			return w.poll.ChangeEvents(t, pos)
		}
		return w.poll.ChangeEventsFile(t, f, pos)
	}
	w.used = true
	c := watch.NewFileChanges()
	w.first <- c
	return c, nil
}

// pathWatcher is a manualWatcher that watches the path, as the inotify
// watcher does.
type pathWatcher struct{ w *manualWatcher }

func (p pathWatcher) BlockUntilExists(t *tomb.Tomb) error { return p.w.BlockUntilExists(t) }

func (p pathWatcher) ChangeEvents(t *tomb.Tomb, pos int64) (*watch.FileChanges, error) {
	return p.w.ChangeEventsFile(t, nil, pos)
}

// startTail tails path, which contains the line "a", and returns once the
// tail read "a" and waits for the first change. opts change the tail before
// it starts.
func startTail(t *testing.T, path string, followsFile bool, opts ...func(*Tail)) (tail *Tail, changes *watch.FileChanges, expect func(string)) {
	t.Helper()
	writeFile(t, path, "a\n")
	first := make(chan *watch.FileChanges, 1)
	var w watch.FileWatcher = &manualWatcher{first: first, poll: watch.NewPollingFileWatcher(path)}
	if !followsFile {
		w = pathWatcher{w.(*manualWatcher)}
	}
	// As TailFile does, but with the test's watcher.
	tail = &Tail{
		Filename: path,
		Lines:    make(chan *Line),
		Config:   Config{Follow: true, ReOpen: true, Logger: DiscardingLogger},
		watcher:  w,
	}
	for _, opt := range opts {
		opt(tail)
	}
	go tail.tailFileSync()
	t.Cleanup(func() { tail.Stop() })
	expect = func(want string) {
		t.Helper()
		select {
		case l, ok := <-tail.Lines:
			if !ok {
				t.Fatalf("tail stopped waiting for %q: %v", want, tail.Err())
			}
			if l.Text != want {
				t.Fatalf("got line %q, want %q", l.Text, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	expect("a")
	return tail, <-first, expect
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// rotate moves path to path+suffix and creates a new file with s at path.
func rotate(t *testing.T, path, suffix, s string) {
	t.Helper()
	if err := os.Rename(path, path+suffix); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, s)
}

// Unread lines from the old file are delivered before lines from the replacement.
func TestReOpenReadsRestOfMovedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.txt")
	_, changes, expect := startTail(t, path, true)
	appendFile(t, path, "b\n")
	rotate(t, path, ".1", "c\n")
	changes.NotifyDeleted() // what both watchers report for the move
	expect("b")
	expect("c")
}

// Rotate again during the first drain and check that both replacements are read.
func TestReOpenFollowsSecondRotationDuringDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.txt")
	_, changes, expect := startTail(t, path, true)
	appendFile(t, path, "w1\nw2\n")
	rotate(t, path, ".1", "b1\n")
	changes.NotifyDeleted()
	// The tail reads w1 only after it handled the move.
	expect("w1")
	rotate(t, path, ".2", "c1\n")
	expect("w2")
	expect("b1")
	expect("c1")
}

func TestReOpenIgnoresAppendsAfterDrainStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.txt")
	_, changes, expect := startTail(t, path, true)
	appendFile(t, path, "w1\nw2\n")
	rotate(t, path, ".1", "b1\n")
	changes.NotifyDeleted()
	expect("w1")
	// Append after tail has captured the old file's size.
	appendFile(t, path+".1", "late\n")
	expect("w2")
	expect("b1")
}

// Watchers that track only the path switch files immediately.
func TestReOpenPathWatcherSwitchesImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.txt")
	_, changes, expect := startTail(t, path, false)
	appendFile(t, path, "b\n")
	rotate(t, path, ".1", "c\n")
	changes.NotifyDeleted()
	expect("c")
}

// Stop returns while the tail waits to send the rate limit notice.
func TestStopDuringRateLimitNotice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.txt")
	writeFile(t, path, "a\nb\n")
	tail, err := TailFile(path, Config{Follow: true, Poll: true, Logger: DiscardingLogger,
		RateLimiter: ratelimiter.NewLeakyBucket(1, time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	<-tail.Lines
	<-tail.Lines // over the limit: the notice comes next, but is not read
	done := make(chan struct{})
	go func() {
		tail.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}
}

// A stat error while polling stops the tail; it is returned from Wait.
func TestPollingStatErrorStopsTail(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory that cannot be searched")
	}
	dir := filepath.Join(t.TempDir(), "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test.txt")
	writeFile(t, path, "a\n")
	tail, err := TailFile(path, Config{Follow: true, ReOpen: true, Poll: true, Logger: DiscardingLogger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tail.Stop() })
	<-tail.Lines
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	select {
	case <-tail.Dead():
	case <-time.After(10 * time.Second):
		t.Fatal("tail still running")
	}
	if err := tail.Err(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("got error %v, want a permission error", err)
	}
}

// StopAtEOF during the drain stops at the end of the old file.
func TestReOpenStopAtEOFDuringDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.txt")
	var logs bytes.Buffer
	tail, changes, expect := startTail(t, path, true, func(tail *Tail) {
		tail.Logger = log.New(&logs, "", 0)
	})
	appendFile(t, path, "w1\nw2\n")
	rotate(t, path, ".1", "b1\n")
	changes.NotifyDeleted()
	expect("w1")
	// The tail holds w2.
	if err := tail.StopAtEOF(); err != errStopAtEOF {
		t.Fatalf("got error %v, want %v", err, errStopAtEOF)
	}
	if strings.Contains(logs.String(), "Re-opening") {
		t.Fatalf("reopened after StopAtEOF:\n%s", logs.String())
	}
}

// After a delete, the unread lines are delivered, then the file created later.
func TestReOpenAfterDeleteWithUnreadLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a deleted file stays at its path while open")
	}
	path := filepath.Join(t.TempDir(), "test.txt")
	_, changes, expect := startTail(t, path, true)
	appendFile(t, path, "w1\n")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	changes.NotifyDeleted()
	expect("w1")
	writeFile(t, path, "c\n")
	expect("c")
}
