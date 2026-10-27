# seekdb failed submission repair implementation plan

> **For agentic workers:** Execute inline using executing-plans; independent read-only diagnostics use dispatching-parallel-agents. Track every verified result below.

**Goal:** Repair genuine seekdb deployment UUID verification and compile/runtime defects blocking contest group 600069, then submit verified artifacts via official UI.

**Architecture:** Keep the existing real deployment identity for the first controlled UUID verification after the successful raw telemetry submission. For AI/file SQL, isolate branch contest/sdb04-ai-repair from 8d1d820c17c40f7b5879ae40a395b0328e0f1daf, use GitHub Actions Linux for compilation and real SQL runtime tests. Do not overwrite other branches or merge main.

**Tech Stack:** seekdb C++/Rust/CMake, Linux, GitHub Actions, Python mock HTTP and SQL regression harness, official OceanBase browser UI, Notion evidence ledger.

---

## Task 1: Reproduce build failure

- [x] Read repo AGENTS.md, upstream build documentation, PR2 and Notion handoff.
- [x] Create isolated branch from exact submitted SHA.
- [x] Add read-only contest build workflow; isolated trusted source-preparation job commits the reviewed patch without executing repository code with its write token.
- [x] Baseline full Release build reproduced missing expression header at ob_expr_ai_embed.cpp:21; actual exit2 and logs retained in run40436271197.
- [x] Confirm rerank ObJsonNode keyed get_value failures before changing production source.

## Task 2: Fix compile boundary

Files: src/sql/engine/expr/ob_expr_ai/ob_expr_ai_rerank.cpp; established unittest/sql/engine/expr regression harness.

- [x] Add focused regression covering real object keyed access through cast under existing J_OBJECT checks.
- [x] After recorded compiler failure, cast JSON node pointers to ObJsonObject for string-key access; preserve base-node methods.
- [ ] Re-run compile. Address further actual compiler diagnostics sequentially, with failing compilation evidence per defect.

## Task 3: Prove AI SQL behavior

Files: unittest/sql/engine/expr/test_ai_contest_runtime.py and existing AI implementation files only for demonstrated failures.

- [ ] Start the actual newly built seekdb against an isolated data directory and Python HTTP mock, using disposable localhost-only credentials.
- [ ] Assert SQL AI_PROMPT repeated/escaped/nested templates and LONGTEXT.
- [ ] Assert AI_COMPLETE valid config, SQL NULL config, rejected empty/null/numeric/object completions against actual JSON parsing.
- [ ] Assert AI_EMBED 65 distinct vectors across batches, reverse response indices, duplicates and NULL/filter/LIMIT mapping, dimension failures.
- [ ] Assert Ollama /api/generate and /api/embed request/response mapping without an access key.
- [ ] Assert AI_RERANK 41 and 141 inputs, score ties/global IDs, string and object inputs, incomplete batch rejection, consistent unscored handling.
- [ ] Observe each new runtime regression fail before minimal production fix, then run combined suite.
- [ ] Run existing file_sql mysqltest and AI mysqltest with the compiled binary, preserve diagnostics and ensure previous file capability remains functional.

## Task 4: UUID

- [x] Verify login and current 600071 records before any submission.
- [x] Submit existing actual UUID bf6e6fc4-1e23-8686-85e2-93ed3f89ad06 once after successful previous raw POST and overnight indexing interval; do not repeat blindly.
- [ ] Only if failed, use findings to test one real official-image deployment in controllable cloud environment, retain generated identity and native transport evidence; never edit payload identity/reporter to claim success.
- [x] 2026-10-27 10:07 official verification: original UUID accepted; 600071 scores30.000, succeeds3/fails0, attempts9. Deployment10/10 resolved.

## Task 5: Review and submission

- [ ] Independent review of final production diff and focused tests, follow .agents/skills/code-review/SKILL.md.
- [ ] PR describes verified compile and runtime evidence, exact SHA and unresolved checks; attach it to this chat.
- [ ] Submit green exact branch/SHA to official600069 UI once; read receipt before any retries.
- [ ] Read official results and repair material failures within authorized scope.
- [ ] Update Notion task/subtask rows, submission channel and index with observed official status, scores and timestamps. No guessed score increments.

## Verified checkpoint — 2026-10-27 Asia/Shanghai

Header-path repair and genuine production-Unity compile probe are committed at `9d01cff86525bcdc3144b6b300a392bb279ddf0c`. Now compile that revision and run actual SQL regressions; runtime and official600069 results are still pending. No score claimed for AI repair.

## Compiler regression repair checkpoint

Run40442385585/job121985512738 compiled the genuine production AI Unity source and reported seven invalid string-key get_value overloads. Under existing J_OBJECT guards, those seven accesses now cast to ObJsonObject at 456edfe70a36f4042eeaa321b033c6079061e0d1. Source-preparation run40463717855 succeeded. Also restored required license comments on two inherited C++ files without changing test behavior. Native recompilation, full binary and real SQL regressions remain pending.

The active branch is contest/sdb04-ai-repair and review continues in PR5. The previous draft PR4 is closed with a link to PR5. The official test wrapper uses unchanged fixtures/results; official SQL tests still execute after a failing runtime regression if instance startup succeeded.

## File SQL compiler regression checkpoint

Full Release run40463906663 reached96% and natively failed at ob_dml_resolver.cpp:11165 and ob_select_resolver.cpp:4723: get_bool_sys_var is private. The existing public bool overload get_sys_variable(ObSysVarClassType,bool&) is declared in ObBasicSessionInfo. Source commit bb2ccd2e826c6cc4b92ef4e1ddce9e9437c5ef28 changes only those two calls to the public interface. Both resolver files belong to the same original production Unity action, now included in the early native syntax gate. Prior AI production Unity passed; runtime remains unexecuted until a complete link succeeds. Initialized dependencies and compiler cache were saved by the real failed run.
