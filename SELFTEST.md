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
