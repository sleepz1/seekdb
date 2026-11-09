# MiniVectorDB M3b — stdin/stdout and WAL

Pure Go SQL/vector engine. No external storage engine or vector library. Requires
Go 1.22+; smoke requires Bash. Default mode is a foreground stdin session with
stdout results, stderr logs, no network listener, and data directory `/data`.
The confirmed contest protocol is stdin/stdout; exact round2 pack framing is TBD.

```sh
go build -o minivectordb ./cmd/minivectordb
printf 'SELECT 1;\n' | ./minivectordb -data /tmp/x
./scripts/smoke.sh
```

Temporary protocol (stable until pack reconciliation): SQL statements end at a
semicolon outside SQL single quotes, or a newline outside quotes and balanced
parentheses/brackets. Multiple statements per line are supported. Multiline
CREATE/INSERT works inside parentheses; arbitrary multiline clauses must stay on
one line. SQL doubled quotes (`''`) escape quotes. Comments are not supported.
EOF executes any remaining statement, then exits cleanly; unfinished transaction
writes are discarded. Malformed unterminated quotes consume input until EOF and
produce an error. Each stdin process owns one Session.

Successful writes and transaction commands print `OK <affected>` (zero for DDL
and transaction commands). SELECT prints tab-separated column names, then one row
per line, then `OK <rowcount>`, including empty results. Text cells escape backslash,
tab, newline and carriage return as `\\`, `\t`, `\n`, `\r`; numbers and vectors
use JSON notation (vectors are arrays). SQL errors print `ERROR <message>` and the
loop continues. Every response is flushed immediately. Startup and I/O failures
log to stderr and exit nonzero. There is no stdout banner.

```sql
CREATE TABLE items (id BIGINT, label VARCHAR(32), embedding VECTOR(2));
BEGIN;
INSERT INTO items VALUES (1, 'first', [1,0]);
COMMIT;
SELECT * FROM items ORDER BY L2_DISTANCE(embedding, [1,0]) LIMIT 1;
```

Supported SQL: CREATE TABLE with INT/INTEGER/BIGINT, TEXT/VARCHAR/VARCHAR(n),
FLOAT/DOUBLE, VECTOR(n); single-row INSERT INTO ... VALUES; SELECT * with optional
vector ORDER BY and LIMIT; SELECT 1; whole-table DELETE FROM; DROP TABLE;
BEGIN/START TRANSACTION, COMMIT, ROLLBACK. VARCHAR uses SQL quotes like TEXT;
its optional length is accepted as metadata syntax, not enforced. BIGINT uses the
existing INT integer check and float64 storage, so integers above 2^53 are not
necessarily exact. VECTOR requires its declared positive dimension.
L2_DISTANCE, EUCLIDEAN and DISTANCE compute Euclidean distance; COSINE computes
cosine distance, DOT negative inner product. ORDER BY is ASC by default, with
optional DESC; ties retain insertion order. Functions work in ORDER BY, not
projections. Searches are exact; `-ann` is a reserved no-op flag.

BEGIN/START TRANSACTION creates a private state copy with read-your-writes;
other sessions see only committed state. COMMIT checks the engine version,
appends all successful writes as one newline-terminated JSON WAL record with a
sequential LSN, and fsyncs before publishing state or returning success.
Autocommit writes use the same path with one statement. ROLLBACK and disconnect
discard private writes. A failed statement leaves prior successful statements
in the transaction intact. Concurrent commits use a coarse database-wide version
check; conflicts require ROLLBACK. Nested BEGIN is rejected. COMMIT/ROLLBACK
without an active transaction are no-ops.

Open loads the legacy snapshot, then replays complete WAL records in LSN order.
An incomplete final record is truncated and synced before accepting new writes;
malformed complete records or LSN gaps fail startup. WAL creation is synced along
with its directory. Read-only transactions write no redo. Write/fsync failure
blocks subsequent durable writes until restart; a commit interrupted before its
acknowledgment has an uncertain outcome and may recover if its full record exists.
Only one server/Engine may own a data directory at a time; there is no process lock.

M3 does not rewrite the base snapshot or compact the WAL. New databases recover
entirely from WAL; old databases retain their immutable snapshot base. Keep both
files together. Automatic atomic checkpoints, WAL checksums, process locking,
bounded log growth, and faster recovery remain open work. Transactions still
clone the in-memory database, and redo uses the supported deterministic SQL
statements rather than page-level changes.
Optional local MySQL wire helper replaces stdin mode only when explicitly set:

```sh
./minivectordb -mysql-addr 127.0.0.1:3306 -data /tmp/wire-data
go run ./scripts/mysqlsmoke -addr 127.0.0.1:3306 -query 'SELECT 1'
```

`-addr` is an alias. The retained helper supports protocol 10, COM_QUERY, COM_PING,
COM_INIT_DB and COM_QUIT, JSON vectors and text results. Authentication accepts
any username/password; no TLS, prepared statements or wire multi-statements.
Wire-specific SELECT VERSION() and SET NAMES remain available. Its transaction
commands must share a connection (repeat client `-query` flags). Primary smoke
does not use the wire helper.

```sh
sg docker -c 'docker build -t minivectordb:m3 .'
printf 'SELECT 1;\n' | docker run --rm -i -v minivectordb-data:/data minivectordb:m3
```

Docker uses CGO=0 and scratch, WORKDIR /data, ENTRYPOINT /minivectordb and CMD
`-data /data`; no port exposure is needed. See [SELFTEST.md](SELFTEST.md).
Exact official framing and broader contest SQL dialect remain open. No official
contest submission or PR merge has been made.
