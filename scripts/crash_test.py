#!/usr/bin/env python3
"""SIGKILL recovery test against the real binary.

Repeatedly: start the database on one data directory, run a stream of
autocommit inserts and multi-statement transactions, and SIGKILL the process
at a random moment (often while a COMMIT is in flight). After each restart
verify that:
  * every acknowledged commit is present,
  * every transaction is all-or-nothing (in-flight ones may go either way),
  * never-committed transactions are absent.
Also kills the process again right after restart (during/just after
recovery) to exercise repeated recovery.
"""
import argparse
import json
import os
import random
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time


def start(binary, data):
    return subprocess.Popen([binary, "--data-dir", data], stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            text=True, bufsize=1)


def ask(p, sql):
    p.stdin.write(sql + "\n")
    p.stdin.flush()
    line = p.stdout.readline()
    if not line:
        raise EOFError
    return json.loads(line)


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", default=os.path.join(root, "minivectordb"))
    ap.add_argument("--rounds", type=int, default=30)
    a = ap.parse_args()
    rng = random.Random(7)
    data = tempfile.mkdtemp(prefix="mvdb-crash-")
    try:
        p = start(a.bin, data)
        ask(p, "CREATE TABLE t (id BIGINT PRIMARY KEY, grp BIGINT, v VECTOR(4))")
        p.stdin.close(); p.wait()
        acked = {}        # id -> grp of acknowledged rows
        maybe = {}        # grp -> ids whose commit was in flight at kill time
        next_id, next_grp = 1, 1
        for rnd in range(a.rounds):
            p = start(a.bin, data)
            killer = threading.Timer(rng.uniform(0.005, 0.08), lambda: p.send_signal(signal.SIGKILL))
            killer.start()
            try:
                while True:
                    grp = next_grp; next_grp += 1
                    n = rng.randint(1, 5)
                    ids = list(range(next_id, next_id + n)); next_id += n
                    vals = lambda i: f"({i},{grp},'[{i},{grp},0.5,-1]')"
                    if rng.random() < 0.5:
                        maybe[grp] = ids
                        r = ask(p, "INSERT INTO t VALUES " + ",".join(vals(i) for i in ids))
                    else:
                        r = ask(p, "BEGIN")
                        for i in ids:
                            r = ask(p, "INSERT INTO t VALUES " + vals(i))
                        maybe[grp] = ids
                        r = ask(p, "COMMIT")
                    assert "error" not in r, r
                    for i in ids:
                        acked[i] = grp
                    del maybe[grp]
            except (EOFError, BrokenPipeError, ValueError):
                pass
            killer.cancel()
            p.wait()
            for f in (p.stdin, p.stdout):
                try:
                    f.close()
                except (BrokenPipeError, OSError):
                    pass
            # restart; sometimes kill again immediately to interrupt recovery
            if rng.random() < 0.3:
                q = start(a.bin, data)
                time.sleep(rng.uniform(0, 0.01))
                q.send_signal(signal.SIGKILL); q.wait()
                for f in (q.stdin, q.stdout):
                    try:
                        f.close()
                    except (BrokenPipeError, OSError):
                        pass
            q = start(a.bin, data)
            rows = ask(q, "SELECT id, grp FROM t")["rows"]
            q.stdin.close(); q.wait()
            got = {r[0]: r[1] for r in rows}
            missing = [i for i in acked if i not in got]
            assert not missing, f"round {rnd}: acknowledged rows lost: {missing[:10]}"
            by_grp = {}
            for i, g in got.items():
                by_grp.setdefault(g, set()).add(i)
            for g, ids in by_grp.items():
                expect = {i for i, gg in acked.items() if gg == g} or set(maybe.get(g, []))
                assert ids == expect, f"round {rnd}: group {g} partially applied: {sorted(ids)} vs {sorted(expect)}"
            # in-flight groups that did survive become acknowledged history
            for g, ids in list(maybe.items()):
                if g in by_grp:
                    for i in ids:
                        acked[i] = g
            maybe.clear()
            # rows that were never committed must not exist
            assert set(got) == set(acked), f"round {rnd}: unexpected rows {sorted(set(got) - set(acked))[:10]}"
        print(f"PASS crash_test: {a.rounds} SIGKILL rounds, {len(acked)} committed rows verified")
    finally:
        shutil.rmtree(data, ignore_errors=True)


if __name__ == "__main__":
    try:
        main()
    except AssertionError as e:
        print("FAIL crash_test:", e)
        sys.exit(1)
