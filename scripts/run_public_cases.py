#!/usr/bin/env python3
"""Run the public mysqltest-style cases against a MiniVectorDB binary.

Each case gets a fresh, empty data directory and its own process, matching
mysqltest/README.md. Statements are sent one per line on stdin; one JSON
response line is read per statement (the --format json output of this
implementation). Assertions follow the README: integers, column names and
strings exactly; floats with abs and rel tolerance 1e-5; "unordered" rows as
multisets; {} means "must succeed".

Usage: scripts/run_public_cases.py [--bin PATH] [--cases DIR] [case ...]
"""
import argparse
import json
import math
import os
import shutil
import subprocess
import sys
import tempfile
from collections import Counter

TOL = 1e-5


def same_value(exp, got):
    if isinstance(exp, bool) or isinstance(got, bool):
        return exp == got
    if isinstance(exp, float) or isinstance(got, float):
        if not isinstance(exp, (int, float)) or not isinstance(got, (int, float)):
            return False
        return math.isclose(exp, got, rel_tol=TOL, abs_tol=TOL)
    if isinstance(exp, int):
        return isinstance(got, int) and exp == got
    return exp == got


def same_row(exp, got):
    return len(exp) == len(got) and all(same_value(a, b) for a, b in zip(exp, got))


def norm_row(row):
    # Hashable key for multiset comparison; floats rounded to tolerance scale.
    out = []
    for v in row:
        if isinstance(v, float):
            out.append(("f", round(v, 4)))
        else:
            out.append((type(v).__name__, v))
    return tuple(out)


def check(exp, got):
    """Return None if got satisfies exp, else a reason string."""
    if "error" in got:
        return f"statement failed: {got['error']}"
    if "affected_rows" in exp and got.get("affected_rows") != exp["affected_rows"]:
        return f"affected_rows {got.get('affected_rows')} != {exp['affected_rows']}"
    if "columns" in exp and got.get("columns") != exp["columns"]:
        return f"columns {got.get('columns')} != {exp['columns']}"
    if "rows" in exp:
        rows = got.get("rows")
        if rows is None:
            return "no rows returned"
        if len(rows) != len(exp["rows"]):
            return f"row count {len(rows)} != {len(exp['rows'])}"
        if exp.get("unordered"):
            if Counter(map(norm_row, rows)) != Counter(map(norm_row, exp["rows"])):
                return f"unordered rows differ: {rows} vs {exp['rows']}"
        else:
            for i, (e, g) in enumerate(zip(exp["rows"], rows)):
                if not same_row(e, g):
                    return f"row {i}: {g} != {e}"
    return None


def run_case(binary, case_dir, name):
    with open(os.path.join(case_dir, name + ".test"), encoding="utf-8") as f:
        stmts = [l.rstrip("\n") for l in f if l.strip()]
    with open(os.path.join(case_dir, name + ".result"), encoding="utf-8") as f:
        expected = json.load(f)
    assert len(stmts) == len(expected), f"{name}: test/result length mismatch"
    data = tempfile.mkdtemp(prefix=f"mvdb-{name}-")
    try:
        p = subprocess.Popen([binary, "--data-dir", data], stdin=subprocess.PIPE,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, encoding="utf-8", bufsize=1)
        failures = []
        for i, (sql, exp) in enumerate(zip(stmts, expected)):
            p.stdin.write(sql + "\n")
            p.stdin.flush()
            line = p.stdout.readline()
            if not line:
                failures.append(f"stmt {i+1}: no response (process exited)")
                break
            got = json.loads(line)
            why = check(exp, got)
            if why:
                failures.append(f"stmt {i+1} `{sql}`: {why}")
        p.stdin.close()
        rc = p.wait(timeout=30)
        if rc != 0:
            failures.append(f"exit code {rc}: {p.stderr.read()}")
        return failures
    finally:
        shutil.rmtree(data, ignore_errors=True)


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", default=os.path.join(root, "minivectordb"))
    ap.add_argument("--cases", default=os.path.join(root, "mysqltest"))
    ap.add_argument("names", nargs="*")
    a = ap.parse_args()
    with open(os.path.join(a.cases, "manifest.json"), encoding="utf-8") as f:
        manifest = json.load(f)
    names = a.names or [c["id"] for c in manifest["cases"]]
    bad = 0
    for n in names:
        fails = run_case(a.bin, os.path.join(a.cases, "cases"), n)
        print(("PASS " if not fails else "FAIL ") + n)
        for x in fails:
            print("   ", x)
        bad += bool(fails)
    print(f"{len(names) - bad}/{len(names)} cases passed")
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
