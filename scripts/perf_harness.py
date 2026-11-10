#!/usr/bin/env python3
"""Local approximation of the platform performance benchmark (task 3.3).

Runs the two scored phases against the real binary over the official JSONL
protocol, each in its own memory cgroup (v1), and reports what the
platform measures:

  T  wall time of the timed part (write: all batches; query: the 1000
     queries, after warm-up)
  B  device read+write bytes (/proc/diskstats delta of the data device),
     from process start to shutdown + sync
  M  peak memory of the cgroup incl. page cache (memory.max_usage_in_bytes)

The write phase starts from an empty data dir and commits 100-row batches.
The query phase restarts on the same dir after dropping the page cache
(cold start), creates the vector index, warms up, then times the queries.

Needs root and cgroup v1 memory/blkio hierarchies (the dev sandbox has them).
The platform's absolute numbers differ; use this for before/after deltas.
"""
import argparse
import json
import os
import random
import shutil
import subprocess
import sys
import tempfile
import time

MEMCG = "/sys/fs/cgroup/memory"
BLKCG = "/sys/fs/cgroup/blkio"


def own_cg(root):
    for line in open("/proc/self/cgroup"):
        _, ctrls, path = line.strip().split(":", 2)
        if os.path.basename(root) in ctrls.split(","):
            return os.path.join(root, path.lstrip("/"))
    return root


def diskstats(dev="vda"):
    for line in open("/proc/diskstats"):
        p = line.split()
        if p[2] == dev:
            return int(p[5]) * 512, int(p[9]) * 512
    raise RuntimeError("device not found")


class Cgroup:
    def __init__(self, name):
        self.mem = os.path.join(own_cg(MEMCG), name)
        self.blk = os.path.join(own_cg(BLKCG), name)
        for d in (self.mem, self.blk):
            if os.path.isdir(d):
                os.rmdir(d)
            os.mkdir(d)
        os.sync()
        self.d0 = diskstats()

    def enter(self):  # runs in the child before exec
        pid = str(os.getpid())
        for d in (self.mem, self.blk):
            with open(os.path.join(d, "tasks"), "w") as f:
                f.write(pid)

    def peak(self):
        return int(open(os.path.join(self.mem, "memory.max_usage_in_bytes")).read())

    def io(self):
        """Device bytes (read, write) since the cgroup was created. blkio
        accounting is not populated in the sandbox, so this uses the
        system-wide /proc/diskstats delta of the data device (the sandbox is
        otherwise idle); it includes filesystem journal writes."""
        rd, wr = diskstats()
        return rd - self.d0[0], wr - self.d0[1]

    def close(self):
        for d in (self.mem, self.blk):
            try:
                os.rmdir(d)
            except OSError:
                pass


class DB:
    def __init__(self, binary, data, cg, env=None):
        self.p = subprocess.Popen([binary, "-data", data], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.DEVNULL, text=True, encoding="utf-8", bufsize=1,
                                  preexec_fn=cg.enter, env=env)
        self.n = 0
        assert json.loads(self.p.stdout.readline()) == {"type": "ready"}

    def req(self, obj):
        self.n += 1
        obj["id"] = self.n
        self.p.stdin.write(json.dumps(obj, ensure_ascii=False) + "\n")
        self.p.stdin.flush()
        r = json.loads(self.p.stdout.readline())
        if not r.get("ok"):
            raise RuntimeError(f"{str(obj)[:100]} -> {r}")
        return r

    def sql(self, s, mode="exact"):
        return self.req({"op": "execute", "sql": s, "search_mode": mode})

    def shutdown(self):
        self.req({"op": "shutdown"})
        self.p.wait(timeout=60)


def drop_caches():
    os.sync()
    try:
        with open("/proc/sys/vm/drop_caches", "w") as f:
            f.write("3")
    except OSError as e:
        print("warning: cannot drop caches:", e, file=sys.stderr)


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap = argparse.ArgumentParser()
    ap.add_argument("--bin", default=os.path.join(root, "minivectordb"))
    ap.add_argument("--rows", type=int, default=10000)
    ap.add_argument("--dim", type=int, default=128)
    ap.add_argument("--queries", type=int, default=1000)
    ap.add_argument("--batch", type=int, default=100)
    ap.add_argument("--cats", type=int, default=10)
    ap.add_argument("--style", choices=["multirow", "txn"], default="multirow")
    ap.add_argument("--mode", default="exact")
    ap.add_argument("--no-index", action="store_true")
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args()
    rng = random.Random(a.seed)

    def vec():
        return "[" + ",".join(f"{rng.uniform(-1, 1):.6f}" for _ in range(a.dim)) + "]"

    data = tempfile.mkdtemp(prefix="mvdb-perf-", dir="/var/tmp")
    batches = []
    for b in range(0, a.rows, a.batch):
        batches.append([f"({i},'c{i % a.cats}','doc {i}','{vec()}')" for i in range(b, min(b + a.batch, a.rows))])
    queries = []
    for k in range(a.queries + 20):
        where = f" WHERE category = 'c{rng.randrange(a.cats)}'" if k % 2 else ""
        queries.append(f"SELECT id, L2_DISTANCE(embedding, '{vec()}') AS distance FROM docs{where} ORDER BY distance LIMIT 10")
    res = {}
    try:
        # ---- write phase
        drop_caches()
        cg = Cgroup("mvdb_w")
        db = DB(a.bin, data, cg)
        db.sql(f"CREATE TABLE docs (id BIGINT PRIMARY KEY, category VARCHAR(16), content VARCHAR(64), embedding VECTOR({a.dim}))")
        t0 = time.perf_counter()
        for rows in batches:
            if a.style == "multirow":
                db.sql("INSERT INTO docs VALUES " + ",".join(rows))
            else:
                db.sql("BEGIN")
                for r in rows:
                    db.sql("INSERT INTO docs VALUES " + r)
                db.sql("COMMIT")
        tw = time.perf_counter() - t0
        db.shutdown()
        os.sync()
        rd, wr = cg.io()
        res["write"] = {"T_ms": tw * 1000, "B_read": rd, "B_write": wr, "M": cg.peak()}
        cg.close()
        # ---- query phase (cold start on the same data dir)
        drop_caches()
        cg = Cgroup("mvdb_q")
        t_start = time.perf_counter()
        db = DB(a.bin, data, cg)
        t_ready = time.perf_counter() - t_start
        if not a.no_index:
            db.sql("CREATE VECTOR INDEX idx_emb ON docs(embedding)")
        for q in queries[:20]:
            db.sql(q, a.mode)
        t0 = time.perf_counter()
        for q in queries[20:]:
            db.sql(q, a.mode)
        tq = time.perf_counter() - t0
        db.shutdown()
        os.sync()
        rd, wr = cg.io()
        res["query"] = {"T_ms": tq * 1000, "ready_ms": t_ready * 1000, "B_read": rd, "B_write": wr, "M": cg.peak()}
        cg.close()
        res["data_bytes"] = sum(os.path.getsize(os.path.join(data, f)) for f in os.listdir(data))
    finally:
        shutil.rmtree(data, ignore_errors=True)
    if a.json:
        print(json.dumps(res))
        return
    mib = 1 << 20
    for ph in ("write", "query"):
        r = res[ph]
        print(f"{ph:5}: T={r['T_ms']:8.1f} ms  B={(r['B_read'] + r['B_write']) / mib:7.2f} MiB "
              f"(r {r['B_read'] / mib:.2f} / w {r['B_write'] / mib:.2f})  M={r['M'] / mib:6.1f} MiB"
              + (f"  ready={r['ready_ms']:.0f} ms" if "ready_ms" in r else ""))
    print(f"data dir: {res['data_bytes'] / mib:.2f} MiB")


if __name__ == "__main__":
    main()
