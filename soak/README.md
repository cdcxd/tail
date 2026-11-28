# Soak matrix for nxadm/tail PR #1

PR #1, "fix(polling): drain rotated files before reopening", against `master`.
Polling watcher only: Podman tails its events log with `Poll: true`
(`libpod/events/logfile.go`), the PR is scoped to the polling watcher, and the
inotify watcher is out of scope.

The recorded run in `results/` was made against PR head `d542824`. The commits
after it change CI, the Go version and vendoring, two unrelated tests, the
inotify tracker and comments only, so the numbers apply to the fix commit
`0728355` as well.

| File | What it is |
|---|---|
| `soak_matrix_test.go` | the harness, package `tail`. A `soak` build tag keeps it out of normal builds; `run.sh` copies it into the package directory of two private copies, where it builds unchanged against both |
| `run.sh` | builds private copies of both revisions and runs the matrix |
| `summarize.py` | turns the records into the tables |
| `results.md` | the numbers of the recorded run, with the method and the caveats |
| `results/` | records of the recorded run: `env.txt`, `matrix.md`, `<version>/r<N>/S*.json`, the delivered lines `<id>.lines.tsv`, and `logs/` with the raw `go test -v` output |

## Reproduce

In a checkout of this branch:

```sh
cd soak
./run.sh 5 2            # 5 repetitions per version, 2 at a time; about 8 minutes
cat out/matrix.md       # the three tables
python3 summarize.py out   # the same tables from the records
```

`run.sh` needs `git`, `bash`, `python3`, `/usr/bin/time` and Go 1.24.6
(`GOTOOLCHAIN=go1.24.6`, downloaded on demand). It compares `HEAD` with
`master` by default (`PR_HEAD`, `BASE`) and writes to `out/` (`OUT`); it
refuses to write into a non-empty directory. Nothing else in the repository is
written: both copies come from `git archive` and live in a `mktemp -d`
directory that is removed at exit.

What `run.sh` does, step by step, if you would rather run it by hand:

```sh
cd soak
REPO=$(cd .. && pwd)
MAIN=S01,S02,S03,S04,S05,S06,S07,S07f,S09a,S09b,S10,S11,S13
STOPS=S08,S12
OUT=$(mktemp -d) && W=$(mktemp -d) && mkdir $W/pr $W/master
export GOTOOLCHAIN=go1.24.6 GOFLAGS= TAIL_SOAK_POLL=250ms TAIL_SOAK_DUMP=1
git -C $REPO archive HEAD | tar -x -C $W/pr
git -C $REPO archive master | tar -x -C $W/master
(cd $W/pr && go mod tidy && go mod vendor)   # a no-op on a consistent vendor tree
cp soak_matrix_test.go $W/pr/ && cp soak_matrix_test.go $W/master/

# main: all scenarios but S08 and S12, concurrently
(cd $W/pr && TAIL_SOAK_ONLY=$MAIN TAIL_SOAK_VERSION=pr TAIL_SOAK_OUT=$OUT/pr/r1 \
   go test -tags soak -run TestSoakMatrix -count=1 -v -parallel 16 -timeout 30m .)
(cd $W/master && TAIL_SOAK_ONLY=$MAIN TAIL_SOAK_VERSION=master TAIL_SOAK_OUT=$OUT/master/r1 \
   go test -mod=vendor -tags soak -run TestSoakMatrix -count=1 -v -parallel 16 -timeout 30m .)

# stops: S08 and S12 one after another, so that process-wide fd counts are exact
(cd $W/pr && TAIL_SOAK_ONLY=$STOPS TAIL_SOAK_SEQ=1 TAIL_SOAK_VERSION=pr TAIL_SOAK_OUT=$OUT/pr/r1 \
   go test -tags soak -run TestSoakMatrix -count=1 -v -parallel 1 -timeout 30m .)
(cd $W/master && TAIL_SOAK_ONLY=$STOPS TAIL_SOAK_SEQ=1 TAIL_SOAK_VERSION=master TAIL_SOAK_OUT=$OUT/master/r1 \
   go test -mod=vendor -tags soak -run TestSoakMatrix -count=1 -v -parallel 1 -timeout 30m .)
python3 summarize.py $OUT
```

Variations:

```sh
SCALE=0.05 OUT=/tmp/soak-trial ./run.sh 1 1    # about 10 seconds, 5 % of the rounds
POLL=50ms OUT=/tmp/soak-poll50 ./run.sh 3 1    # another poll interval
# one scenario in a copy that already has soak_matrix_test.go:
(cd $W/pr && TAIL_SOAK_ONLY=S05 go test -tags soak -run TestSoakMatrix/S05 -count=1 -v .)
```

## The two copies

- **master (`ba755e4`)**: `go.mod` says `go 1.13`, so vendor mode is not
  automatic: pass `-mod=vendor`.
- **PR head**: `go.mod` says `go 1.18` with a consistent vendor tree. Earlier
  heads of the PR (`d542824`, the recorded run) lacked the `## explicit`
  markers and the indirect `golang.org/x/sys` requirement, so every go command
  failed with "inconsistent vendoring"; `run.sh` runs `go mod tidy && go mod
  vendor` in the private copy for them. No source file changes.

## Method

`soak_matrix_test.go` sets `watch.POLL_DURATION` itself (`TAIL_SOAK_POLL`,
default 250 ms, the library default and what Podman gets); the `TestMain` in
`tail_test.go` forces 5 ms, which the soak does not use. Every scenario is a
subtest with its own directory under `/tmp` (tmpfs) and its own tail created
with `TailFile(path, Config{Follow: true, ReOpen: true, Poll: true, ...})`.
Each subtest writes `S*.json` with its counts and, with `TAIL_SOAK_DUMP`, the
delivered lines with arrival times.

- **Lines.** Every line written in a scenario has a unique text. *Delivered*
  counts the expected lines that arrived at least once, *missing* the others,
  *duplicates* further copies of an expected line, *unexpected* lines that
  were never written for the scenario, *misordered* expected lines that
  arrived out of turn. The rate-limit notice is counted separately
  (*notices*).
- **Hangs.** A wait for an expected line that takes more than 3 s, a
  `Stop`/`StopAtEOF` that does not return within 5 s, and (S12) a stop that
  does not return within 2 s. A scenario gives up after 40 hangs.
- **Settle.** Two rounds of a scenario are 50 ms apart. Without the pause the
  next rotation starts within microseconds of the last delivery, which races
  the tail's own bookkeeping after a delivery and is not how rotations happen.
- **Latency** (S01–S03), per rotation from the rename: to the last of the
  five pre-rename lines and to the first line of the replacement. Rotations
  whose five lines all arrived before the rename (the poll fell into the pause,
  S03) are counted separately and left out of the "last old line" samples.
- **S03 pauses**: an LCG seeded with 1 (`x = x*6364136223846793005 +
  1442695040888963407`), pause = `(x >> 33) mod 301` ms, so the sequence does
  not depend on the Go version.
- **S08, S12**: the consumer takes one line of the rotation and stops reading,
  which leaves the tail blocked in `sendLine` (S08) or on the rate-limit
  notice (S12). A goroutine dump confirms the blocked state before the stop is
  called. File descriptors are counted through `/proc/self/fd`: those that
  refer to files of the scenario's directory (exact), and the process total
  before the tail started minus after the stop (only meaningful because S08
  and S12 run alone in their process).

Two rows are supplementary: **S07f** writes the new lines right after the
tail logged its truncation reopen, without the 50 ms settle (see
`results.md`); **S09b** completes the old file's last line 600 ms after the
rename, once the tail has moved on.
