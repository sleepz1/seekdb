# M5 SQL subset self-test

Baseline: `0c33e8fe7c5b7c9b10ec6ae4487f2db21f10df36` (M4), branch
`2026-competition-round2`. No reset/rebase, formal submission or PR merge.

Implementation followed the required sequence, with `go test ./...` passing
after each stage: (1) explicit PK / CREATE constraints, (2) atomic multi-row
INSERT / quoted vectors / UPDATE and DELETE, (3) comparisons / projections /
sorting, (4) self-built vector index.

Final checks, all exit 0:

```sh
go test ./...
go test -race ./...
./scripts/smoke.sh
cmp conversation.json agent/conversation.json
git diff --check
```

Smoke retains the M3 SIGKILL-after-acknowledgment recovery, immutable snapshot,
COMMIT/ROLLBACK/EOF isolation, flush, errors and continuation checks. M4 int64,
Unicode VARCHAR, finite float32/dimensions, filtering, tie ordering and search
mode checks pass; fixtures now declare explicit BIGINT PRIMARY KEY.

The new M5 stdin section runs the Yuque 2.2 example with INSERT and SELECT each
on one line under existing newline framing. It verifies projected content and
L2 scores, UPDATE/DELETE rollback, duplicate-PK multi-row statement atomicity,
all comparison operators, vector-index ANN/exact and restart from WAL.

Go tests additionally cover missing/multiple/nonconforming PK constraints,
multiple vectors, quoted vectors, multi-row validation failures, forbidden PK
updates, update assignment validation, affected counts, predicates shared across
SELECT/UPDATE/DELETE, explicit PK tie ordering when another BIGINT comes first,
aliases with/without AS, ordinary ordering, invalid queries and literal WHERE
inside UPDATE text. Index tests distinguish bucket candidates from the old
subsample scan, verify exact bypass, filter-before-candidates, transaction
insert/update/delete visibility and rollback, empty-shell fallback, duplicate
index names, wrong columns/tables, committed mutations after WAL replay, and
PK/index metadata plus buckets after snapshot recovery.

All packages pass normal and race tests. No new dependencies, forbidden engines,
secrets or binaries. Optional MySQL wire implementation was not expanded; its
test fixture now declares a PK.

Temporary protocol / open items:

- Official framing, ready flag/handshake, numeric error codes and ANN recall
  requirements: **待确认**, official pack unpublished.
- Current stdin framing: semicolon outside quotes or newline outside quotes and
  balanced parentheses/brackets; arbitrary multiline clauses must be flattened.
  SQL responses flush immediately; stdout TSV + OK/ERROR, stderr logs.
- Sign-LSH probes the closest nonempty filtered Hamming shell. ANN may return
  fewer than k rows; exact remains brute-force exact. No recall guarantee.
- Whole-state clones and index rebuilds are deliberately simple; performance,
  checkpoint/compaction, bounded WAL growth, WAL checksums and process locking
  remain open. Only one Engine/process may own a data directory.
- CREATE now requires an explicit BIGINT PRIMARY KEY. Legacy snapshot tables
  without that metadata remain readable; pre-M5 WAL CREATE statements lacking
  PRIMARY KEY are not migrated by this milestone.
- Docker runtime checks were not rerun. No contest submission or PR merge.


## M7 engineering hardening

Baseline: `f724f01d8059a1b06b7240ce94b3ae77f8f5f8ff` (M6). This section
supersedes the M5 whole-payload clone and missing ready-gate limitations above.
`-memory-budget=512` selects a tiny decoded payload cache; CLI default is 64 MiB,
while `Open` and explicit zero retain unlimited compatibility. Immutable row pages
and pending transaction SQL live under `<data>/segments/`. Recovery streams the
snapshot/WAL to a fresh page generation, then rebuilds all index buckets before
readiness. The cache budget excludes active rows, SQL, output and metadata; an
oversized row is read uncached. Compaction and recovery speed remain open.

`go test ./...` covers 160 rows exceeding a 512-byte cache, asserts nonzero bounded
resident/peak counters and empty `Table.Rows`, checks transaction SQL spooling,
cold filtered TopK in exact/ANN modes, no-close WAL recovery, mutation commit and
rollback, failed multi-row statement atomicity, streaming legacy snapshots, and
page I/O errors without acknowledging failed mutations. `TestM7ReadyGate` observes
not-ready, checks Execute rejection, erases replay buckets and verifies the final
startup rebuild restores ANN results. Existing unlimited tests remain enabled.

Smoke adds a real SIGKILL after COMMIT, reopens with the same 512-byte budget,
verifies 160 rows and indexed filtered TopK, and checks `minivectordb: ready` only
on stderr. Official ready framing is **待题包确认**; stdout still flushes each
statement result. New smoke output:

```text
PASS: M7 beyond-memory 512-byte cache, 160 rows, cold filtered TopK, SIGKILL + WAL/index recovery (resident bounds asserted in Go tests)
PASS: M7 ready-gate rejection/rebuild unit test, stderr ready handshake, clean stdout
```

Validation: `go test ./...`, `go test -race ./internal/db/... ./cmd/minivectordb/...`,
`./scripts/smoke.sh`, conversation byte identity/size checks and `git diff --check`.
No Docker EXPOSE, wire expansion, external engine, formal submission or 提测.

## M8 compaction, stress and budget boundary

Baseline: `499c315e71d509af112fd553de9d8e137f5af76f` (M7).
Fetched upstream `2026-competition-round2` before coding:
`c842a368c3394748d0499386c18f6b465242e141`, unchanged scaffold.
The **interim** stderr-only ready line remains `minivectordb: ready`;
official framing is **待题包确认**. `cmd/minivectordb/main.go:reportReady`
is the one-place string/file/protocol adapter. Open returns only after recovery,
startup compaction and index rebuild. TestM7ReadyGate still tests rejection.

M8 supersedes earlier segment-compaction limitations: `Engine.Compact` streams
live page bytes to a synced replacement, publishes refs without changing index
ordinals, removes the old file and syncs the directory. Transactions pin refs;
Compact rejects active transactions. Automatic compaction after COMMIT/writes
requires >=1 MiB and >50% dead bytes; it skips active transactions. Failed copy
leaves references unchanged. Automatic errors cannot invalidate durable COMMIT;
explicit Compact reports errors. Recovery compacts and cleans abandoned segment
and spool generations before readiness. Snapshot + WAL stay authoritative.

Budget policy **exclude**: decoded row page cache only, excluding active/oversized
uncached rows, SQL/parser/copy buffers, returned result sets, schema, page refs,
PK validation keys, index buckets and ordering keys. This is not process RSS.
Tests retain 500 results while checking bounded counters, read a 4096-byte row
uncached under 128 bytes, and verify subsequent cache reuse and scan eviction.

Validation commands:
```sh
go test ./...
go test -count=1 ./internal/db/ -run 'M7|M8|Compact|Budget|Stress' -v
./scripts/smoke.sh
git diff --check
```
Stress uses 1000 rows, a 256-byte budget, three UPDATE/DELETE/reinsert COMMIT
cycles, explicit compaction, PK/count and exact/ANN filtered TopK assertions,
and abrupt engine abandonment with an uncommitted delete followed by same-budget
reopen. It logs elapsed time and all Resident counters. Separate tests cover the
automatic threshold, transaction pins and failed-copy publication. Smoke retains
all earlier PASS lines and adds M8 compaction and budget-boundary PASS lines.

OPEN_ITEMS: official ready/error/result framing; WAL checkpoint/compaction and
checksums; total RSS/large SQL/results; replay performance; process locking;
ANN recall/performance. Segment compaction does not bound WAL or peak temporary
disk space. No Docker changes, formal contest submit or 提测.

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

## M13 recovery performance and framing watch

Fetched upstream `2026-competition-round2`: **a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c**,
unchanged samples-only tip. Official framing remains **TBD**; interim `reportReady`,
stdin/stdout and Dockerfile remain unchanged. No formal submission / 提测.

WAL replay now defers derived Sign-LSH bucket rebuilds until the existing final
pre-readiness pass. Previously every replayed mutation rebuilt all indexes over
all accumulated rows. SQL validation and exact mutation semantics do not depend
on buckets. Live writes retain their existing rebuild behavior. Checksum validation
still precedes application with two streaming passes; fsync and checkpoint ordering
are unchanged. This removes repeated index scans during replay, not all quadratic
work: per-INSERT PK validation still scans prior rows. Snapshot decoding may also
construct indexes before the final pass. No before/after speedup is claimed.

`TestM13Recovery` writes 128 real transaction commits of 32 rows (4096 total), with
an index created before inserts, then commits a vector update and delete/reinsert.
A subprocess is SIGKILLed with an uncommitted full-table delete. Four configurations
cover unlimited memory / 256-byte decoded page cache and WAL-only / checkpoint
halfway plus redo. Each reopens twice, checks every ID, payload and vector, every
bucket's membership/uniqueness/coverage, exact and ANN nearest results, the readiness
gate, and cache accounting. WAL-only recovery also checks buckets were not built
before the final pass. M10 retains corruption/publication and bounded-growth tests;
M11/M12 retain exact oracle and recall floors.

First isolated local run (Open through readiness, including final index rebuild
and bounded-mode compaction; two observations per configuration):

| Cache budget | Recovery source | WAL bytes | Duration | Rows/sec |
| --- | --- | ---: | --- | --- |
| unlimited | WAL | 362619 | 32.70–37.92 ms | 108028–125245 |
| unlimited | snapshot + WAL | 183063 | 30.40–30.52 ms | 134188–134757 |
| 256 bytes | WAL | 362619 | 1.217–1.385 s | 2957–3365 |
| 256 bytes | snapshot + WAL | 183063 | 1.002–1.063 s | 3854–4089 |

Rows/sec counts all 4096 recovered live rows, including snapshot rows. Fixture
creation and verification queries are excluded. Timings are observational, with
no wall-time assertion; OS cache, storage and host load affect results. These are
local regression measurements, not official recovery scores or an RSS guarantee.

Automatic checkpoint scheduling remains disabled: `Engine.Checkpoint()` is explicit
and rejects active transactions. No new knobs. Validation: `go test ./...`, targeted
`M13|M12|M11|ANNRecall|M10|WALChecksum|Recover`, all prior smoke lines plus M13,
and unchanged local public-case harness 5/5. Dual conversation files remain identical
and <=128 KiB. OPEN_ITEMS: official framing; automatic checkpoint scheduling;
process locking; PK-validation and bounded-page replay performance at larger scale;
total RSS/large statements/results; ANN generalization beyond fixed fixtures.

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

## M16 RSS, recovery scale and ANN generalization

Upstream re-fetch remains `a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c`:
samples only, FRAMING=TBD, interim `reportReady` unchanged. No formal submission.
No runtime change was needed: WAL already streams records, checkpoint writes rows
incrementally, and the decoded cache is bounded. M12 probing and M15 flock remain.

`TestM16RSSRecoveryScale` uses 16,384 rows, 32D vectors and 640-byte text in
64 batches of 256 rows, unlimited and 4096-byte cache modes. The real 8 MiB WAL
threshold automatically produces snapshot LSN 45; final version 67 leaves
3,885,035 bytes of redo including index creation and a payload update. Close with
an uncommitted DELETE, then two reopens verify every scalar/vector, all bucket
offsets exactly once, LSNs and filtered exact/ANN singleton queries. Close does
not checkpoint; this is a scale/reopen test, while M13/M15 retain real SIGKILL.
First isolated run, Open through readiness (no wall-time assertions):

| Cache budget | Recovery durations | Rows/sec | VmRSS before / loaded / reopened (kB) |
| --- | --- | --- | --- |
| unlimited | 486.06 / 486.31 ms | 33708 / 33690 | 5076 / 52656 / 124940, 109676 |
| 4096 bytes | 4.045 / 4.198 s | 4051 / 3903 | 109680 / 15380 / 15612, 15720 |

Peak decoded cache was 888 bytes in bounded mode. VmRSS is sampled with t.Logf,
not capped; shared-process GC/allocator history explains why the second mode
starts high. These are observations, not isolated peak-memory comparisons.
The decoded row/page cache budget excludes active rows, SQL/parser buffers,
results, references, indexes, Go heap slack and runtime overhead. Kernel filesystem
page cache also consumes host/container memory and is distinct from process VmRSS;
neither cache budget nor these samples establish a total memory ceiling.

`TestM16ANNGeneralization`: seed 1601, 2048 independent isotropic Gaussian
64D rows, 32 independent queries, k=10; no planted neighbors. Unfiltered mean/min
recall is 0.29688/0.00; category=1 (one third eligible) is 0.28438/0.00.
All 64 exact results match an independent full-sort L2 oracle using documented
float32 accumulation and PK ties. ANN asserts cardinality, unique valid IDs and
predicate membership. A conservative mean >=0.20 regression floor leaves headroom
below both measured means; minimum is logged, not guaranteed. Eight coordinate
signs lose substantial information in 64D. This is evidence against generalizing
M12's near-duplicate recall to arbitrary data, not a contest 0.95 claim.

Validation: go test ./..., requested M10–M16/recovery/checksum/lock selection,
M1–M16 smoke, unchanged public sample harness 5/5, dual conversation equality and
128 KiB limit, and git diff --check. No public sample rewriting or prestoring.
OPEN_ITEMS: official framing/driver; larger recovery and peak RSS profiling;
large statements/results; weak ANN recall on independent high-dimensional data.

## M17 Gaussian64 ANN recall

Local Sign-LSH tuning expands the mandatory complete Hamming shells from radius
2 to 4 (163 of 256 eight-sign buckets). The existing post-filter target of 8*K,
overflow cap and further expansion through radius 8 remain. No external ANN
library, new hash format, exact-mode change or protocol change was introduced.

The unchanged M16 fixture uses seed 1601, 2048 independent Gaussian 64D rows,
32 queries per group and K=10. M16 still runs; M17 shares its data generator and
independent float32 full-sort exact oracle, adding stronger recall and candidate
subset assertions:

| Query group | M16 before mean/min | M17 after mean/min | Candidates per query |
| --- | --- | --- | --- |
| Unfiltered | 0.29688 / 0.00 | 0.84062 / 0.60 | 1255–1348 of 2048 |
| category=1 | 0.28438 / 0.00 | 0.81250 / 0.50 | 409–457 of 683 |

M17 asserts mean >=0.75 and minimum >=0.40 for both groups, with headroom below
these measurements. Every query asserts unique, predicate-matching candidates
between K and 75% of eligible rows: distance ranking still uses an ANN subset.
M12's unchanged representative data remains mean/min **1.00/1.00** in both groups;
its regression now requires those exact values. Exact mode still matches the oracle.

This trades more distance evaluations for recall: roughly 61–67% of eligible rows
are scored here. It is not a speedup claim. Eight coordinate signs still omit most
64D information; no universal recall floor or official contest score is claimed.
Radius 4 meets the local target, so wider shells were not adopted merely to approach
an exhaustive scan. Sparse predicates, small datasets, large K or no LIMIT can
exhaust all buckets under the existing expansion rules; there is no new exact
fallback. Index storage and rebuild work are unchanged.

Upstream remains `a3dba559c1e7ebdc48bc990ffb1a4c75d1c1075c`, samples-only.
FRAMING=TBD; interim `reportReady`, stdin/stdout, `-data` and Dockerfile remain.
Validation: go test ./..., requested M17/M16/M12/M11/ANNRecall selection,
M1–M17 smoke, unchanged public mysqltest samples 5/5, identical conversation files
within 128 KiB, and git diff --check. No official submit or formal 提测.
OPEN_ITEMS: official driver/framing; broader ANN distributions and recall/cost tuning;
larger recovery and peak RSS profiling; large statements/results.
