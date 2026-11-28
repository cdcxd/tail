# Soak results: PR #1 (d542824) against master (ba755e4), polling watcher

One complete run of `run.sh 5 2`, 2026-11-27 15:26–15:34 (local), wall 457 s.
Five repetitions per version; every repetition and version is one "main"
`go test` process (all scenarios but S08 and S12, concurrently) and one
"stops" process (S08 and S12, one after another). All 20 processes passed.
The method is in `README.md`.

## Environment (`results/env.txt`)

- Repository `/home/cdong/Code/tail`, read with `git archive` only: base
  `ba755e4`, PR head `d542824`.
- `go version go1.24.6 linux/amd64`; Linux 7.0.0-44-generic x86_64; 24 CPUs;
  `/tmp` is tmpfs.
- `watch.POLL_DURATION` = 250 ms (set by the harness; `TestMain` in
  `tail_test.go` would force 5 ms).
- PR copy: `go mod tidy && go mod vendor` applied in the private copy (adds the
  indirect `golang.org/x/sys` requirement and the `## explicit` markers);
  master copy built with `-mod=vendor`. No source change in either.
- `soak_matrix_test.go` sha256
  `ebf0fb53474d488fb31a6bc602c42ec5b75fa7fb80f8f3b3c8fb44b783c3c663`.
- CPU per test process: main 152–153 s wall, 0.5–2.1 s user, 0.4–0.8 s sys;
  stops 15–56 s wall, at most 2.0 s user. The machine was shared with other
  work (load average 1.0–12.3 during the run); the soak itself used under
  2 % of a core per process.
- A first attempt of this run was killed after 134 s by a process-wide
  `kill -QUIT` from another agent's session (four `tail.test` processes of
  this run were hit; exit code 2 with a goroutine dump, not a library crash).
  The run above is the rerun with the same harness.
- The branch moved while the soak ran: its head is now `af6d817` (vendoring
  fixed in-tree, CI and README edits). `d542824` plus `go mod tidy && go mod
  vendor` is identical to `af6d817` in `go.mod`, `vendor/modules.txt`, the
  vendor tree and every source file but one comment line in `tail.go`
  (`diff -r` of both archives, tidy and vendor applied to the first), so the
  numbers apply to `af6d817` as well. The working tree also has uncommitted
  edits to `tail_test.go` and `watch/inotify_tracker.go` made by someone
  else; neither is part of this run.

## Matrix

Repetitions per version: master 5, pr 5.

| Scenario | master | PR head |
|---|---|---|
| **1. 300 isolated rotations, 5 lines then rename+create back-to-back** | 301/1801 · dup 0 · hangs 0 | 1801/1801 · dup 0 · hangs 0 |
| **2. 300 isolated rotations, replacement created 300 ms after the rename** | 301/1801 · dup 0 · hangs 0 | 1801/1801 · dup 0 · hangs 0 |
| **3. 300 isolated rotations, random 0–300 ms pause between append and rename** | 836–841/1801 · dup 0 · hangs 0 | 1801/1801 · dup 0 · hangs 0 |
| **4. Slow consumer, rotated again during the drain (30 rounds)** | 61/211 · dup 0 · hangs 30 | 211/211 · dup 0 · hangs 0 |
| **5. Replacement deleted during the drain, recreated 500 ms later (30 rounds)** | 61/211 · dup 0 · hangs 30 | 211/211 · dup 0 · hangs 0 |
| **6. Deleted with unread lines, recreated 500 ms later (30 rounds)** | 61/151 · dup 0 · hangs 0 | 151/151 · dup 0 · hangs 0 |
| **7. In-place truncation (50 rounds)** | 101/101 · dup 0 · hangs 0 | 101/101 · dup 0 · hangs 0 |
| **7f. In-place truncation, new lines written right after the reopen (50 rounds; supplementary)** | 97–101/101 · dup 0 · hangs 0–4 | 91–99/101 · dup 0 · hangs 2–10 |
| **8. Stop and StopAtEOF during a blocked drain (30 + 30 stops)** | Stop returned 30/30, max 54 µs–158 µs · StopAtEOF returned 30/30, max 36 µs–264 µs, errStopAtEOF 30/30 · blocked on the replacement's next line · file fds 1 during the drain → 0 after the stop · process fds after the stop vs before the start: max +0 | Stop returned 30/30, max 32 µs–102 µs · StopAtEOF returned 30/30, max 24 µs–87 µs, errStopAtEOF 30/30 · blocked on the old file's next line · file fds 2 during the drain → 0 after the stop · process fds after the stop vs before the start: max +0 |
| **9a. CompleteLines, old last line completed right after the rename (50 rounds)** | 51/101 · dup 0 · hangs 0 | 101/101 · dup 0 · hangs 0 |
| **9b. CompleteLines, old last line completed 600 ms after the rename (30 rounds)** | 31/61 · dup 0 · hangs 0 | 31/61 · dup 0 · hangs 0 |
| **10. MaxLineSize 10, long line across the snapshot (50 rounds)** | 51/151 · dup 0 · hangs 0 | 151/151 · dup 0 · hangs 0 |
| **11. Rate-limit cooloff during the drain (5 rounds)** | 21/36 · dup 0 · hangs 0 · notices 5 | 36/36 · dup 0 · hangs 0 · notices 10 |
| **12. Stop and StopAtEOF during a blocked rate-limit notice (10 + 10 stops)** | 40/40 lines · Stop: hung 10/10 · StopAtEOF: hung 10/10 (errStopAtEOF 10/10) · hang bound 2 s · dup 0 | 40/40 lines · Stop: hung 0/10, otherwise max 14 µs–27 µs · StopAtEOF: hung 0/10, otherwise max 13 µs–120 µs (errStopAtEOF 10/10) · hang bound 2 s · dup 0 |
| **13. 300 back-to-back rotations, no wait** | 2/601 · dup 0 · hangs 0 | 3/601 · dup 0 · hangs 0 |

Runs agree in every row except 3 (master 836 or 841: one rotation more or
less caught by a poll), 7f (random race, see below) and the stop latencies of
8 and 12 (microseconds). No duplicates, no misordered lines, no unexpected
lines, no dead tail in any of the 150 records. "Hangs" in row 4 and 5
(master) are the harness's 3 s bound on a wait for lines that never come.

## Latency (polling, from the rename)

Per repetition the max and the p90 over the rotations; the cell gives the
min–max of those over the five repetitions (one value when they agree). ms.

| Scenario | Version | old lines drained after the rename | rename → last old line: max / p90 | rename → first line of the replacement: max / p90 | old lines delivered before the rename |
|---|---|---|---|---|---|
| S01 | master | 0 | n/a: old lines not delivered | 250.0–251.1 / 200.7–200.8 | 0 |
| S01 | PR head | 300 | 250.8–251.2 / 200.7–200.8 | 250.8–251.2 / 200.7–200.8 | 0 |
| S02 | master | 0 | n/a: old lines not delivered | 500.1–501.3 / 451.6–451.7 | 0 |
| S02 | PR head | 300 | 250.5–250.8 / 200.6–200.7 | 501.1–501.4 / 451.5–451.6 | 0 |
| S03 | master | 1–3 | 0.0–0.1 / 0.0–0.1 | 250.9–251.7 / 217.7–220.2 | 105–106 |
| S03 | PR head | 194–195 | 195.6–196.2 / 177.6–178.7 | 250.7–251.1 / 217.6–218.8 | 105–106 |

- **rename → last old line**: when the five lines written before the rename
  have all reached the consumer. On the PR this is the poll that notices the
  move (uniformly 0–250 ms after the rename) plus the drain, which takes less
  than a millisecond: max 251 ms, p90 201 ms. On master the lines are not
  delivered at all.
- **rename → first line of the replacement**: when the tail follows the new
  file again. Row 1: the same poll, the switch follows the drain at once
  (identical numbers on both versions). Row 2: 300 ms creation delay, then
  the first `os.Stat` of `BlockUntilExists` that finds the file (it runs every
  250 ms): p90 451 ms, max 501 ms, identical on both versions. Row 3: the
  rotations whose old lines came before the rename (the poll fell into the
  pause) are excluded from the "last old line" samples; the 105–106 of them
  are the same rotations on both versions.

## What each row shows

Line numbers are `tail.go` and `watch/polling.go` of the PR head unless
marked master.

**1, 2.** Master reopens as soon as the poll reports the move
(master `tail.go:393-398`) and never reads the five lines: 301 of 1801 in
every repetition. The PR bounds the reader by the old file's size
(`tail.go:438`), reads the five lines, then switches (`tail.go:401-407`,
`468-478`): 1801 of 1801, 1500 rotations, no duplicate, no reordering.

**3.** Seed 1. Master only sees a rotation's lines when a poll tick falls
between the append and the rename: 105–106 rotations of 300, 836–841 lines.
The PR delivers all 1801; 194–195 rotations were drained after the rename.

**4.** While master is blocked handing over a line of the second file, the
file is renamed again. Its next watcher starts with `os.Stat(path)`
(master `watch/polling.go:46-47`, master `tail.go:375-380`), which already
returns the third file, so the move of the open file is never reported: c1
and c2 never arrive in 30 of 30 rounds, and the tail only recovers at the
next rotation. The PR compares the path with the open file
(`watch/polling.go:55-61`, `:100`) and follows both moves: 211 of 211.

**5.** Same mechanism on master (the recreated file's lines never arrive, 30
of 30 rounds). The PR reads the replacement's two lines from the descriptor
it opened at the first move, although the file was unlinked meanwhile, then
picks up the recreated file: 211 of 211.

**6.** Both deliver the recreated file. Master loses the three unread lines
(61 of 151); the PR drains them from the unlinked inode (151 of 151).

**7.** Unchanged: 101 of 101 on both.

**7f (supplementary).** A blind spot of the poller that predates the PR.
After it reports a truncation the poller's last seen size is zero
(`watch/polling.go:107-112`, unchanged). When the tail reads the new lines
right after its reopen, because it won the race against the writer, no new
poller starts; a truncation that follows before the poller has seen the
refilled size is not reported, and the lines written after it are read from
the old offset and lost (or arrive as a fragment). This run: master 8 lines
in 4 of 250 rounds, PR 24 lines in 12 of 250. Isolated rerun of 4 × 50 rounds
per version, one process each (`results/supplementary/S07f-isolated/`):
master 9 of 200 rounds, PR 5 of 200. With 50 ms between the reopen and the
write (row 7) it does not occur.

**8.** The consumer takes the first line of the rotation and stops reading,
which leaves the tail blocked in `sendLine`; a goroutine dump confirmed the
blocked state before each of the 300 stops per version. Both versions: 300
of 300 stops return within 24–264 µs, and no descriptor is left. The PR holds
two descriptors during the blocked drain (old file and replacement,
`tail.go:442-444`), master one; after the stop both hold none, and the
process-wide count is back to its starting value (`tail.go:202-208`).
`StopAtEOF` returns `errStopAtEOF` in 150 of 150 calls on both. The stop
lands on different files: on the PR the tail is blocked on the old file's
second line and had not reopened (no "Re-opening" logged in 150 of 150); on
master it had already reopened and is blocked on the replacement's second
line. The 120 of 360 lines are the first line plus the one the consumer took;
the lines the tail was holding are discarded because nobody reads them (the
`Dying` case of `sendLine`, `tail.go:505-536`, same on master).

**9a.** The completion is appended to the old inode right after the rename,
before the PR takes its snapshot: the joined line is delivered, 101 of 101.
Master drops it: 51 of 101.

**9b.** The completion comes 600 ms after the rename, after both versions
moved on: 31 of 61 on both. The PR drops the buffered partial line when it
reopens (`tail.go:219-221`).

**10.** The 18-byte line without newline is in the snapshot and arrives as a
10-byte and an 8-byte line (MaxLineSize 10), then the replacement's line: 151
of 151. The seven bytes appended after the snapshot are not delivered (the
bound at `tail.go:438`, as in `TestReOpenIgnoresAppendsAfterDrainStarts`).
Master: 51 of 151.

**11.** Leaky bucket of two lines. Per round the PR delivers w1, w2, w3,
notice, one second, b1, b2, b3, notice, one second, probe: the cooloff
switches to the replacement (`tail.go:339-341`) and skips w4, w5. 36 of 36,
10 notices. Master delivers b1, b2, b3, notice, probe: 21 of 36, 5 notices.

**12.** The tail is blocked handing over the rate-limit notice (confirmed by
the goroutine dump before each of the 200 stops per version). Master's send
has no `Dying` case (master `tail.go:317`): 100 of 100 stops hang, each
bounded at 2 s and then freed by taking the notice. The PR: 100 of 100 return
(`tail.go:328-332`), the notice is never delivered.

**13.** 300 rotations in 10–15 ms. master delivers `g0:a` and `g300:a`;
the PR additionally `r1:a`, the line appended to the file it had open when
the burst started (its snapshot includes it): 2 of 601 and 3 of 601,
identical in all five repetitions. One poll can see one state; this limit is
the same on both.

## Caveats of the harness

- Two rounds of a scenario are 50 ms apart. Without the pause the next
  rotation starts within microseconds of the last delivery; master's watcher
  then starts with a stat of the path that can already return the next file
  (the mechanism of row 4), which is not how rotations happen.
- Row 11 pauses 130 ms after the probe. With 250 ms (the poll period) the
  tick fell between the append and the rename in 2 or 3 of 5 rounds, and
  master read the old lines as a modification; a first full run with that
  pause gave master 24–30 of 36 there.
- Row 11's "expected" lines are the PR's behaviour (the three old lines until
  the bucket overflows, three replacement lines, probe). The line that
  overflows the bucket is delivered before the cooloff on both versions.
- Rows 4–6, 9a, 10 use a consumer that stops reading after the first line of
  a round, so that the next step of the round happens while the tail is in
  the drain. On master that first line is the replacement's, on the PR the
  old file's.
- "Hang" is a 3 s bound on one wait. In rows 4 and 5 (master) the tail is
  not stuck in the library's sense: it polls, but watches the wrong file.
- Scenarios other than 8 and 12 run concurrently in one process; the
  per-process CPU is small (above), and all latency figures are within the
  poll interval.

## Every record

| Scenario | Version | Rep | Delivered/expected | Missing | Dup | Misordered | Unexpected | Notices | Hangs | Deaths | Rounds | Wall s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| S01 | master | 1 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | master | 2 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | master | 3 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | master | 4 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | master | 5 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | PR head | 1 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | PR head | 2 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | PR head | 3 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | PR head | 4 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S01 | PR head | 5 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 76.8 |
| S02 | master | 1 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 152.0 |
| S02 | master | 2 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 152.0 |
| S02 | master | 3 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 152.0 |
| S02 | master | 4 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 152.0 |
| S02 | master | 5 | 301/1801 | 1500 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 152.0 |
| S02 | PR head | 1 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 151.9 |
| S02 | PR head | 2 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 151.9 |
| S02 | PR head | 3 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 151.9 |
| S02 | PR head | 4 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 151.9 |
| S02 | PR head | 5 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 151.9 |
| S03 | master | 1 | 836/1801 | 965 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.9 |
| S03 | master | 2 | 836/1801 | 965 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.6 |
| S03 | master | 3 | 841/1801 | 960 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.9 |
| S03 | master | 4 | 836/1801 | 965 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.6 |
| S03 | master | 5 | 841/1801 | 960 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.9 |
| S03 | PR head | 1 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.8 |
| S03 | PR head | 2 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.8 |
| S03 | PR head | 3 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.8 |
| S03 | PR head | 4 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.6 |
| S03 | PR head | 5 | 1801/1801 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 103.8 |
| S04 | master | 1 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 108.4 |
| S04 | master | 2 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 108.3 |
| S04 | master | 3 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 108.4 |
| S04 | master | 4 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 108.4 |
| S04 | master | 5 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 108.3 |
| S04 | PR head | 1 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 25.6 |
| S04 | PR head | 2 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 25.6 |
| S04 | PR head | 3 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 25.6 |
| S04 | PR head | 4 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 25.6 |
| S04 | PR head | 5 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 25.6 |
| S05 | master | 1 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 114.4 |
| S05 | master | 2 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 114.4 |
| S05 | master | 3 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 114.4 |
| S05 | master | 4 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 114.4 |
| S05 | master | 5 | 61/211 | 150 | 0 | 0 | 0 | 0 | 30 | 0 | 30 | 114.3 |
| S05 | PR head | 1 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 31.6 |
| S05 | PR head | 2 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 31.6 |
| S05 | PR head | 3 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 31.6 |
| S05 | PR head | 4 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 31.6 |
| S05 | PR head | 5 | 211/211 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 31.6 |
| S06 | master | 1 | 61/151 | 90 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 23.9 |
| S06 | master | 2 | 61/151 | 90 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 23.9 |
| S06 | master | 3 | 61/151 | 90 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 23.9 |
| S06 | master | 4 | 61/151 | 90 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 24.1 |
| S06 | master | 5 | 61/151 | 90 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 23.9 |
| S06 | PR head | 1 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 23.9 |
| S06 | PR head | 2 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 23.9 |
| S06 | PR head | 3 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 23.9 |
| S06 | PR head | 4 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 24.1 |
| S06 | PR head | 5 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 23.9 |
| S07 | master | 1 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | master | 2 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | master | 3 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | master | 4 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | master | 5 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | PR head | 1 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | PR head | 2 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | PR head | 3 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | PR head | 4 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07 | PR head | 5 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07f | master | 1 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07f | master | 2 | 99/101 | 2 | 0 | 0 | 0 | 0 | 2 | 0 | 50 | 31.9 |
| S07f | master | 3 | 99/101 | 2 | 0 | 0 | 0 | 0 | 2 | 0 | 50 | 31.9 |
| S07f | master | 4 | 97/101 | 4 | 0 | 0 | 0 | 0 | 4 | 0 | 50 | 37.1 |
| S07f | master | 5 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 26.6 |
| S07f | PR head | 1 | 97/101 | 4 | 0 | 0 | 0 | 0 | 4 | 0 | 50 | 37.1 |
| S07f | PR head | 2 | 95/101 | 6 | 0 | 0 | 0 | 0 | 6 | 0 | 50 | 42.4 |
| S07f | PR head | 3 | 99/101 | 2 | 0 | 0 | 0 | 0 | 2 | 0 | 50 | 31.9 |
| S07f | PR head | 4 | 99/101 | 2 | 0 | 0 | 0 | 0 | 2 | 0 | 50 | 31.9 |
| S07f | PR head | 5 | 91/101 | 10 | 0 | 0 | 0 | 0 | 10 | 0 | 50 | 52.9 |
| S08 | master | 1 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | master | 2 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | master | 3 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | master | 4 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | master | 5 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | PR head | 1 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | PR head | 2 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | PR head | 3 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | PR head | 4 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 15.1 |
| S08 | PR head | 5 | 120/360 | 240 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 14.9 |
| S09a | master | 1 | 51/101 | 50 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | master | 2 | 51/101 | 50 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | master | 3 | 51/101 | 50 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | master | 4 | 51/101 | 50 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | master | 5 | 51/101 | 50 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | PR head | 1 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | PR head | 2 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | PR head | 3 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | PR head | 4 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09a | PR head | 5 | 101/101 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S09b | master | 1 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | master | 2 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | master | 3 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | master | 4 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | master | 5 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | PR head | 1 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | PR head | 2 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | PR head | 3 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | PR head | 4 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S09b | PR head | 5 | 31/61 | 30 | 0 | 0 | 0 | 0 | 0 | 0 | 30 | 21.0 |
| S10 | master | 1 | 51/151 | 100 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | master | 2 | 51/151 | 100 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | master | 3 | 51/151 | 100 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | master | 4 | 51/151 | 100 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | master | 5 | 51/151 | 100 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | PR head | 1 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | PR head | 2 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | PR head | 3 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | PR head | 4 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S10 | PR head | 5 | 151/151 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 50 | 14.1 |
| S11 | master | 1 | 21/36 | 15 | 0 | 0 | 0 | 5 | 0 | 0 | 5 | 9.1 |
| S11 | master | 2 | 21/36 | 15 | 0 | 0 | 0 | 5 | 0 | 0 | 5 | 9.1 |
| S11 | master | 3 | 21/36 | 15 | 0 | 0 | 0 | 5 | 0 | 0 | 5 | 9.1 |
| S11 | master | 4 | 21/36 | 15 | 0 | 0 | 0 | 5 | 0 | 0 | 5 | 9.1 |
| S11 | master | 5 | 21/36 | 15 | 0 | 0 | 0 | 5 | 0 | 0 | 5 | 9.1 |
| S11 | PR head | 1 | 36/36 | 0 | 0 | 0 | 0 | 10 | 0 | 0 | 5 | 14.1 |
| S11 | PR head | 2 | 36/36 | 0 | 0 | 0 | 0 | 10 | 0 | 0 | 5 | 14.1 |
| S11 | PR head | 3 | 36/36 | 0 | 0 | 0 | 0 | 10 | 0 | 0 | 5 | 14.1 |
| S11 | PR head | 4 | 36/36 | 0 | 0 | 0 | 0 | 10 | 0 | 0 | 5 | 14.1 |
| S11 | PR head | 5 | 36/36 | 0 | 0 | 0 | 0 | 10 | 0 | 0 | 5 | 14.1 |
| S12 | master | 1 | 40/40 | 0 | 0 | 0 | 0 | 20 | 20 | 0 | 10 | 40.0 |
| S12 | master | 2 | 40/40 | 0 | 0 | 0 | 0 | 20 | 20 | 0 | 10 | 40.0 |
| S12 | master | 3 | 40/40 | 0 | 0 | 0 | 0 | 20 | 20 | 0 | 10 | 40.0 |
| S12 | master | 4 | 40/40 | 0 | 0 | 0 | 0 | 20 | 20 | 0 | 10 | 40.0 |
| S12 | master | 5 | 40/40 | 0 | 0 | 0 | 0 | 20 | 20 | 0 | 10 | 40.0 |
| S12 | PR head | 1 | 40/40 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 10 | 0.0 |
| S12 | PR head | 2 | 40/40 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 10 | 0.0 |
| S12 | PR head | 3 | 40/40 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 10 | 0.0 |
| S12 | PR head | 4 | 40/40 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 10 | 0.0 |
| S12 | PR head | 5 | 40/40 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 10 | 0.0 |
| S13 | master | 1 | 2/601 | 599 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | master | 2 | 2/601 | 599 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | master | 3 | 2/601 | 599 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | master | 4 | 2/601 | 599 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | master | 5 | 2/601 | 599 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | PR head | 1 | 3/601 | 598 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | PR head | 2 | 3/601 | 598 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | PR head | 3 | 3/601 | 598 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | PR head | 4 | 3/601 | 598 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |
| S13 | PR head | 5 | 3/601 | 598 | 0 | 0 | 0 | 0 | 0 | 0 | 300 | 1.8 |

The 20 notices of S12 (master) are the ones the harness took to free each
hung stop. Wall times are per scenario inside a process; the whole run took
457 s.

## Files

- `results/env.txt`: the environment above, load averages per round.
- `results/matrix.md`: the three tables as generated by `summarize.py`.
- `results/<version>/r<1–5>/S*.json`: one record per scenario and repetition
  (counts, latency statistics, stop latencies, descriptor counts, the first
  missing lines).
- `results/<version>/r<1–5>/S*.lines.tsv`: every delivered line with its
  arrival time (notices prefixed with `NOTICE`).
- `results/logs/<version>-r<N>-{main,stops}.log`: the raw `go test -v` output;
  `*.time`: wall, user and sys of each process.
- `results/supplementary/S07f-isolated/`: records of the isolated rerun of
  row 7f.
