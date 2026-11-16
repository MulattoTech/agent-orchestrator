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

## Read-only controller health

HealthSnapshot(ctx) separates durable occupancy from live controller/process
evidence. It probes only the stored runtime handle with its exact launch
generation, rechecks durable ownership after observation, uses at most four
observers and a two-second overall budget, and never starts or reconnects a
provider. Missing children make the aggregate DEGRADED while SQLite/API reads
can still succeed. Identity mismatches and failed probes stay UNKNOWN.

Chat's local event-controller registry can prove an attachment or its absence,
but does not prove the native process/descendant tree. The snapshot preserves that
distinction. verifiedRuntimeProcesses is a measured lower bound for owned TUI
runtime children; runningToolProcesses remains null without a descendant
observer. Intentionally stopped, hibernated and provisioning sessions are shown
separately. The native Windows fixture verifies HEALTHY changes to DEGRADED
after one owned child exits, before any reconciliation changes durable state.

Queued restore identity uses CleanupGeneration, the monotonic terminal/restore
episode counter. Renaming or reconciling metadata while a restore waits does not
invalidate it. An intervening restore followed by a new archive changes that
generation, so the earlier queued restore blocks instead of reviving work the
owner subsequently stopped.

Workspace owner comparison resolves existing filesystem ancestors and checks
native directory/file identity, including Windows junctions and case aliases.
A future directory beneath an alias is fenced by its existing ancestor identity
plus relative suffix. Unreadable, cyclic or dangling alias identity refuses
admission with WORKSPACE_OWNERSHIP_UNKNOWN. Restore checks ownership before
workspace mutation as well as immediately before controller launch.

## Explicit PR completion dependencies

POST /api/v1/sessions accepts two separate optional dependency lists:

- admissionDependencies names existing durable provisioning commands. Completion
  means their spawn or restore command committed; it says nothing about the work.
- prCompletionDependencies names an exact sessionId, canonical prUrl and full
  expected headSha. This is a PR completion dependency, not requirements acceptance.

A PR-dependent command is always durably queued before worker admission. The
queue reads the existing SCM observation store; it performs no GitHub request or
model turn. It requires the exact session, canonical URL and head SHA, Merged=true,
a full MergeCommitSHA, a provider merge timestamp, and provider/host/repository/
provider-ID provenance. ObservedAt must be within 15 minutes, with one minute of
clock-skew tolerance. Missing, mismatched, non-merged or stale facts keep the
command queued as PR_COMPLETION_PENDING with zero failure-attempt consumption.
Ten-second readiness checks do not block other projects' eligible commands.
An SCM refresh can release the immutable command once the required proof exists.
The queue status includes its explicit PR criteria but keeps its payload private.

A completed PR-dependent spawn replays through the same durable client request
identity after restart; another queue tick or repeated request does not launch a
second worker. Idle, archive, comments, review prose and task status never imply
that a PR dependency succeeded. Queue-disabled requests refuse instead of
bypassing the dependency. A provider observation is an input to this gate; this
feature does not itself refresh stale observations or approve a merge.

Offline tests use synthetic provider facts committed through the real SCM store,
including SQLite close/reopen, 18 incomplete-observation cases, independent-project
fairness, and one dispatch after an exact merged fact. Owner-approved artifact
acceptance and full requirements DAGs still require a separate authoritative
outcome interface. Global CI/test/tool subprocess semaphores are not implemented
by worker admission or the inference gate.

## AO-owned workspace shell admission

Setup and Cleanup commands now acquire a separate permit immediately before the
shell is constructed and cmd.Run executes. The default is two owned shells across
this daemon process and one per project. AO_MAX_WORKSPACE_SCRIPTS and
AO_MAX_PROJECT_WORKSPACE_SCRIPTS accept integers 1 through 40; the project value
must not exceed the global value. Invalid controls fail closed with
WORKSPACE_SCRIPT_POLICY_INVALID before any shell starts. These controls are
independent of worker slots and simultaneous inference. All Manager Setup paths
and the managed Cleanup path supply the trusted project identity.

Waiters honor context cancellation without reserving a global slot while their
project is full. Success, start failure, nonzero exit and cancellation release the
permit once cmd.Run returns. The gate never retries an arbitrary side-effecting
command. Setup already stops on its first failed step. The accepted workspace
cleanup operation has its separately recorded bounded retry policy described
above; the shell admission layer adds no retries.

WorkspaceScriptSnapshot() exposes active/waiting owned shells, global/project
limits and project occupancy. DescendantProcesses remains null. This is a cap on
AO-owned workspace shell invocations in one daemon, not a general CI/test resource
semaphore. Provider-native commandExecution notifications arrive after Codex owns
the execution; optional approval messages are not a universal dispatch boundary.
User Cue commands are delivered into persistent terminals and acknowledge
consumption, not command completion. Neither path passes through this gate.

On Windows, ConfigureTreeCancellation currently uses taskkill for cancellation;
it does not place these commands in crash-bound Job Objects. A detached child may
survive shell completion or daemon crash, so the gate cannot prove a global bound
on descendant processes across restart. Adding that ownership/recovery authority
and a provider-native tool adapter remains necessary before claiming a fleet-wide
CI/test cap or a generic tool failure retry budget.
