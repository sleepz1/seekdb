# M3b self-test

Built on M3 HEAD `6baa3fd` without changing `internal/db/wal.go`.
Observed commands from the repository root, all exit 0:

```sh
go test ./...
go test -race ./...
./scripts/smoke.sh
go build -o minivectordb ./cmd/minivectordb
printf 'SELECT 1;\n' | ./minivectordb -data /tmp/x
sg docker -c 'docker build -t minivectordb:m3 .'
cmp conversation.json agent/conversation.json
```

Manual demo stdout:

```text
1
1
OK 1
```

Exact smoke output:

```text
PASS: stdin/stdout, types, vector ranking, error continuation, SIGKILL snapshot + WAL recovery, COMMIT/ROLLBACK/EOF isolation, DELETE/DROP
```

Primary smoke builds in a temporary directory and uses stdin exclusively, with no
listener or MySQL client. A FIFO keeps the foreground process alive after COMMIT
acknowledgment, before SIGKILL. Restart uses the same data directory and proves
committed rows survive, rollback and active uncommitted rows do not, the legacy
snapshot remains byte-identical, and vector ranking survives WAL replay. A
separate EOF session proves uncommitted writes are discarded. Bad SQL is followed
by successful SELECT output. All five distance names, BIGINT and VARCHAR(n),
DELETE and DROP are exercised.

New REPL tests cover multiline parentheses, semicolon splitting, doubled SQL
quotes, semicolons and tabs in strings, TSV escaping, integer rejection with
continuation, EOF without a terminator, and transaction recovery. Existing WAL
tests still cover atomic committed records, torn tails, repeat recovery,
corruption/LSN rejection, snapshot bases, isolation and conflicts. Existing MySQL
wire tests also pass. Race tests pass.

Docker image minivectordb:m3 built successfully; no image was pushed. Container
runtime was not tested. The manual binary is not committed.

Open: exact round2 pack framing and broader SQL grammar; precise full-range BIGINT
storage (current INT-compatible float64); VARCHAR length enforcement; ANN is a
no-op and searches remain exact; atomic checkpoints/compaction, checksums,
process locking, WAL growth and cloning costs, disk-failure fault injection.
Only one process may own a data directory. No formal submission or PR merge.
