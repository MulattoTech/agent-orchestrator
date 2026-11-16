# Worker admission and offline recovery

Worker-session capacity is separate from provider inference capacity. A 40-slot
fleet preset does not authorize 40 simultaneous model requests. The provider
boundary owns model, reasoning, quota and inference concurrency enforcement.

## Controls

Bootstrap defaults: 40 registered slots, four worker sessions globally, four per
project, durable queue enabled.

| Control | Purpose |
| --- | --- |
| AO_FLEET_PRESET | Registered capacity: 2, 4, 8, 16, 20, 30 or 40 |
| AO_MAX_WORKER_SESSIONS | Global ceiling, 1 through registered capacity |
| AO_MAX_PROJECT_WORKER_SESSIONS | Default project ceiling |
| AO_PROJECT_WORKER_LIMITS | JSON project ID to ceiling overrides |
| UpdateAdmissionLimits(ctx, limits) | Persist validated controls, then publish under the admission lock |
| LoadAdmissionLimits(ctx) | Load durable controls before the daemon accepts requests |
| SetWorkerResourceCeiling(n) | Constrain expansion from measured pressure; zero pauses |
| SetAdmissionDispatchCheck(check) | Non-inference quota/policy observer before queue claims |

Durable settings override bootstrap defaults after an operator update. Lowering
limits never kills workers, archives sessions or removes worktrees. Already
claimed commands can finish. QueueEnabled=false stops future claims and deferrals.

Admission counts nonterminated worker sessions plus in-flight reservations. A
stopped, resumable session still occupies a slot; restarting its known released
runtime must fit current ceilings. The running tool-process count is null when
no separate observer has measured it.

## Scheduling and recovery

Spawn, restore, restore-all and chat-resume paths share reservation accounting.
A durable row and its bound reservation count once. A brief insertion/binding
overlap may conservatively defer a contender; it cannot over-admit. Unknown
session kinds fail closed when admission is configured.

Coordinator reservations are exclusive per project. Workspace ownership is
checked across projects as well as within the existing project workspace gate.
Conflicts preserve the tree. Existing controller/runtime generation fencing
remains authoritative.

Capacity refusal returns SESSION_CAPACITY_QUEUED and admissionRequestId when
queueing is enabled. Commands use immutable payloads and stable client request
keys. Duplicate keys do not create duplicate queue commands. Commands without a
caller key receive a stable key before eventual execution.

The queue admits at most 1,024 pending/running commands. Transactional SQLite
claims rotate projects by latest durable claim time and preserve FIFO within a
project. Each claim advances a generation and records its exclusive daemon owner.
A late completion cannot overwrite a newer owner.

Dependencies must reference existing commands, preventing forward cycles.
Completed dependencies unblock; failed/cancelled ones block dependents. Queued
and blocked commands can be cancelled; already running work remains under its
session lifecycle.

Startup reclaims interrupted claims after exclusive daemon ownership. A committed
launch is recovered through its client request key. Incomplete seeds and ambiguous
cancellation block for reconciliation; they never restart under a new identity.
Only proven operation contention gets one automatic retry with bounded delay.
Capacity/dependency waits do not spend that failure budget. Repeated identical
pause logs are suppressed until state changes.

Queue startup waits for background session reconciliation. A quota observer can
refuse before a claim without changing queue state or entering provider preflight.
The final provider boundary must enforce policy and quota again.

## Offline validation

Targeted tests run the actual Manager, lifecycle reducer and SQLite with synthetic
adapters. Coverage includes seven presets, 64 competing spawns/restores, three
projects, durable controls, queue fairness, cancellation/dependencies, quota pause,
lost acknowledgements, ambiguous starts, ownership conflicts and dirty content.

The Windows fixture starts 40 small standard-library Go helpers and ignores all
configured provider commands. Each helper has a bounded lifetime, an owned process
handle/generation, and only a readiness-file/stdin protocol. The fixture samples
native memory/CPU at each preset, kills one owned child, reopens SQLite, rebuilds
the Manager, reconciles, and resumes exactly one new PID/generation. It verifies
the dirty workspace hash and survival of all other children. Cleanup acts only
on owned process handles.

A packaged companion uses a fixed sibling ao-offline-worker.exe whose SHA-256
digest is linked into the test binary. Source tests compile the same small helper
locally with toolchain/network downloads disabled. Its launcher selects only the
explicit offline test allowlist.

This evidence does not establish real-model throughput or memory capacity. It does
not cover the full AO HTTP daemon, ConPTY UI, detached-host adoption, real provider
billing/refresh, production worktree Git hooks, or deployment. Those boundaries
need separate evidence before rollout.

## Accepted cleanup retry budget

An already-authorized cleanup that reports a transient busy-worktree refusal has
one automatic retry after a deterministic 5–7 second delay. Starting an attempt
and recording its deferred result count once. A second transient refusal records
WORKSPACE_RETRY_EXHAUSTED and stops automatic retries across daemon restarts.
Old pending records already at the budget stop without another removal attempt.
Dirty/locked/unsafe worktrees remain preserved. The owner can explicitly retry
after fixing the external cause; this does not reopen an automatic retry loop.
