#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
pid=''
cleanup() { if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi; rm -rf "$tmp"; }
trap cleanup EXIT
go build -o "$tmp/minivectordb" ./cmd/minivectordb
bin="$tmp/minivectordb"
mkdir -p "$tmp/data"
printf '%s' '{"legacy":{"Columns":[{"Name":"id","Type":"INT","Dim":0}],"Rows":[[42]]}}' > "$tmp/data/snapshot.json"
cp "$tmp/data/snapshot.json" "$tmp/base.json"
# FIFO keeps the foreground stdin process alive until we kill it after acknowledgment.
mkfifo "$tmp/in"
exec 3<>"$tmp/in"
"$bin" -data "$tmp/data" <"$tmp/in" >"$tmp/out" 2>"$tmp/err" 3>&- &
pid=$!
cat >&3 <<'SQL'
CREATE TABLE items (id BIGINT PRIMARY KEY, label VARCHAR(32), embedding VECTOR(2));
INSERT INTO items VALUES (1, 'first', [1,0]);
BEGIN; INSERT INTO items VALUES (2, 'committed', [0,1]); COMMIT;
BEGIN; INSERT INTO items VALUES (3, 'rolledback', [1,0]); ROLLBACK;
INVALID SQL;
SELECT * FROM items ORDER BY L2_DISTANCE(embedding, [1,0]) LIMIT 1;
BEGIN; INSERT INTO items VALUES (4, 'uncommitted', [1,0]);
SELECT * FROM items;
SQL
for _ in {1..100}; do
 if grep -q '^OK 3$' "$tmp/out"; then break; fi
 kill -0 "$pid"
 sleep .05
done
grep -q '^OK 3$' "$tmp/out"
grep -q '^ERROR ' "$tmp/out"
grep -q $'^1\tfirst\t\[1,0\]$' "$tmp/out"
test -s "$tmp/data/wal.log"
cmp "$tmp/base.json" "$tmp/data/snapshot.json"
if timeout 5 "$bin" -data "$tmp/data" </dev/null >"$tmp/lockout" 2>"$tmp/lockerr"; then
 echo 'FAIL: second process accepted data directory'; exit 1
else
 test "$?" -eq 1
fi
grep -q 'ERROR opening database: cannot lock data directory' "$tmp/lockerr"
! grep -q 'ready' "$tmp/lockerr"
test ! -s "$tmp/lockout"

kill -9 "$pid"
wait "$pid" 2>/dev/null || true
pid=''
exec 3>&-
cat <<'SQL' | "$bin" -data "$tmp/data" >"$tmp/recovered" 2>"$tmp/err"
SELECT * FROM legacy;
SELECT * FROM items;
SELECT * FROM items ORDER BY L2_DISTANCE(embedding, [1,0]) LIMIT 1;
INVALID SQL
SELECT 1;
SQL
grep -q '^42$' "$tmp/recovered"
grep -q $'^2\tcommitted\t\[0,1\]$' "$tmp/recovered"
! grep -Eq 'rolledback|uncommitted' "$tmp/recovered"
grep -q '^OK 2$' "$tmp/recovered"
grep -q '^ERROR ' "$tmp/recovered"
[[ $(tail -3 "$tmp/recovered") == $'1\n1\nOK 1' ]]
for metric in L2_DISTANCE EUCLIDEAN DISTANCE COSINE DOT; do
 printf 'SELECT * FROM items ORDER BY %s(embedding, [1,0]) LIMIT 1;\n' "$metric" | "$bin" -data "$tmp/data" >"$tmp/rank"
 [[ $(cat "$tmp/rank") == $'id\tlabel\tembedding\n1\tfirst\t[1,0]\nOK 1' ]]
done
printf '%s\n' 'BEGIN' "INSERT INTO items VALUES (5, 'eof_uncommitted', [1,0])" | "$bin" -data "$tmp/data" >"$tmp/eof"
printf 'SELECT * FROM items;\nDELETE FROM items;\nSELECT * FROM items;\nDROP TABLE items;\n' | "$bin" -data "$tmp/data" >"$tmp/deleted"
! grep -q eof_uncommitted "$tmp/deleted"
grep -q '^OK 0$' "$tmp/deleted"
printf '%s\n' 'PASS: stdin/stdout, types, vector ranking, error continuation, SIGKILL snapshot + WAL recovery, COMMIT/ROLLBACK/EOF isolation, DELETE/DROP'
cat <<'SQL' | "$bin" -data "$tmp/m4" >"$tmp/m4out"
CREATE TABLE typed (id BIGINT PRIMARY KEY, label VARCHAR(2), v VECTOR(1));
INSERT INTO typed VALUES (9223372036854775807, '你好', [1]);
INSERT INTO typed VALUES (-9223372036854775808, '你好', [-1]);
INSERT INTO typed VALUES (0, '跳过', [0]);
INSERT INTO typed VALUES (1, '你好啊', [0]);
INSERT INTO typed VALUES (9223372036854775808, '好', [0]);
INSERT INTO typed VALUES (1, '好', [NaN]);
INSERT INTO typed VALUES (1, '好', [Inf]);
INSERT INTO typed VALUES (1, '好', [-Inf]);
INSERT INTO typed VALUES (1, '好', [1e40]);
INSERT INTO typed VALUES (1, '好', [0,0]);
SELECT * FROM typed ORDER BY L2_DISTANCE(v, [NaN]) LIMIT 1;
SELECT * FROM typed ORDER BY L2_DISTANCE(v, [Inf]) LIMIT 1;
SELECT * FROM typed ORDER BY L2_DISTANCE(v, [0,0]) LIMIT 1;
UNSUPPORTED SQL;
SELECT 1;
SQL
[[ $(grep -c '^ERROR ' "$tmp/m4out") == 11 ]]
[[ $(tail -3 "$tmp/m4out") == $'1\n1\nOK 1' ]]
cat <<'SQL' | "$bin" -data "$tmp/m4" >"$tmp/m4rank"
SELECT search_mode;
SELECT * FROM typed WHERE label = '你好' AND label = '你好' ORDER BY L2_DISTANCE(v, [0]) LIMIT 1;
SET search_mode=ann;
SELECT search_mode;
SELECT * FROM typed WHERE label = '你好' ORDER BY L2_DISTANCE(v, [0]) LIMIT 1;
SET search_mode=exact;
SELECT * FROM typed WHERE id = 9223372036854775807;
SQL
cat >"$tmp/expected" <<'EXPECTED'
search_mode
exact
OK 1
id	label	v
-9223372036854775808	你好	[-1]
OK 1
OK 0
search_mode
ann
OK 1
id	label	v
9223372036854775807	你好	[1]
OK 1
OK 0
id	label	v
9223372036854775807	你好	[1]
OK 1
EXPECTED
cmp "$tmp/expected" "$tmp/m4rank"
printf 'SELECT search_mode;\n' | "$bin" -ann -data "$tmp/m4" >"$tmp/mode"
[[ $(cat "$tmp/mode") == $'search_mode\nann\nOK 1' ]]
printf '%s\n' 'PASS: M4 int64 WAL round-trip, Unicode VARCHAR, finite float32/dimensions, filter-before-TopK, PK ties, exact/ann session and CLI, ERROR continuation'
# Yuque 2.2: clauses occupy one line under the temporary newline framing.
cat <<'SQL' | "$bin" -data "$tmp/m5" >"$tmp/m5out"
CREATE TABLE docs (
 id BIGINT PRIMARY KEY,
 category VARCHAR(32),
 content VARCHAR(1024),
 embedding VECTOR(4)
);
INSERT INTO docs VALUES (1, 'database', '事务与恢复', '[1, 0, 0, 0]'), (2, 'database', '向量索引', '[0, 1, 0, 0]'), (3, 'travel', '旅行攻略', '[0, 0, 1, 0]');
SELECT id, content, L2_DISTANCE(embedding, '[0.9, 0.1, 0, 0]') AS distance FROM docs WHERE category = 'database' ORDER BY distance LIMIT 2;
BEGIN;
UPDATE docs SET embedding = '[0.8, 0.2, 0, 0]' WHERE id = 1;
DELETE FROM docs WHERE id = 2;
ROLLBACK;
CREATE VECTOR INDEX idx ON docs(embedding);
SET search_mode=ann;
SELECT id FROM docs WHERE id >= 1 AND id <= 2 ORDER BY L2_DISTANCE(embedding,'[0.9,0.1,0,0]') LIMIT 2;
SET search_mode=exact;
SELECT id FROM docs WHERE id > 0 AND id < 3 ORDER BY L2_DISTANCE(embedding,'[0.9,0.1,0,0]') LIMIT 2;
INSERT INTO docs VALUES (4,'test','atomic','[0,0,0,0]'),(1,'test','duplicate','[0,0,0,0]');
SELECT id FROM docs ORDER BY id;
SQL
[[ $(grep -c '^ERROR ' "$tmp/m5out") == 1 ]]
grep -q $'^id\tcontent\tdistance$' "$tmp/m5out"
grep -q $'^1\t事务与恢复\t0.141421' "$tmp/m5out"
grep -q $'^2\t向量索引\t1.272792' "$tmp/m5out"
[[ $(tail -5 "$tmp/m5out") == $'id\n1\n2\n3\nOK 3' ]]
cat <<'SQL' | "$bin" -data "$tmp/m5" >"$tmp/m5recovered"
SET search_mode=ann;
SELECT id FROM docs ORDER BY L2_DISTANCE(embedding,'[0.9,0.1,0,0]') LIMIT 2;
SET search_mode=exact;
SELECT id FROM docs ORDER BY L2_DISTANCE(embedding,'[0.9,0.1,0,0]') LIMIT 2;
SQL
[[ $(cat "$tmp/m5recovered") == $'OK 0\nid\n1\n2\nOK 2\nOK 0\nid\n1\n2\nOK 2' ]]
printf '%s\n' 'PASS: M5 Yuque 2.2 projection/distance, PK, multi-INSERT atomicity, quoted vectors, UPDATE/DELETE rollback, comparisons, index ANN/exact and WAL restart'
# M7: bounded storage, index recovery and stderr-only interim readiness.
go test -count=1 -v ./internal/db -run '^TestM7' >"$tmp/m7tests"
grep -q -- '--- PASS: TestM7BeyondMemory' "$tmp/m7tests"
grep -q -- '--- PASS: TestM7ReadyGate' "$tmp/m7tests"
mkfifo "$tmp/m7in"
exec 3<>"$tmp/m7in"
"$bin" -data "$tmp/m7" -memory-budget=512 <"$tmp/m7in" >"$tmp/m7out" 2>"$tmp/m7err" 3>&- &
pid=$!
{
 printf '%s\n' 'CREATE TABLE t (id BIGINT PRIMARY KEY, label TEXT, v VECTOR(2));' 'BEGIN;'
 for i in {0..159}; do printf "INSERT INTO t VALUES (%d, 'payload beyond a tiny cache budget', [%d,1]);\n" "$i" "$i"; done
 printf '%s\n' 'CREATE VECTOR INDEX ix ON t(v);' 'COMMIT;' 'SELECT id FROM t WHERE id >= 100 ORDER BY L2_DISTANCE(v, [102,1]) LIMIT 3;'
} >&3
for _ in {1..200}; do
 if grep -q '^OK 3$' "$tmp/m7out"; then break; fi
 kill -0 "$pid"
 sleep .05
done
grep -q '^OK 3$' "$tmp/m7out"
grep -qx 'minivectordb: ready' "$tmp/m7err"
! grep -q 'ready\|ERROR' "$tmp/m7out"
kill -9 "$pid"
wait "$pid" 2>/dev/null || true
pid=''
exec 3>&-
cat <<'SQL' | "$bin" -data "$tmp/m7" -memory-budget=512 >"$tmp/m7recovered" 2>"$tmp/m7err"
SET search_mode=ann;
SELECT id FROM t WHERE id >= 100 ORDER BY L2_DISTANCE(v, [102,1]) LIMIT 3;
SET search_mode=exact;
SELECT id FROM t;
SQL
grep -qx 'minivectordb: ready' "$tmp/m7err"
! grep -q 'ready\|ERROR' "$tmp/m7recovered"
grep -q '^OK 160$' "$tmp/m7recovered"
[[ $(head -6 "$tmp/m7recovered") == $'OK 0\nid\n102\n101\n103\nOK 3' ]]
printf '%s\n' 'PASS: M7 beyond-memory 512-byte cache, 160 rows, cold filtered TopK, SIGKILL + WAL/index recovery (resident bounds asserted in Go tests)'
printf '%s\n' 'PASS: M7 ready-gate rejection/rebuild unit test, stderr ready handshake, clean stdout'
# M8 exercises real compaction and abrupt engine abandonment under tiny budgets.
go test -count=1 -v ./internal/db -run '^TestM8' >"$tmp/m8tests"
grep -q -- '--- PASS: TestM8CompactionStress' "$tmp/m8tests"
grep -q -- '--- PASS: TestM8BudgetBoundary' "$tmp/m8tests"
grep -q -- '--- PASS: TestM8AutoCompact' "$tmp/m8tests"
printf '%s\n' 'PASS: M8 compaction stress, automatic threshold, abrupt WAL recovery, bounded segment growth'
printf '%s\n' 'PASS: M8 budget boundary exclude: large results, oversized uncached row, eviction'
# M9 checksum integrity, interrupted durable writes and coarse own-index recall.
go test -count=1 -v ./internal/db -run '^TestM9' >"$tmp/m9tests"
for name in WALChecksum WALCorruptTail WALHardCrash ANNRecallCoarse; do
 grep -q -- "--- PASS: TestM9$name" "$tmp/m9tests"
done
printf '%s\n' 'PASS: M9 WAL checksum, corrupt/torn tail, SIGKILL before/after COMMIT durability'
printf '%s\n' 'PASS: M9 Sign-LSH ANN coarse recall@10=1.00 (synthetic), VmRSS observation'
# M10 explicit checkpoint, crash recovery, and repeated WAL reclamation.
go test -count=1 -v ./internal/db -run '^TestM10' >"$tmp/m10tests"
for name in WALCheckpoint WALBoundedGrowth CheckpointFailure; do
 grep -q -- "--- PASS: TestM10$name" "$tmp/m10tests"
done
printf '%s\n' 'PASS: M10 WAL checkpoint, active-txn reject, publication recovery, post-checkpoint checksum'
printf '%s\n' 'PASS: M10 bounded WAL growth, 600 batch commits, SIGKILL recovery of 601 committed rows'
# M11 deterministic 32D near-duplicates/distractors with an independent L2 oracle.
go test -count=1 -v ./internal/db -run '^TestM11ANNRecallRepresentative$' >"$tmp/m11tests"
grep -q -- '--- PASS: TestM11ANNRecallRepresentative' "$tmp/m11tests"
printf '%s\n' 'PASS: M11 representative 32D ANN recall, exact oracle, filtered TopK and cross-shell predicate'
# M12 retains M11's fixture with stronger recall floors and observational timing.
go test -count=1 -v ./internal/db -run '^TestM12' >"$tmp/m12tests"
grep -q -- '--- PASS: TestM12ANNFilteredExpansion' "$tmp/m12tests"
grep -q 'M12 timing:' "$tmp/m11tests"
printf '%s\n' 'PASS: M12 expanded Sign-LSH recall mean=1.00/min=1.00, SQL timing log, filtered distant-shell cardinality'
# M13 recovery includes real SIGKILL, many commits, final index rebuild and timing.
go test -count=1 -v ./internal/db -run '^TestM13Recovery$' >"$tmp/m13tests"
grep -q -- '--- PASS: TestM13Recovery' "$tmp/m13tests"
grep -q 'M13 recovery:' "$tmp/m13tests"
printf '%s\n' 'PASS: M13 4096-row recovery, 128 batch commits, SIGKILL, snapshot+redo, deferred index rebuild, timing log'
# M14 automatic checkpoint and observational process memory coverage.
go test -count=1 -v ./internal/db -run '^TestM14' >"$tmp/m14tests"
grep -q -- '--- PASS: TestM14AutoCheckpointCommits' "$tmp/m14tests"
grep -q -- '--- PASS: TestM14AutoCheckpointBytesDeferral' "$tmp/m14tests"
grep -q -- '--- PASS: TestM14AutoCheckpointFailure' "$tmp/m14tests"
grep -q -- '--- PASS: TestM14RSSObserve' "$tmp/m14tests"
printf '%s\n' 'PASS: M14 automatic checkpoint thresholds, active-txn deferral, failure retry, RSS observation'
# M15 nonblocking directory ownership and OS cleanup after SIGKILL.
go test -count=1 -v ./internal/db -run '^TestM15' >"$tmp/m15tests"
for name in Lock LockFailedOpen LockProcess; do
 grep -q -- "--- PASS: TestM15$name " "$tmp/m15tests"
done
printf '%s\n' 'PASS: M15 process lock, second-open rejection, Close/reopen, failed-open cleanup, SIGKILL restart'
# M16 wider RSS observations, automatic snapshot+redo scale, independent Gaussian ANN.
go test -count=1 -v ./internal/db -run '^TestM16' >"$tmp/m16tests"
for name in RSSRecoveryScale ANNGeneralization; do
 grep -q -- "--- PASS: TestM16$name " "$tmp/m16tests"
done
printf '%s\n' 'PASS: M16 16384-row 32D RSS/recovery, auto-checkpoint+redo, 64D Gaussian ANN recall and exact oracle'
# M17 repeats the original M16 Gaussian fixture with recall and ANN subset bounds.
go test -count=1 -v ./internal/db -run '^TestM17' >"$tmp/m17tests"
grep -q -- '--- PASS: TestM17ANNGaussian64' "$tmp/m17tests"
printf '%s\n' 'PASS: M17 Gaussian64 recall mean>=0.75/min>=0.40, filtered/unfiltered, candidates<=75% of eligible rows'

# M18 broader ANN and observational RSS; no timing or RSS caps.
go test -count=1 -v ./internal/db -run '^TestM18' >"$tmp/m18tests"
grep -q -- '--- PASS: TestM18ANNBroaderRSS ' "$tmp/m18tests"
printf '%s\n' 'PASS: M18 4096-row 96D ANN recall mean>=0.75/min>=0.40, exact oracle, filtered candidates, VmRSS observation'
