#!/usr/bin/env bash
# Soak matrix for nxadm/tail PR #1: the PR head against master, on private
# copies. The repository is only read, through `git archive`; nothing is
# written to it.
#
#   ./run.sh [reps] [par]    reps = repetitions per version (default 5)
#                            par  = repetitions run at the same time (default 2)
#
# Environment: REPO (default: the repository containing this directory),
# PR_HEAD (default HEAD), BASE (default master), GO (default go1.24.6), OUT
# (default <this directory>/out; results/ holds the recorded run), SCALE
# (default 1, see soak_matrix_test.go), POLL (default 250ms).
#
# Per repetition and version two `go test` processes run: "main" runs the
# scenarios concurrently (all but S08 and S12), "stops" runs S08 and S12 one
# after another so that their process-wide descriptor counts are exact.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=${REPO:-$(cd "$HERE/.." && pwd)}
PR_HEAD=${PR_HEAD:-HEAD}
BASE=${BASE:-master}
GO=${GO:-go1.24.6}
OUT=${OUT:-$HERE/out}
REPS=${1:-5}
PAR=${2:-2}
export GOTOOLCHAIN=$GO GOFLAGS=
export TAIL_SOAK_POLL=${POLL:-250ms}
export TAIL_SOAK_SCALE=${SCALE:-1}
export TAIL_SOAK_DUMP=1   # also writes the delivered lines, <id>.lines.tsv

if [ -e "$OUT" ] && [ -n "$(ls -A "$OUT")" ]; then
  echo "$OUT is not empty: remove it, or point OUT elsewhere" >&2
  exit 2
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/tail-soak-work.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT/logs" "$WORK/pr" "$WORK/master"

git -C "$REPO" archive "$PR_HEAD" | tar -x -C "$WORK/pr"
git -C "$REPO" archive "$BASE" | tar -x -C "$WORK/master"
# Earlier heads of this PR bumped go.mod to 1.18 without a consistent vendor
# tree, which makes every go command fail. Regenerating is a no-op on a
# consistent one.
(cd "$WORK/pr" && go mod tidy && go mod vendor)
cp "$HERE/soak_matrix_test.go" "$WORK/pr/"
cp "$HERE/soak_matrix_test.go" "$WORK/master/"

{
  echo "date: $(date -Is)"
  echo "repository: $REPO (read with git archive only)"
  echo "base: $(git -C "$REPO" rev-parse --short "$BASE") ($BASE)   pr head: $(git -C "$REPO" rev-parse --short "$PR_HEAD") ($PR_HEAD)"
  echo "go: $(go version)"
  echo "kernel: $(uname -srm)"
  echo "cpus: $(nproc)"
  echo "/tmp: $(df -T /tmp | awk 'NR==2{print $2}')"
  echo "poll: $TAIL_SOAK_POLL   scale: $TAIL_SOAK_SCALE   repetitions: $REPS   concurrent repetitions: $PAR"
  echo "soak_matrix_test.go sha256: $(sha256sum "$HERE/soak_matrix_test.go" | cut -d' ' -f1)"
  echo "pr copy after 'go mod tidy && go mod vendor', go.mod:"
  sed 's/^/    /' "$WORK/pr/go.mod"
} > "$OUT/env.txt"

MAIN=S01,S02,S03,S04,S05,S06,S07,S07f,S09a,S09b,S10,S11,S13
STOPS=S08,S12
start=$(date +%s)
echo "load average before: $(cut -d' ' -f1-3 /proc/loadavg)" >> "$OUT/env.txt"
for ((first = 1; first <= REPS; first += PAR)); do
  pids=()
  names=()
  for ((rep = first; rep <= REPS && rep < first + PAR; rep++)); do
    for v in master pr; do
      mod=""
      [ "$v" = master ] && mod="-mod=vendor"   # go.mod says 1.13 there: no automatic vendoring
      mkdir -p "$OUT/$v/r$rep"
      (
        cd "$WORK/$v"
        TAIL_SOAK_ONLY=$MAIN TAIL_SOAK_VERSION=$v TAIL_SOAK_OUT="$OUT/$v/r$rep" \
          /usr/bin/time -f "wall %e s, user %U s, sys %S s" -o "$OUT/logs/$v-r$rep-main.time" \
          go test $mod -tags soak -run TestSoakMatrix -count=1 -v -parallel 16 -timeout 30m . \
          > "$OUT/logs/$v-r$rep-main.log" 2>&1
      ) &
      pids+=($!)
      names+=("$v-r$rep-main")
      (
        cd "$WORK/$v"
        TAIL_SOAK_ONLY=$STOPS TAIL_SOAK_SEQ=1 TAIL_SOAK_VERSION=$v TAIL_SOAK_OUT="$OUT/$v/r$rep" \
          /usr/bin/time -f "wall %e s, user %U s, sys %S s" -o "$OUT/logs/$v-r$rep-stops.time" \
          go test $mod -tags soak -run TestSoakMatrix -count=1 -v -parallel 1 -timeout 30m . \
          > "$OUT/logs/$v-r$rep-stops.log" 2>&1
      ) &
      pids+=($!)
      names+=("$v-r$rep-stops")
    done
  done
  failed=0
  for i in "${!pids[@]}"; do
    if ! wait "${pids[$i]}"; then
      echo "go test ${names[$i]} failed, see $OUT/logs/${names[$i]}.log" >&2
      failed=1
    fi
  done
  if [ "$failed" = 1 ]; then
    exit 1
  fi
  echo "repetitions $first to $((rep - 1)) done at $(( $(date +%s) - start )) s"
  echo "load average after repetitions $first to $((rep - 1)): $(cut -d' ' -f1-3 /proc/loadavg)" >> "$OUT/env.txt"
done

wall=$(( $(date +%s) - start ))
{
  echo "load average after: $(cut -d' ' -f1-3 /proc/loadavg)"
  echo "wall seconds: $wall"
} >> "$OUT/env.txt"
python3 "$HERE/summarize.py" "$OUT" > "$OUT/matrix.md"
echo "wall time: $wall s; records in $OUT, tables in $OUT/matrix.md"
