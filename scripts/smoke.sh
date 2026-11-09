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
CREATE TABLE items (id BIGINT, label VARCHAR(32), embedding VECTOR(2));
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
