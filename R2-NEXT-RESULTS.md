# R2 next improvement: compiled VARCHAR equality

Starting point: `e12768530c91fbce87a4773c75ba13666b4671fc`, branch
`contest/r2-next-from-e12768530`. Local work on 2026-11-10; no push or tip.

## Decision

Keep the performance hypothesis, choosing a small exact-search improvement.
The vector index name is validated and persisted but does not select a search
structure. ANN continues to use exact search. A sequential TopK CPU profile
at the starting code attributed 19.05% cumulative samples to `cmpE.eval`.
Compile VARCHAR-column/string-constant equality, in either operand order,
into a direct string equality expression after existing type validation.
This removes generic Value comparison dispatch on the common equality filter.
Other comparisons retain their evaluator. No storage, protocol, distance,
heap, visible-row scan, fsync, or worker policy changes.

Distance-loop flattening, explicit unrolling, and full-distance evaluation
were tried and discarded because measurements did not improve. Reducing
workers to roughly one per 4096 rows was also discarded: query time increased
to 536–548 ms versus 248 ms baseline on this eight-CPU host. These experiments
are absent from the final diff. No speculative hidden-case changes or ANN.

## Verification

Builds (Go 1.27.3 linux/amd64, stdlib only):

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /tmp/minivectordb-r2base ./cmd/minivectordb
# Above at starting HEAD, before edits. Below with final engine code:
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /tmp/minivectordb-r2next ./cmd/minivectordb
```

Docker daemon is unavailable; official checks use local argv, without Docker
resource isolation. The read-only f4d4e266 copy has only mysqltest/README.md,
not its manifest/cases. The first default public invocation failed before
running cases with FileNotFoundError for manifest.json. The successful run
explicitly selects the available official minivectordb-public-v4 package:

```sh
python3 /workspace/ob-jiasai-prep/upstream-f4d4e266/protocol_check/run.py --command '["/tmp/minivectordb-r2next","-data","{data_dir}"]' --handshake-only --output /workspace/ob-jiasai-prep/protocol-results/handshake-codex-20261110-str-equality-1806
python3 /workspace/ob-jiasai-prep/upstream-f4d4e266/protocol_check/run.py --command '["/tmp/minivectordb-r2next","-data","{data_dir}"]' --package /workspace/ob-jiasai-prep/upstream-9f6df2ba/mysqltest --output /workspace/ob-jiasai-prep/protocol-results/public-codex-20261110-str-equality-v4-1810
```

Both exit 0, result.json passed=true: handshake 1/1; public 20/20 (95 SQL).
Public checks cover restart, index mutations, exact/ann switches, BIGINT,
float32 vectors, filtered ties, failed-statement atomicity, shutdown rollback.

`go test ./...` and `go test -race ./...` pass. Untracked tools/ initially
prevented ./... from building (obsolete internal/db import). For these tests,
that directory was temporarily moved to /tmp/r2next-untracked-tools-20261110
with an EXIT trap restoring it; restored unchanged and left untracked.
New tests compare the optimized expression with the generic evaluator across
operand reversal, empty strings, Unicode, case, trailing spaces, AND/OR/NOT,
TopK ties, disk-only/cache paths, transaction mutations/rollback and restart.
An initial test incorrectly expected boolean-to-boolean comparison to succeed;
it now verifies the existing type error, without changing engine semantics.

```sh
python3 scripts/crash_test.py --bin /tmp/minivectordb-r2next --rounds 20
```

PASS: 20 SIGKILL rounds, 10262 committed rows verified.

## Local performance, not official scores

Intel Xeon host, affinity 0–7, Go 1.27.3; no cpu.max or memory.max at standard
cgroup-v2 paths. scripts/bench.py uses legacy --format json, exact mode,
seed 1, 10000 rows × 128 dimensions, 100-row durable multirow batches,
1000 K=10 queries (half cat equality filtered), 20 warmup queries.

Initial unrestricted baseline: write 364 ms, query 251 ms, RSS 26.2 MiB,
data 5.24 MiB. Unrestricted end-to-end differences were small/noisy.
For a two-CPU relative comparison, alternate these commands three times:

```sh
taskset -c 0,1 python3 scripts/bench.py --bin /tmp/minivectordb-r2base
taskset -c 0,1 python3 scripts/bench.py --bin /tmp/minivectordb-r2next
```

| Metric | Baseline runs | New runs | Baseline / new median |
| --- | --- | --- | --- |
| Write ms | 332, 378, 324 | 319, 320, 294 | 332 / 319 |
| Query ms | 462, 467, 504 | 416, 396, 420 | 467 / 416 (10.9% less) |
| Peak RSS MiB | 23.1, 22.9, 23.5 | 23.1, 23.3, 23.9 | 23.1 / 23.3 |
| Data MiB | 5.24 each | 5.24 each | unchanged |

CPU affinity is not a Docker CPU quota. Write differences are noise: the
optimization changes comparison evaluation, not insertion or fsync. File
size is not measured disk I/O B; RSS is not container peak M.

Sequential engine benchmark (Workers defaults to 1 in this benchmark):

```sh
# Baseline test binary built before final change, with CPU profile:
go test -run '^$' -bench TopK -benchtime 3s -cpuprofile /tmp/r2-cpu.prof -o /tmp/r2-engine.test ./internal/engine/
go tool pprof -top /tmp/r2-engine.test /tmp/r2-cpu.prof
/tmp/r2-engine.test -test.run '^$' -test.bench TopK -test.benchtime 3s -test.count 3
go test -run '^$' -bench TopK -benchtime 3s -count 3 ./internal/engine/
```

Baseline ns/op: 809057, 804941, 831608; new: 638143, 643402, 635096.
Median reduction 21.1%. This microbenchmark does not configure production
parallel workers; it is a separate relative indicator.

## Limits and next action

No claim about unpublished formal 29 cases or performance/Agent scores.
No ANN recall experiment is needed for a new approximation: no approximation
was introduced. Existing randomized reference and parallel-equivalence tests
pass, but are not official recall scoring. Gains depend on VARCHAR equality
filters; integer filters and other predicates do not gain this specialization.
Go 1.22 Docker build/runtime remains unverified here. conversation.json adds
truthful agent/tool evidence and a labelled excerpt of the actual user request;
it does not claim new student understanding or student-directed experiments.
Wait for e12768530's formal score and boss approval before any push/tip.
