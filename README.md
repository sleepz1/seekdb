# MiniVectorDB

A single-process, single-client vector database written from scratch in Go
(standard library only) for the 2026 OceanBase competition, extra round
(`2026-competition-round2`). It supports a SQL subset, exact filtered vector
TopK, durable storage in `/data`, transactions and crash recovery.

## Build and run

Language: Go 1.22 (no third-party modules). Runtime image: Ubuntu 24.04.

```sh
docker build -t minivectordb:latest .
docker run --rm -i -v minivectordb-data:/data minivectordb:latest
```

Without Docker:

```sh
go build -o minivectordb ./cmd/minivectordb
./minivectordb --data-dir /tmp/mvdb
```

The process runs in the foreground, reads SQL from **stdin**, writes one
response line per statement to **stdout** and logs to **stderr**. It opens no
network port.

| Flag / env | Default | Meaning |
| --- | --- | --- |
| `--data-dir` / `MVDB_DATA_DIR` | `/data` | data directory (log file lives here) |
| `--mode` / `MVDB_SEARCH_MODE` | `exact` | default vector search mode, `exact` or `ann` |
| `--format` / `MVDB_FORMAT` | `json` | response format, `json` or `tsv` |
| `--ready-line` / `MVDB_READY_LINE` | empty | line printed to stdout after recovery (none by default) |
| `--cache-mb` | auto | decoded-row cache budget; auto = 1/4 of the cgroup memory limit |

### Protocol

* **Statement boundaries**: a statement ends at `;` outside quotes/comments.
  Until the first `;` is seen, a newline (outside quotes, brackets balanced)
  also ends a statement, so "one statement per line" input works with or
  without semicolons; once `;` is in use, statements may span lines.
* **Responses** (`--format json`, one line each, field names as in the public
  `.result` files):
  * query: `{"columns":["id","distance"],"rows":[[2,1.0],[3,2.0]]}`
  * other success: `{"affected_rows":3}` (DDL and transaction control report 0)
  * error: `{"error":{"code":1062,"sqlstate":"23000","message":"Duplicate entry '1' for key 'PRIMARY'"}}`

  BIGINT values are JSON integers, distances are JSON floats (always with a
  decimal point), VARCHAR values are strings, vector values are strings such
  as `"[3,4]"`. Output is flushed after every statement.
* **Ready**: the database only starts reading stdin after recovery has
  finished. `minivectordb: ready` is written to stderr; an optional stdout
  ready line can be configured with `--ready-line`.
* **Exact / ANN**: default `exact`. `SET search_mode = ann` / `exact` changes
  the session mode, `--mode ann` sets the default, and `ORDER BY ... APPROXIMATE`
  is accepted. ANN currently runs the same exact search (recall 1.0).
* **Shutdown**: EOF on stdin, `EXIT;`/`QUIT;`, SIGTERM or SIGINT. An open
  transaction is rolled back; committed data is already durable.

> The official test driver framing is not public. JSON lines matching the
> `.result` field names is this implementation's choice; `--format tsv` gives
> a mysql `--batch`-like alternative. Error numbers follow MySQL conventions.

## Example (empty data directory)

```sql
CREATE TABLE docs (id BIGINT PRIMARY KEY, category VARCHAR(8), content VARCHAR(16), embedding VECTOR(2));
INSERT INTO docs VALUES (3, '乙', '三', '[0, 2]'), (1, '甲', '一', '[0, 0]'), (2, '甲', '二', '[1, 0]');
SELECT * FROM docs WHERE category = '甲' ORDER BY id;
SELECT id, L2_DISTANCE(embedding, '[0, 0]') AS distance FROM docs ORDER BY distance LIMIT 10;
BEGIN;
UPDATE docs SET embedding = '[3,4]' WHERE id = 1;
DELETE FROM docs WHERE id = 3;
ROLLBACK;                       -- both changes are undone
BEGIN;
INSERT INTO docs VALUES (4, '乙', '四', '[2, 2]');
COMMIT;                         -- durable before this response is written
CREATE VECTOR INDEX idx_emb ON docs(embedding);
```

After a restart (or `kill -9`) with the same `/data`, the table contains ids
1, 2, 3, 4 with their committed values.

## Supported SQL

* `CREATE TABLE [IF NOT EXISTS] t (col type [PRIMARY KEY] [NOT NULL], ..., [PRIMARY KEY (col)])`
  with `BIGINT` (also `INT`/`INTEGER`), `VARCHAR(n)`, `VECTOR(n)`; exactly one
  BIGINT primary key, at most one VECTOR column.
* `DROP TABLE [IF EXISTS] t`, `CREATE VECTOR INDEX name ON t(vec_col)`, `DROP INDEX name ON t`.
* `INSERT INTO t [(cols)] VALUES (...), (...)`: all columns required.
* `SELECT * | expr [AS alias], ... FROM t [WHERE ...] [ORDER BY expr|alias|ordinal [ASC|DESC], ...] [LIMIT n [OFFSET m] | LIMIT m, n]`.
* `UPDATE t SET col = value, ... [WHERE ...]`; `DELETE FROM t [WHERE ...]`.
* Predicates: `=, !=, <>, <, <=, >, >=`, `AND`, `OR`, `NOT`, parentheses.
* `L2_DISTANCE(vec_col, '[...]')`: Euclidean distance (not squared), usable in
  the select list, `ORDER BY` and `WHERE`.
* `BEGIN` / `START TRANSACTION`, `COMMIT`, `ROLLBACK`; `SET search_mode = ...`.

Semantics: duplicate primary keys are rejected; VARCHAR length counts Unicode
characters; strings compare by code point; vectors are float32, and NaN,
Infinity, wrong dimension and float32 overflow are rejected; distances are
accumulated in float64 over the stored float32 values. Ties in any `ORDER BY`
(including DESC and distance ties) are broken by primary key ascending. Results
without `ORDER BY` come out in primary-key order. A filtered TopK applies the
`WHERE` clause to every visible row before choosing the K nearest. `UPDATE`
reports matched rows. A failed statement leaves no partial changes; in an open
transaction, earlier successful statements remain.

Not supported (error code 1235 or 1064, never silently reinterpreted): NULL,
JOIN, aggregates, GROUP BY, subqueries, ALTER TABLE, foreign keys, nested
transactions (`BEGIN` inside a transaction is an error) and primary-key
updates. DDL inside a transaction implicitly commits it first (MySQL
behaviour).

## Design

**Storage.** `/data/mvdb.log` is an append-only log and the only durable
structure. Every committed transaction, autocommit statement or DDL statement
is one record: `u32 length | u32 CRC32C | payload`, where the payload is a
list of ops (create/drop table, create/drop index, put row, delete row). Rows
use a compact binary encoding (8-byte BIGINT, varint-prefixed UTF-8, raw
little-endian float32 vectors). COMMIT writes the record and calls
`fdatasync` before the response is printed.

**In-memory state.** Per table: a primary-key hash index pointing to slots
that hold the row's file offset and length, plus the decoded row while the
row cache has room. The cache budget is derived from the cgroup memory limit
(`memory.max / 4`) and `GOMEMLIMIT` is set to 60% of the limit. Rows that do
not fit are read back from the log with `pread`; full scans read them in file
order. Tables larger than memory therefore work, with only the index resident.

**Transactions.** One explicit transaction keeps a per-table map
`pk -> new row | tombstone`. Each statement first builds its own change set
over (statement ∪ transaction ∪ committed) views; only if it succeeds is it
merged into the transaction (or committed immediately in autocommit mode).
Reads, including vector TopK, see statement, then transaction, then committed
versions, giving read-your-writes. ROLLBACK discards the map, so nothing
uncommitted ever reaches the log.

**Recovery.** Startup scans the log, applying every record whose length and
CRC are valid. The first torn or invalid record marks the end: the file is
truncated there and synced. A kill during COMMIT therefore yields all-or-
nothing, and recovery is idempotent if it is itself interrupted. The engine
reads stdin only after recovery is complete.

**Compaction.** When the log is over 64 MiB and more than half dead, live rows
are rewritten into `mvdb.log.compact`, which is fsynced and atomically renamed
over the log. A leftover temporary file from a killed compaction is deleted on
startup.

**Vector search.** Exact search only. The TopK path scans the slot array
linearly (sequential memory), applies the filter, computes float64 L2 with a
4-way unrolled loop and early termination against the current K-th distance
(with a safety margin so primary-key tie-breaks are unaffected), and keeps a
bounded max-heap. On tables of 4096 rows or more with no pending transaction
changes, the scan is split across `min(NumCPU, cgroup CPU quota)` goroutines
and merged in the same (distance, pk) order. `CREATE VECTOR INDEX` is
persisted and validated; since the search is exact, index maintenance after
inserts, updates and deletes is correct by construction.

## Verification

```sh
go test ./...                       # engine tests (also passes with -race)
go build -o minivectordb ./cmd/minivectordb
python3 scripts/run_public_cases.py # 5/5 public mysqltest cases
python3 scripts/crash_test.py       # SIGKILL loop: acknowledged commits kept, no partial txns
python3 scripts/bench.py            # 10k x 128 workload
```

The engine tests cover restart and simulated crash, torn tails cut at every
7th byte of a record, transaction read-your-writes and rollback (including
vector search), statement atomicity, validation errors, PK tie ordering,
filter-before-TopK, randomized comparison against a brute-force model in
fully cached and fully on-disk modes, compaction with crash, and
parallel-vs-sequential equality.

Local results on a 2-vCPU sandbox:

* 5/5 public cases pass.
* `crash_test.py`: 40 SIGKILL rounds, 11,405 committed rows verified.
* `bench.py` (10,000 × 128-dim, batches of 100, 1,000 queries with half
  filtered, K = 10): about 3.7 ms per committed batch, 0.84 ms per query end
  to end, 22 MiB peak RSS, 5.2 MiB data.

## Known limitations

* The official driver protocol (response framing, error codes, ready marker,
  mode parameter) is not public yet. The choices above may need adjusting
  once the platform's environment pre-check reports a protocol mismatch.
* ANN mode is exact search, so it is correct but not faster.
* The primary-key index and slot table stay in memory (about 70 bytes per
  row). Very large tables are bounded by that, not by row size.
* `UPDATE`/`DELETE` collect the matching keys in memory before applying them.
