# MiniVectorDB M12 — Sign-LSH recall and performance

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
CREATE TABLE items (id BIGINT PRIMARY KEY, label VARCHAR(32), embedding VECTOR(2));
BEGIN;
INSERT INTO items VALUES (1, 'first', [1,0]);
COMMIT;
SELECT * FROM items ORDER BY L2_DISTANCE(embedding, [1,0]) LIMIT 1;
```

Supported SQL: CREATE TABLE with exactly one inline BIGINT PRIMARY KEY and at most
one VECTOR(n); INT/INTEGER, TEXT/VARCHAR/VARCHAR(n), FLOAT/DOUBLE remain available
for other columns. INSERT INTO ... VALUES supports multiple rows atomically.
UPDATE ... SET col=literal [, ...] WHERE predicates rejects PK assignments.
DELETE FROM supports predicates or deletes all rows without WHERE. SELECT supports
* or projected columns/distance expressions, optional AS aliases, ordinary-column
or distance ORDER BY with ASC/DESC, and LIMIT. WHERE supports =, <, <=, >, >= and
AND; vectors are rejected. BIGINT compares numerically; UTF-8 text compares in
Unicode code point order. Also: SELECT 1, DROP TABLE, BEGIN/START TRANSACTION,
COMMIT, ROLLBACK. Unsupported SQL returns ERROR and the next statement still runs.
Duplicate primary keys are rejected without partial writes, including duplicate
keys within one multi-row INSERT. Bare and SQL-quoted vector arrays are accepted
in INSERT, UPDATE and distance expressions.
INT/INTEGER/BIGINT use int64 and ParseInt: fractions, exponent notation and values
outside [-9223372036854775808, 9223372036854775807] are rejected. Typed transaction
copies and snapshot decoding preserve integer precision; WAL retains SQL literals.
VARCHAR(n) enforces a positive maximum Unicode rune count, not bytes; bare VARCHAR
and TEXT remain unlimited. Strings require SQL quotes and valid UTF-8.
VECTOR(n) requires a positive fixed dimension, stores []float32, and rounds inputs
to float32. NaN, infinities, null elements, conversion overflow and dimension
mismatch are errors on insertion and query. Distance arithmetic accumulates in
float32 (square root uses Go math then rounds to float32); nonfinite distance
results are errors, including overflow from large finite input values.
L2_DISTANCE is primary; EUCLIDEAN and DISTANCE are aliases. COSINE computes cosine
distance and DOT negative inner product. Filtering precedes candidate selection,
distance sorting and LIMIT. All ORDER BY ties use the explicit primary key
ascending, including DESC. Example projection:
`SELECT id, L2_DISTANCE(embedding, '[1,0]') AS distance FROM items ORDER BY distance LIMIT 2;`

Search defaults to exact. `-ann` sets the mode for new sessions;
`SET search_mode=ann` / `SET search_mode=exact` changes the current session and
`SELECT search_mode` reads it. Mode is session configuration, unaffected by
ROLLBACK and not persisted. `CREATE VECTOR INDEX idx ON items(embedding)`
creates a self-built sign-LSH index using up to eight vector coordinates.
ANN probes complete Hamming shells through radius 2 and expands to a post-filter
8*K candidate target (capped by table size); it has no general recall guarantee. Without an index, the retained M4 ANN
fallback samples every second filtered row. Exact always scans all qualifying
rows, regardless of indexes. Queries without vector ORDER BY are unaffected.
Index metadata is stored in snapshot JSON / WAL DDL. Startup rebuilds every bucket
before marking the engine ready; online CREATE VECTOR INDEX finishes rebuilding
before success. Transactions share immutable buckets until mutation rebuilds them.
Rollback discards private index changes. This is not yet a performance-tuned index.

```sql
SELECT * FROM items WHERE label = 'first' AND id = 1 ORDER BY L2_DISTANCE(embedding, [1,0]) LIMIT 1;
SET search_mode=ann;
SELECT search_mode;
SET search_mode=exact;
```

Startup emits `minivectordb: ready` on **stderr only**, after recovery and index
rebuild. `Engine.Ready()` remains false until then; `Session.Execute` returns
`database not ready` during the gate. `Open` never returns a partially ready engine.
This is our interim readiness handshake; official ready framing is **待题包确认**.
Numeric error codes, exact framing and official ANN/recall requirements remain
pending. SQL errors allow continued input; startup/I/O failures exit nonzero.

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

`-memory-budget=BYTES` bounds the decoded row cache (CLI default: 67108864 bytes,
64 MiB). `0` selects the legacy unlimited path. Library callers retain compatibility:
`Open(dir)` is unlimited; `OpenWithOptions(dir, OpenOptions{MemoryBudgetBytes: n})`
enables bounded storage. Negative budgets are rejected.

Bounded mode stores immutable one-row pages in `segments/rows-*.page` beneath
`-data`. Tables retain offset/length directories, not `Table.Rows` payloads;
transaction copies copy references. A conservative single-page hot cache evicts
before decoding a different page. Rows larger than the budget are read uncached.
The budget covers decoded cached payloads, not a process RSS limit: one active row,
SQL/parser buffers, returned results, schema, page references, PK validation keys,
index buckets and ordering keys are additional memory. Limited TopK retains at
most k candidates plus the current candidate. Unbounded SELECT results still scale
with output size. `ResidentRowBytes`, `PeakResidentRowBytes` and `ResidentPages`
expose cache accounting for automated tests.

Successful mutations append immutable pages; COMMIT syncs segments before writing
and syncing the authoritative JSON WAL batch. Active transaction SQL is spooled to
`segments/tx-*.pending`, so a batch does not retain all SQL payloads in RAM. Failed
statements discard private references; rollback and EOF never publish their pages
or pending SQL. Recovery streams complete WAL batches one statement at a time and
legacy snapshots one row at a time, reconstructing a fresh segment and page
directory without loading all payloads together. Index rebuild scans cold pages.
Snapshot and WAL remain the recovery authority; keep both together. Old segments
and abandoned transaction spools are never recovery input; successful bounded
startup compacts the recovered generation and removes abandoned generations/spools.

Recovery replay cost, page/reference copying, per-row file I/O, large individual
SQL statements/results and process locking remain open work. Only one Engine/process may own a data
directory. Checkpointing is explicit through `Engine.Checkpoint()`; no automatic threshold is enabled.
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

M8 adds `Engine.Compact()` for bounded storage. It copies only live page bytes
into a new segment, fsyncs the file and directory, then publishes new references
and removes the old segment. Row ordinals and index buckets remain unchanged.
Active transactions pin references: explicit compaction returns an error while
any are active. Automatic maintenance runs after writes/COMMIT when the current
segment is at least 1 MiB and more than twice its live bytes, unless transactions
are active. Automatic maintenance errors do not undo an acknowledged durable
commit; explicit Compact reports errors. Unlimited `Open` is a no-op for Compact.
Snapshot + WAL remain authoritative through interruption at any compaction stage.
Recovery compacts before readiness; this bounds recovered segment generations,
not WAL size or peak temporary disk usage. One Engine owns each data directory.
Library callers must end transactions with COMMIT/ROLLBACK to release their pins.

Ready framing adapter: `cmd/minivectordb/main.go:reportReady` is the single switch
point for the interim stderr-only `minivectordb: ready`; official framing remains
**待题包确认**. Upstream fetched for M8:
`c842a368c3394748d0499386c18f6b465242e141` (unchanged scaffold).

Budget policy is **exclude**: ResidentRowBytes/PeakResidentRowBytes meter only
decoded row page cache contents. Active/oversized uncached rows, SQL/parser and
copy buffers, returned result sets, schema, page references, PK validation keys,
index buckets and ordering keys are excluded. This is not an RSS bound.
`go test -count=1 ./internal/db -run 'M7|M8|Compact|Budget|Stress' -v` verifies
compaction, recovery, readiness, large results, oversized rows and eviction.

## M9 WAL checksum, hard crash and coarse recall

Upstream competition branch fetched before coding: `c842a368c3394748d0499386c18f6b465242e141`
(unchanged scaffold). Official ready/error/result framing remains **待题包确认 / TBD**.
The single `reportReady` adapter still emits `minivectordb: ready` on stderr only,
after snapshot/WAL recovery and index rebuild; stdout flush behavior is unchanged.

Every new WAL record includes an IEEE CRC32 checksum. Input is the little-endian
uint64 LSN followed, for each statement in order, by its little-endian uint64
UTF-8 byte length and UTF-8 bytes. JSON escaping is not part of the checksum.
Recovery requires checksums (pre-M9 checksum-free WAL is not accepted), verifies
a complete record before applying any of its statements, and uses two streaming
passes without retaining the whole batch. Complete-record corruption fails Open
with an offset and checksum error; it does not skip records or modify the bad WAL.
The committed prefix remains recoverable by explicit operator repair: preserve a
backup and truncate at the reported first bad record offset, discarding that record
and everything later. An incomplete final line is automatically truncated as before.
CRC32 detects accidental corruption, not malicious tampering. An unacknowledged
complete record may recover; failure before acknowledgment alone does not prove rollback.

`TestM9WALChecksum` and `TestM9WALCorruptTail` cover valid checksums, altered payloads,
checksum damage, missing checksums, bad middle records, explicit prefix repair,
and multiple truncation offsets. `TestM9WALHardCrash` kills subprocesses after a
real streaming COMMIT writer is interrupted with a partial record before fsync,
and after a fully synced COMMIT; two reopens check absence/presence respectively.
Existing smoke retains real CLI SIGKILL and ready-gate checks.

`TestM9ANNRecallCoarse` builds 160 deterministic 2D vectors in four sign-separated
clusters around (+/-10,+/-10), with small deterministic offsets, and compares four
indexed Sign-LSH queries to exact Top10. Recall is intersection size / 10. Measured
recall@10 was **1.00 for each query, mean 1.00**; each query asserts only a 0.10
floor and rejects empty ANN output. Exact neighbors share each query's sign bucket,
so this is an intentionally easy degeneration check, not an official contest metric
or a general recall guarantee. No external ANN library is used.
Linux `/proc/self/status` VmRSS in the targeted run was 5700 kB before and 6420 kB
after the recall test. These are observations only, vary across runs, and do not
establish a process RSS cap; page cache budget is not process RSS. Unsupported
systems log an observation skip.

M9 validation: `go test ./...`,
`go test -count=1 ./internal/db/ -run 'M9|WALChecksum|HardCrash|ANNRecall|Corrupt' -v`,
`./scripts/smoke.sh`, JSON identity/size/pointer checks and `git diff --check`.
OPEN_ITEMS: official framing; WAL checkpoint/growth; process locking; recovery
performance; total RSS/large statements/results; representative ANN recall and
performance. M9 supersedes earlier checksum and unmeasured coarse-recall open items.
No formal contest submission or 提测.

## M10 explicit WAL checkpoint / bounded growth

Upstream fetched before coding: `a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c`
(samples only; companion driver/framing remains unpublished). `reportReady` remains
the single interim stderr adapter; official framing is TBD. Dockerfile and stdin
result flushing are unchanged. No formal submission / 提测.

`Engine.Checkpoint()` holds the engine mutex and rejects active transactions,
not-ready engines and poisoned WAL writers. It streams committed rows and index
metadata into a temporary snapshot under `-data`, fsyncs it, renames it to
`snapshot.json`, and fsyncs the directory before truncating and fsyncing `wal.log`.
Snapshots use `[checkpointLSN, tableMap]`; table schemas/rows/index metadata retain
the existing encoding, and Open still accepts legacy bare table-map snapshots.
Recovery verifies checksums even on retained WAL records covered by the snapshot,
skips applying those records, and replays later sequential LSNs. Thus interruption
between snapshot publication and WAL truncation cannot duplicate committed SQL.
New records retain M9 CRC32 checksums and monotonically increasing LSNs. A failed
WAL truncation/sync blocks further writes until recovery. Temporary snapshots are
not recovery input; a killed checkpoint may leave an ignored temporary file.

Policy: explicit library API only, no SQL/protocol extension or automatic trigger.
After each successful checkpoint WAL size is **0 bytes**; between calls its size
is the committed redo since the last checkpoint. Callers must schedule checkpoints
to bound growth; the default CLI still has no automatic WAL bound. Snapshot size
scales with live data, and replacement temporarily requires old plus new snapshot
space as well as WAL. Bounded mode streams one row at a time; existing cache-budget
exclusions still apply. Checkpoint does not replace page-segment Compact.

`TestM10WALBoundedGrowth` uses a 128-byte cache, three rounds of 200 committed
batches, and checks WAL sizes **15418->0**, **15543->0**, **15548->0 bytes**.
It then commits row 600, leaves row 999 uncommitted, SIGKILLs the subprocess and
reopens twice: all 601 committed IDs survive, row 999 does not. Checkpoint tests
also cover unlimited/bounded recovery, an empty checkpoint, active-txn rejection,
rollback, deleted rows, index metadata, snapshot publication with old WAL retained,
post-checkpoint checksum corruption, failed snapshot rename and poisoned writers.

Validation: `go test ./...`, targeted `M10|Checkpoint|BoundedGrowth|WALChecksum|HardCrash`,
and smoke with all prior PASS lines plus M10. M10 closes the explicit checkpoint
work item; automatic scheduling, process locking, official framing, representative
ANN recall, recovery performance and total RSS limits remain open.

## M11 representative ANN regression

`TestM11ANNRecallRepresentative` uses Go math/rand seed **1101**, **32 dimensions**,
32 standard-normal centers, 40 near-duplicates per center with independent Gaussian
noise sigma 0.15, and 256 independent standard-normal distractors: **1536 rows**.
Each center supplies one query with independent Gaussian noise sigma 0.08.
All inputs are rounded to float32 and serialized losslessly. Coordinates 9–32
contribute to L2 but are invisible to the existing eight-bit coordinate-sign index.
Near-duplicates cross bucket boundaries: 92/320 unfiltered and 98/320 filtered
exact neighbors have a different signature from their query. This adds difficulty
beyond M9's separated 2D quadrants; M9 remains unchanged and green.

Each of 32 queries runs both unfiltered and with `category = 1` (odd IDs, half the
rows). An independent brute-force float64 L2 calculation over float32 inputs,
rounded to float32 and sorted with ascending PK ties, supplies Top10 truth. On this
fixture all exact-mode ordered IDs agree despite the engine's float32 accumulation.
Recall is intersection size / 10, including when ANN returns fewer than ten rows.
Measured mean/min recall@10: **0.71250 / 0.10 unfiltered**, **0.69375 / 0.20 filtered**.
Each group asserts mean >= **0.60** and every query >= **0.10**: mean headroom is
about 0.09 below the weaker baseline, while the per-query floor rejects total misses.
Validity, uniqueness and predicate compliance are checked separately. A singleton
predicate forces expansion beyond an occupied query bucket, catching filtering
after candidate selection/TopK; an empty predicate result must remain empty.

No runtime/index parameter changes or dependencies were needed. The existing
nearest nonempty Hamming shell can return fewer than K rows and has weak worst-case
recall. These deterministic local regression numbers are **not official metrics**,
not evidence of the stated 0.95 contest recall target, and not a general recall or
performance guarantee. Wider probing would trade more distance evaluations for
recall; higher-dimensional signatures and representative performance remain future
work. The fixed planted fixture does not cover every overlapping distribution.

Validation: `go test ./...`, targeted `M11|ANNRecall|M10|WALChecksum`, and
`./scripts/smoke.sh` all pass, retaining prior smoke checks plus the M11 PASS line.
The existing local public-case harness reports **5/5**, without implementation
changes or stored answers. Root/agent conversation files are identical and below
128 KiB. Upstream remains `a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c` (samples only);
official framing remains TBD and interim `reportReady` is unchanged. No formal
contest submission / 提测. Other open items: automatic checkpoint scheduling,
process locking, recovery performance, total RSS and large statements/results.

## M12 Sign-LSH recall and timing

The self-implemented eight-coordinate Sign-LSH now accumulates complete Hamming
shells through radius 2, then expands until it has min(8*K, table rows) eligible
candidates or exhausts all buckets. WHERE is applied before counting candidates;
full shells avoid truncation bias between equally distant buckets. LIMIT 0 returns
no candidates; queries without LIMIT exhaust the index. Exact mode is unchanged.
No dependencies, signature format, rebuilding, protocol or persistence changes.
M5 assertions that required short ANN results now expect min(K,N), retaining the
nearest-ID, transaction, rollback and recovery checks. M12 adds a filtered fixture
with eligible rows at Hamming distance 8, including K=0 and K>N.

On the unchanged M11 seed=1101, dims=32, 1536-row, 32-query-per-group fixture:

| Recall@10 mean / minimum | M11 baseline | M12 |
| --- | --- | --- |
| Unfiltered | 0.71250 / 0.10 | 1.00000 / 1.00 |
| category=1 | 0.69375 / 0.20 | 1.00000 / 1.00 |

M11 retains its independent exact oracle and now asserts mean >=0.95 and minimum
>=0.90 in each group, plus exactly ten ANN results. Observational SQL timing in a
targeted run (32 queries, excluding mode switches, data/index preparation and oracle):
unfiltered exact **9.769 ms**, ANN **3.400 ms**; filtered exact **6.210 ms**, ANN
**2.308 ms**. Timing uses time.Now around Session.Execute via the test helper; no
wall-clock assertion. Earlier measurement: 10.249/2.858 ms and 5.530/2.736 ms.
These are warm, local timings, not official end-to-end or cgroup measurements.

Wider probing costs candidate offsets, predicate reads and distance evaluations;
selective filters or large K can exhaust the whole index, and completing a shell
can exceed the candidate target. The first eight coordinate signs still ignore
remaining dimensions and vector magnitudes: the planted fixture's perfect recall
does not guarantee 0.95 on other distributions or hidden seeds. No-index ANN keeps
the legacy sampling behavior. Rebuild cost and bounded-cache disk performance
remain unmeasured here.

Validation: go test ./..., targeted M12|M11|ANNRecall|M10|WALChecksum, all prior
smoke checks plus M12, and unchanged local public harness **5/5** pass.
Upstream fetched: **a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c**, samples-only;
framing remains **TBD**, interim reportReady unchanged. No formal contest submit
or 提测. Automatic checkpoint scheduling, process locking, recovery performance,
total RSS and large statements/results remain open.

## M13 recovery and framing watch

Upstream remains `a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c` (samples only).
Official framing is **TBD**; interim `reportReady` is unchanged. Replay now skips
per-statement Sign-LSH rebuilds and constructs buckets in the final pass before
readiness. Live write behavior, checksum verification and durability are unchanged.

A deterministic SIGKILL test recovers 4096 rows from 128 transaction commits, both
WAL-only and checkpoint-plus-redo, with two reopens and full row/index checks.
First local WAL-only observations: **32.70–37.92 ms (108028–125245 rows/sec)** in
memory, **1.217–1.385 s (2957–3365 rows/sec)** with a 256-byte page cache. Snapshot
plus redo measured **30.40–30.52 ms** / **1.002–1.063 s**, respectively. Timing covers
Open through readiness; it has no strict assertion and is not an official score.
See SELFTEST for fixture details and limits. PK validation still scans existing
rows; larger-scale recovery and total RSS remain open. Automatic checkpoints are
not enabled: callers use `Engine.Checkpoint()` with no active transactions.

## M14 automatic checkpoint and RSS observation

Upstream re-fetched: `a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c` remains samples-only.
FRAMING=TBD; interim `reportReady`, stdin/stdout protocol and Dockerfile are unchanged.
No official contest submission or formal 提测.

Automatic checkpoints are now enabled for both library and CLI. Constants
`AutoCheckpointWALBytes = 8 << 20` (8 MiB) and `AutoCheckpointEveryN = 1024`
trigger at either threshold, checked synchronously after each ready-engine statement.
DDL/autocommit writes each count once; a nonempty explicit transaction counts once
at COMMIT. Empty commits and reads do not count. All active transactions defer
maintenance, including a conflicted transaction until ROLLBACK. The last transaction's
COMMIT/ROLLBACK permits maintenance; an idle SELECT can retry pending maintenance.
Recovered WAL bytes and commits are included. There is no timer or background worker.
Constants are the configuration points; no new CLI flags or protocol commands.

The existing M10 checkpoint implementation runs under the engine mutex; explicit
`Engine.Checkpoint()` remains available. Only a successful checkpoint resets both
threshold counters. Snapshot failures log to stderr, retain redo and retry after later
statements without changing the statement result. WAL truncate/sync failures retain
the existing poisoned-writer behavior: reads continue, writes require restart.
WAL is zero immediately after success, but may exceed the byte threshold by one
statement/transaction; active transactions or persistent errors can defer it indefinitely.
Checkpoint latency and temporary snapshot disk space grow with live data.
This supersedes earlier explicit-only / AUTO_CHECKPOINT=no descriptions.

`TestM14RSSObserve` inserts 1024 deterministic 2D vectors in 32 batches, creates an
index and checks exact/ANN Top10, with unlimited and 256-byte row-cache modes.
Linux `/proc/self/status` VmRSS is logged via t.Logf before/after, without cap assertions.
One isolated M14 run observed 15680 -> 15596 kB (unlimited), and 15596 -> 16164 kB
(256-byte cache, peak decoded row bytes 96). These shared-process observations include
prior tests and allocator history, are not leak measurements or official resource scores.
The decoded row-cache budget excludes indexes, references, results and parser buffers;
it is not a process RSS bound. Non-Linux systems log an observation skip.

M14 tests cover commit and byte triggers, explicit transaction completion, multi-session
pins/conflicts, failure diagnostics, idle retry after recovery, and M10 API coexistence.
OPEN_ITEMS: official framing/driver; process locking; large-scale recovery and total RSS;
large statements/results; representative ANN performance and hidden-distribution recall.

## M15 process locking and framing watch

Upstream re-fetched: `a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c` is unchanged,
samples-only. FRAMING=TBD; interim `reportReady` remains unchanged. No official
contest submission or formal 提测.

`Open` / `OpenWithOptions` acquire an exclusive, nonblocking OS `flock` on
`LOCK` under `-data` (default `/data`) before opening WAL, snapshots or segments.
Linux is supported using Go stdlib `syscall.Flock`, with no added dependency.
A competing engine/process fails immediately with a data-directory lock error;
the CLI exits nonzero with `ERROR opening database` on stderr, before readiness.
This is an advisory lock: every writer must honor it, on a filesystem supporting
flock. Do not remove/replace `LOCK` or the data directory while an owner is alive.

`Engine.Close()` serializes with execution/maintenance, disables existing sessions,
and releases ownership; repeated Close is safe. It does not checkpoint or commit
active transactions. Library callers must Close before reopening. The CLI defers
Close on normal EOF; process exit, including SIGKILL, releases the OS lock even
when defers do not run. Startup failures also release it. The empty LOCK file
remains permanently as a stable inode; its existence is not a stale-lock signal,
and no manual deletion is needed after a crash.

M15 tests cover same-directory opens, Close/reopen, closed-session rejection,
failed startup (including recovered storage panic), live subprocess contention and
SIGKILL/restart with committed WAL recovery. Earlier in-process recovery fixtures
now Close before reopening, without introducing checkpoints; existing real crash
tests remain intact. Smoke retains prior checks and adds the M15 PASS line.
This supersedes earlier process-locking OPEN_ITEMS.

OPEN_ITEMS: official framing/driver; large-scale recovery and total RSS;
large statements/results; representative ANN performance and hidden-distribution recall.
