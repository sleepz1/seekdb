#!/usr/bin/env python3
"""Portable timing of the two phases for CI (no cgroups, no root).

Same workload as perf_harness.py: 10,000 x 128 rows in 100-row INSERTs,
restart, 1,000 top-10 queries (half filtered). Prints wall time measured by
the client and the CPU time of the server process, so two
machines or two builds can be compared. Usage: ci_bench.py BINARY [mode]
"""
import json, os, random, shutil, subprocess, sys, tempfile, time

ROWS, DIM, QUERIES, BATCH, CATS = 10000, 128, 1000, 100, 10


class DB:
    def __init__(self, binary, data):
        self.p = subprocess.Popen([binary, "-data", data], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.DEVNULL)
        self.n = 0
        assert json.loads(self.p.stdout.readline()) == {"type": "ready"}

    def sql(self, s, mode="exact"):
        self.n += 1
        self.p.stdin.write(json.dumps({"id": self.n, "op": "execute", "sql": s, "search_mode": mode}).encode() + b"\n")
        self.p.stdin.flush()
        r = json.loads(self.p.stdout.readline())
        if not r.get("ok"):
            raise RuntimeError(f"{s[:80]} -> {r}")
        return r

    def shutdown(self):
        self.n += 1
        self.p.stdin.write(json.dumps({"id": self.n, "op": "shutdown"}).encode() + b"\n")
        self.p.stdin.flush()
        self.p.stdout.readline()
        _, _, ru = os.wait4(self.p.pid, 0)
        return (ru.ru_utime + ru.ru_stime) * 1000


def main():
    binary = sys.argv[1]
    mode = sys.argv[2] if len(sys.argv) > 2 else "exact"
    rng = random.Random(1)

    def vec():
        return "[" + ",".join(f"{rng.uniform(-1, 1):.6f}" for _ in range(DIM)) + "]"

    batches = [[f"({i},'c{i % CATS}','doc {i}','{vec()}')" for i in range(b, min(b + BATCH, ROWS))]
               for b in range(0, ROWS, BATCH)]
    queries = []
    for k in range(QUERIES + 20):
        where = f" WHERE category = 'c{rng.randrange(CATS)}'" if k % 2 else ""
        queries.append(f"SELECT id, L2_DISTANCE(embedding, '{vec()}') AS distance FROM docs{where} ORDER BY distance LIMIT 10")
    data = tempfile.mkdtemp(prefix="mvdb-ci-")
    try:
        db = DB(binary, data)
        db.sql(f"CREATE TABLE docs (id BIGINT PRIMARY KEY, category VARCHAR(16), content VARCHAR(64), embedding VECTOR({DIM}))")
        t0 = time.perf_counter()
        for rows in batches:
            db.sql("INSERT INTO docs VALUES " + ",".join(rows))
        tw = (time.perf_counter() - t0) * 1000
        cpu = db.shutdown()
        print(f"write: wall {tw:8.1f} ms  server cpu {cpu:8.1f} ms")
        db = DB(binary, data)
        db.sql("CREATE VECTOR INDEX idx_emb ON docs(embedding)")
        for q in queries[:20]:
            db.sql(q, mode)
        t0 = time.perf_counter()
        for q in queries[20:]:
            db.sql(q, mode)
        tq = (time.perf_counter() - t0) * 1000
        cpu = db.shutdown()
        print(f"query: wall {tq:8.1f} ms  server cpu {cpu:8.1f} ms  ({mode}; cpu includes the cold start)")
    finally:
        shutil.rmtree(data, ignore_errors=True)


if __name__ == "__main__":
    main()
