#!/usr/bin/env python3
"""Build the markdown tables of the soak matrix from the records that
soak_matrix_test.go writes (layout produced by run.sh):

    RESULTS/<version>/r<rep>/S*.json     one record per scenario and repetition

Usage: summarize.py RESULTS_DIR

Prints three tables: the matrix (one line per scenario and version), the
latency of the rotation scenarios, and every record on its own line.
"""
import glob
import json
import os
import sys

VERSIONS = [("master", "master"), ("pr", "PR head")]
ROWS = [
    ("S01", "1. 300 isolated rotations, 5 lines then rename+create back-to-back"),
    ("S02", "2. 300 isolated rotations, replacement created 300 ms after the rename"),
    ("S03", "3. 300 isolated rotations, random 0–300 ms pause between append and rename"),
    ("S04", "4. Slow consumer, rotated again during the drain (30 rounds)"),
    ("S05", "5. Replacement deleted during the drain, recreated 500 ms later (30 rounds)"),
    ("S06", "6. Deleted with unread lines, recreated 500 ms later (30 rounds)"),
    ("S07", "7. In-place truncation (50 rounds)"),
    ("S07f", "7f. In-place truncation, new lines written right after the reopen (50 rounds; supplementary)"),
    ("S08", "8. Stop and StopAtEOF during a blocked drain (30 + 30 stops)"),
    ("S09a", "9a. CompleteLines, old last line completed right after the rename (50 rounds)"),
    ("S09b", "9b. CompleteLines, old last line completed 600 ms after the rename (30 rounds)"),
    ("S10", "10. MaxLineSize 10, long line across the snapshot (50 rounds)"),
    ("S11", "11. Rate-limit cooloff during the drain (5 rounds)"),
    ("S12", "12. Stop and StopAtEOF during a blocked rate-limit notice (10 + 10 stops)"),
    ("S13", "13. 300 back-to-back rotations, no wait"),
]


def load(root):
    recs = {}
    for path in glob.glob(os.path.join(root, "*", "r*", "S*.json")):
        with open(path) as f:
            r = json.load(f)
        rep_dir = os.path.dirname(path)
        rep = int(os.path.basename(rep_dir)[1:])
        version = r.get("version") or os.path.basename(os.path.dirname(rep_dir))
        recs.setdefault(version, {}).setdefault(rep, {})[r["scenario"]] = r
    return recs


def span(vals, f=str):
    """One value when the repetitions agree once formatted, otherwise the
    smallest and the largest."""
    pairs = sorted(zip(vals, [f(v) for v in vals]))
    if pairs[0][1] == pairs[-1][1]:
        return pairs[0][1]
    return "%s–%s" % (pairs[0][1], pairs[-1][1])


def runs_of(recs, version, sid):
    return [recs[version][rep][sid] for rep in sorted(recs.get(version, {})) if sid in recs[version][rep]]


def us(x):
    return "%.0f µs" % x


def std_cell(runs):
    if not runs:
        return "not run"
    s = "%s/%s · dup %s · hangs %s" % (
        span([r["delivered"] for r in runs]), span([r["expected"] for r in runs]),
        span([r["duplicates"] for r in runs]), span([r["hangs"] for r in runs]))
    for key in ("misordered", "unexpected", "deaths"):
        vals = [r[key] for r in runs]
        if any(vals):
            s += " · %s %s" % (key, span(vals))
    if any(r["notices"] for r in runs):
        s += " · notices %s" % span([r["notices"] for r in runs])
    if any(r["aborted"] for r in runs):
        s += " · ABORTED after %s rounds" % span([r["iterations"] for r in runs])
    return s


def s08_cell(runs):
    if not runs:
        return "not run"
    out = []
    for kind in ("Stop", "StopAtEOF"):
        xs = [r["extra"][kind] for r in runs]
        s = "%s returned %s/%d" % (kind, span([x["n"] - x["hung"] for x in xs]), xs[0]["n"])
        if any(x["hung"] for x in xs):
            s += " (hung %s)" % span([x["hung"] for x in xs])
        s += ", max %s" % span([x["latency_us"]["max"] for x in xs], us)
        if kind == "StopAtEOF":
            s += ", errStopAtEOF %s/%d" % (span([x["err_stopateof"] for x in xs]), xs[0]["n"])
        out.append(s)
    every = [r["extra"][k] for r in runs for k in ("Stop", "StopAtEOF")]
    first = runs[0]["extra"]["Stop"]
    gate = "old file" if first["gate_line_from_old_file"] == first["n"] else "replacement"
    out.append("blocked on the %s's next line" % gate)
    out.append("file fds %s during the drain → %s after the stop" % (
        span([x["file_fds_during_drain"]["max"] for x in every], lambda v: "%d" % v),
        span([x["file_fds_after_stop"]["max"] for x in every], lambda v: "%d" % v)))
    out.append("process fds after the stop vs before the start: max %s" % span(
        [x["process_fd_delta"]["max"] for x in every], lambda v: "%+d" % v))
    if any(r["hangs"] for r in runs):
        out.append("hangs %s" % span([r["hangs"] for r in runs]))
    return " · ".join(out)


def s12_cell(runs):
    if not runs:
        return "not run"
    out = ["%s/%s lines" % (span([r["delivered"] for r in runs]), span([r["expected"] for r in runs]))]
    for kind in ("Stop", "StopAtEOF"):
        xs = [r["extra"][kind] for r in runs]
        s = "%s: hung %s/%d" % (kind, span([x["hung"] for x in xs]), xs[0]["n"])
        clean = [x for x in xs if x["hung"] == 0]
        if clean:
            s += ", otherwise max %s" % span([x["latency_us"]["max"] for x in clean], us)
        if kind == "StopAtEOF":
            s += " (errStopAtEOF %s/%d)" % (span([x["err_stopateof"] for x in xs]), xs[0]["n"])
        out.append(s)
    out.append("hang bound %d s" % round(runs[0]["extra"]["Stop"]["hang_bound_ms"] / 1000))
    out.append("dup %s" % span([r["duplicates"] for r in runs]))
    return " · ".join(out)


def cell(recs, version, sid):
    runs = runs_of(recs, version, sid)
    if sid == "S08":
        return s08_cell(runs)
    if sid == "S12":
        return s12_cell(runs)
    return std_cell(runs)


def matrix(recs):
    lines = ["| Scenario | master | PR head |", "|---|---|---|"]
    for sid, title in ROWS:
        lines.append("| **%s** | %s | %s |" % (title, cell(recs, "master", sid), cell(recs, "pr", sid)))
    return "\n".join(lines)


def latency(recs):
    lines = [
        "Polling latency in ms, measured from the rename: per repetition the max and the p90 over "
        "the rotations, here the min–max of those over the repetitions (one value when they agree).",
        "",
        "| Scenario | Version | old lines drained after the rename | rename → last old line: max / p90 | "
        "rename → first line of the replacement: max / p90 | old lines delivered before the rename |",
        "|---|---|---|---|---|---|",
    ]
    ms = lambda v: "%.1f" % v
    for sid in ("S01", "S02", "S03"):
        for v, label in VERSIONS:
            runs = runs_of(recs, v, sid)
            if not runs:
                lines.append("| %s | %s | not run | | | |" % (sid, label))
                continue
            old = [r["extra"]["rename_to_last_old_line_ms"] for r in runs]
            new = [r["extra"]["rename_to_first_replacement_line_ms"] for r in runs]
            if all(o["n"] == 0 for o in old):
                nold, last = "0", "n/a: old lines not delivered"
            else:
                nold = span([o["n"] for o in old])
                last = "%s / %s" % (span([o["max"] for o in old], ms), span([o["p90"] for o in old], ms))
            first = "%s / %s" % (span([o["max"] for o in new], ms), span([o["p90"] for o in new], ms))
            before = span([r["extra"]["rotations_old_lines_delivered_before_rename"] for r in runs])
            lines.append("| %s | %s | %s | %s | %s | %s |" % (sid, label, nold, last, first, before))
    return "\n".join(lines)


def detail(recs):
    lines = ["| Scenario | Version | Rep | Delivered/expected | Missing | Dup | Misordered | Unexpected | Notices | Hangs | Deaths | Rounds | Wall s |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for sid, _ in ROWS:
        for v, label in VERSIONS:
            for rep in sorted(recs.get(v, {})):
                r = recs[v][rep].get(sid)
                if r is None:
                    lines.append("| %s | %s | %d | not run | | | | | | | | | |" % (sid, label, rep))
                    continue
                lines.append("| %s | %s | %d | %d/%d | %d | %d | %d | %d | %d | %d | %d | %d | %.1f |" % (
                    sid, label, rep, r["delivered"], r["expected"], r["missing"], r["duplicates"], r["misordered"],
                    r["unexpected"], r["notices"], r["hangs"], r["deaths"], r["iterations"], r["wall_ms"] / 1000))
    return "\n".join(lines)


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    recs = load(sys.argv[1])
    counts = ", ".join("%s %d" % (v, len(recs.get(v, {}))) for v, _ in VERSIONS)
    print("Repetitions per version: %s." % counts)
    print()
    print(matrix(recs))
    print()
    print(latency(recs))
    print()
    print(detail(recs))


if __name__ == "__main__":
    main()
