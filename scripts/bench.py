#!/usr/bin/env python3
"""Local approximation of the contest benchmark.

Default workload from the task statement: 10,000 rows, 128-dim vectors,
batches of 100 rows committed per batch, then 1,000 TopK queries (K=10),
half with an equality filter on an ordinary column. Reports wall time per
phase, peak RSS of the process and bytes written under the data directory.
(The platform measures the whole container cgroup incl. page cache; this is
only a relative indicator.)
"""
import argparse
import os
import random
import shutil
import subprocess
import tempfile
import time


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", default=os.path.join(root, "minivectordb"))
    ap.add_argument("--rows", type=int, default=10000)
    ap.add_argument("--dim", type=int, default=128)
    ap.add_argument("--queries", type=int, default=1000)
    ap.add_argument("--batch", type=int, default=100)
    ap.add_argument("--style", choices=["multirow", "txn"], default="multirow")
    ap.add_argument("--mode", default="exact")
    a = ap.parse_args()
    rng = random.Random(1)
    vec = lambda: "[" + ",".join(f"{rng.uniform(-1, 1):.6f}" for _ in range(a.dim)) + "]"
    data = tempfile.mkdtemp(prefix="mvdb-bench-")
    p = subprocess.Popen([a.bin, "--data-dir", data, "--mode", a.mode], stdin=subprocess.PIPE,
                         stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)

    def ask(sql):
        p.stdin.write(sql + "\n")
        p.stdin.flush()
        line = p.stdout.readline()
        assert line and '"error"' not in line, (sql[:80], line)
        return line

    ask(f"CREATE TABLE items (id BIGINT PRIMARY KEY, cat VARCHAR(16), content VARCHAR(64), embedding VECTOR({a.dim}))")
    batches = []
    for b in range(0, a.rows, a.batch):
        rows = [f"({i},'c{i % 10}','doc {i}','{vec()}')" for i in range(b, min(b + a.batch, a.rows))]
        batches.append(rows)
    t0 = time.perf_counter()
    for rows in batches:
        if a.style == "multirow":
            ask("INSERT INTO items VALUES " + ",".join(rows))
        else:
            ask("BEGIN")
            for r in rows:
                ask("INSERT INTO items VALUES " + r)
            ask("COMMIT")
    t_write = time.perf_counter() - t0
    ask("CREATE VECTOR INDEX idx ON items(embedding)")
    qs = []
    for k in range(a.queries):
        where = f" WHERE cat = 'c{k % 10}'" if k % 2 else ""
        qs.append(f"SELECT id, L2_DISTANCE(embedding, '{vec()}') AS distance FROM items{where} ORDER BY distance LIMIT 10")
    for q in qs[:20]:  # warm-up, not timed
        ask(q)
    t0 = time.perf_counter()
    for q in qs:
        ask(q)
    t_query = time.perf_counter() - t0
    with open(f"/proc/{p.pid}/status") as f:
        hwm = next(l for l in f if l.startswith("VmHWM")).split()[1]
    p.stdin.close()
    p.wait()
    size = sum(os.path.getsize(os.path.join(data, f)) for f in os.listdir(data))
    shutil.rmtree(data, ignore_errors=True)
    print(f"style={a.style} mode={a.mode} rows={a.rows} dim={a.dim}")
    print(f"write: {t_write*1000:.0f} ms total, {t_write*1000/len(batches):.2f} ms/batch")
    print(f"query: {t_query*1000:.0f} ms total, {t_query*1000/len(qs):.3f} ms/query")
    print(f"peak RSS: {int(hwm)/1024:.1f} MiB, data dir: {size/1024/1024:.2f} MiB")


if __name__ == "__main__":
    main()
