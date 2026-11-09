#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "$PWD/.m3-smoke.XXXXXX")
pid=''
cleanup() { if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi; rm -rf "$tmp"; }
trap cleanup EXIT
go build -o "$tmp/minivectordb" ./cmd/minivectordb
go build -o "$tmp/mysqlsmoke" ./scripts/mysqlsmoke
start() {
  "$tmp/minivectordb" -addr 127.0.0.1:0 -data "$tmp/data" >"$tmp/server.log" 2>&1 &
  pid=$!
  for _ in {1..100}; do
    if grep -q 'listening on' "$tmp/server.log"; then addr=$(sed -n 's/.*listening on //p' "$tmp/server.log"); return; fi
    kill -0 "$pid" || { cat "$tmp/server.log"; exit 1; }
    sleep .05
  done
  cat "$tmp/server.log"; exit 1
}
q() { "$tmp/mysqlsmoke" -addr "$addr" -query "$1"; }
# An M2 snapshot base must continue to recover alongside new WAL commits.
mkdir -p "$tmp/data"
printf '%s' '{"legacy":{"Columns":[{"Name":"id","Type":"INT","Dim":0}],"Rows":[[42]]}}' > "$tmp/data/snapshot.json"
cp "$tmp/data/snapshot.json" "$tmp/base.json"
start
[[ $(q 'SELECT * FROM legacy') == '[["42"]]' ]]
q 'CREATE TABLE items (id INT, label TEXT, embedding VECTOR(2))'
q "INSERT INTO items VALUES (1, 'first', [1,0])"
q "INSERT INTO items VALUES (2, 'second', [0,1])"
result=$(q 'SELECT * FROM items ORDER BY EUCLIDEAN(embedding, [1,0]) LIMIT 1')
[[ "$result" == *'[["1","first","[1,0]"]]'* ]]
for metric in DISTANCE COSINE DOT; do
  [[ $(q "SELECT * FROM items ORDER BY $metric(embedding, [1,0]) LIMIT 1") == *'first'* ]]
done
"$tmp/mysqlsmoke" -addr "$addr" -password '' -query 'SELECT 1'
if q 'INVALID SQL' >"$tmp/error.log" 2>&1; then echo 'Expected ERR'; exit 1; fi
"$tmp/mysqlsmoke" -addr "$addr" -query BEGIN -query "INSERT INTO items VALUES (3, 'committed', [0.9,0])" -query COMMIT -query 'SELECT * FROM items' > "$tmp/committed.json"
[[ $(cat "$tmp/committed.json") == *'committed'* ]]
"$tmp/mysqlsmoke" -addr "$addr" -query 'START TRANSACTION' -query "INSERT INTO items VALUES (4, 'rolledback', [1,0])" -query ROLLBACK
"$tmp/mysqlsmoke" -addr "$addr" -query BEGIN -query "INSERT INTO items VALUES (5, 'uncommitted', [1,0])"
test -s "$tmp/data/wal.log"
cmp "$tmp/base.json" "$tmp/data/snapshot.json"
# SIGKILL exercises recovery without a graceful shutdown.
kill -9 "$pid"; wait "$pid" 2>/dev/null || true; pid=''
start
result=$(q 'SELECT * FROM items')
[[ "$result" == *'second'* && "$result" == *'committed'* && "$result" != *'rolledback'* && "$result" != *'uncommitted'* ]]
[[ $(q 'SELECT * FROM legacy') == '[["42"]]' ]]
for metric in EUCLIDEAN DISTANCE COSINE DOT; do
  [[ $(q "SELECT * FROM items ORDER BY $metric(embedding, [1,0]) LIMIT 1") == *'first'* ]]
done
q 'DELETE FROM items'
[[ $(q 'SELECT * FROM items') == '[]' ]]
q 'DROP TABLE items'
printf '%s\n' 'PASS: build, MySQL wire (go-sql-driver/mysql), empty/any-password auth, ERR, vector ranking, SIGKILL snapshot + WAL recovery, COMMIT/ROLLBACK/disconnect isolation, recovered vector ranking, DELETE/DROP'
