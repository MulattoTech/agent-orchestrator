# Public review evidence for #3921 / PR #8456

**Prepared for user review; not posted to issue #3921.** [PR #8456](https://github.com/OrchestratorInc/agent-orchestrator/pull/8456) repairs a reproducible per-project orchestrator Restore admission gap. It does not establish the cause of [the original incident](https://github.com/OrchestratorInc/agent-orchestrator/issues/3921) or resolve the whole issue.

## Scope and evidence classes

`SYNTHETIC_REPRODUCED` identifies a controlled fixture, including one executed natively on Windows. `SOURCE_INFERRED` identifies conclusions from code. `HOST_OBSERVED` applies only to a recorded native observation. `UNVERIFIED` identifies an outcome not established. A PASS is separate from its evidence class.

| Evidence | Exact boundary | Result |
|---|---|---|
| Restore admission — SYNTHETIC_REPRODUCED | Public service regression; fake manager/store; distinct archived orchestrators in one project | Baseline admits 64/64; candidate admits 1/64 and rejects 63 conflicts |
| Native Manager/SQLite — SYNTHETIC_REPRODUCED | Actual Manager/reducer/temp SQLite; fake runtime/workspace adapters; independent 1/4/8/16/32/40/64 worker stages | All stages pass; no final owned fake runtime handles |
| Native renderer — SYNTHETIC_REPRODUCED | Standalone browser dev renderer; upstream fake bridge and mocked page APIs | 64 unique board DOM IDs and two fake SSE moves pass |
| Incident cause / crash recovery — UNVERIFIED | Original macOS incident and a native Windows crash were not reproduced | No causal or crash-recovery claim |

Every fixture above used **zero real agent processes and zero model-consuming test turns**. The two auxiliary fixtures are not part of the public PR and are not an admission or capacity benchmark. Their selected numeric results are in [SYNTHETIC_METRICS.json](SYNTHETIC_METRICS.json); their private raw artifacts are deliberately absent.

## Public regression and source

Pinned upstream source baseline: `53a545ad20fcd300c7c989f175f9a5c5d90fe58e`. Candidate: `3dcec6549cd56d2bd7943f6c18e904ef633058c2`.

**SOURCE_INFERRED:** [Baseline Restore](https://github.com/OrchestratorInc/agent-orchestrator/blob/53a545ad20fcd300c7c989f175f9a5c5d90fe58e/backend/internal/service/session/service.go) bypasses the existing project spawn/replacement gate. [The candidate](https://github.com/OrchestratorInc/agent-orchestrator/blob/3dcec6549cd56d2bd7943f6c18e904ef633058c2/backend/internal/service/session/service.go) shares that gate, checks for another active orchestrator and holds admission through publication. Its scope is one active orchestrator per project. It adds no global worker/model-turn cap and no interruptible mutex wait.

| Concurrent Restore requests | Baseline accepted | Candidate accepted |
|---|---:|---:|
| 1 | 1 | 1 |
| 4 | 4 | 1 |
| 8 | 8 | 1 |
| 16 | 16 | 1 |
| 32 | 32 | 1 |
| 40 | 40 | 1 |
| 64 | 64 | 1 |

The baseline run uses unchanged upstream source plus [the candidate regression fixture](https://github.com/OrchestratorInc/agent-orchestrator/blob/3dcec6549cd56d2bd7943f6c18e904ef633058c2/backend/internal/service/session/orchestrator_restore_test.go). It fails the assertions as expected. The earlier test-only compile typo is excluded from this comparison; the attached corrected baseline is the valid reproduction.

| Attached log | Result / package time |
|---|---|
| [restore-baseline-corrected.log](restore-baseline-corrected.log) | Expected failure, 0.022 s |
| [restore-patched.log](restore-patched.log) | PASS, 0.192 s |
| [restore-race20.log](restore-race20.log) | Linux race detector, 20 repetitions, PASS, 7.673 s |
| [windows-restore-repeat.log](windows-restore-repeat.log) | Native Windows x64, 20 repetitions, PASS, 3.292 s; no race detector |

Coverage includes both Restore/Spawn orderings, clean replacement, TUI/chat targets, other projects/workers, restoration after a newer orchestrator terminates, same-live/missing-session error compatibility, store failures, cancelled requests and gate release after launch failures. Restore of the same already-live session retains `SESSION_NOT_RESTORABLE`. This repair is distinct from the adjacent conversation-ownership rules in [#7387](https://github.com/OrchestratorInc/agent-orchestrator/pull/7387).

Replay the focused scope from the candidate's `backend` directory with Go 1.27.4:

```sh
go test -p 4 ./internal/service/session -run '^TestRestoreOrchestrator' -count=1 -v
go test -p 4 -race ./internal/service/session -run '^TestRestoreOrchestrator' -count=20
```

For the baseline comparison, place only the linked regression fixture in a separate checkout of the pinned upstream baseline revision. Windows used 20 repetitions without `-race`; a supported CGO C compiler was unavailable.

## Auxiliary fixture limits and incomplete gates

The native Manager fixture passed in 5.986 s. It checks one additional failed launch per stage, dirty-sentinel preservation, owned fake-handle cleanup and an explicit restore after a clean SQLite close/reopen with retained fake adapter state. This does not exercise real Git dirty detection, process trees or an OS crash. Go heap figures cover the whole test process; retained disk bytes are final usage, not peak. There are 165 accepted slots summed across independent stages, with a maximum stage of 64.

The separate Windows browser fixture passed one test in 10.029 s using the [upstream fake bridge](https://github.com/OrchestratorInc/agent-orchestrator/blob/53a545ad20fcd300c7c989f175f9a5c5d90fe58e/frontend/e2e/support/fake-bridge.ts). Its 266 page API requests were mocked. No real daemon was called; no uncaught page exception occurred. Mixed-export and undefined-query-data warnings remain. The result is a scoped board assertion, not full renderer/Electron E2E or a packaged 64-agent board. Screenshots are excluded.

The numerical auxiliary results above retain their historical local-source identities (`1ef31a7955bb` / `1810dcde8e8c`). The final owner-local package source is `5a34a629f75b446b653e387e454bfd1b70658036`; earlier packaging-only `45d677e6ecc9` is an intermediate revision, not the final delivered source. None of these local packaging changes is part of PR #8456.

**HOST_OBSERVED / limited final package observation:** The final `5a34a629f75b` package passed isolated startup, health/readiness checks, and inspection of its actual empty renderer and About dialog. It reported zero projects/sessions and closed its owned app, daemon and test listeners. This is a separate package observation, not the 64-worker browser fixture or a 64-provider workload. The packaged Electron `better-sqlite3` binding is present; successful native Database construction and an in-memory SQL query remain **UNVERIFIED** because they were not separately executed. The Go Manager/temporary-SQLite fixture does not test that Electron native binding. Private raw host evidence and screenshots are excluded from this export.

This export does not claim complete repository validation: the Linux whole-backend race run was killed, and the full Windows service suite has three failures reproduced on baseline (CRLF, permission-fixture behavior and symlink privilege). Windows race detection remains unrun. Bounded auxiliary Linux fleet race tests used the [CI modernc exemption](https://github.com/OrchestratorInc/agent-orchestrator/blob/53a545ad20fcd300c7c989f175f9a5c5d90fe58e/.github/workflows/go.yml), `-gcflags=modernc.org/...=-race=false`; AO code remained instrumented. These qualifications do not convert incomplete suites into passes.

## Redaction and integrity

This is an explicit allowlist export, not a copy of the private evidence directory. The first three focused logs are exact original bytes after content review. The Windows log retains its single result line, removes 24 dependency-download lines, and normalizes CRLF to LF. No failure/assertion line was removed from the focused baseline or candidate logs. Public Go module paths and test names are preserved.

The metrics JSON is reconstructed from selected numerical/boolean fixture fields and scope descriptions. Host/profile/scratch paths, host/account identifiers, PIDs, ports, wall-clock times, production logs, databases, credentials, quota data, owner inventory, private URLs and all screenshots are excluded. [SHA256SUMS.txt](SHA256SUMS.txt) hashes every exported payload file, excluding the manifest itself. Hashes establish byte identity, not independent reproduction.

[ISSUE_3921_COMMENT_DRAFT.md](ISSUE_3921_COMMENT_DRAFT.md) is an unchanged copy of the proposed comment. Its separately labeled limited native package-smoke paragraph has no raw host evidence in this export; the focused logs must not be treated as proof of that paragraph. Nothing here requests closure of #3921.
