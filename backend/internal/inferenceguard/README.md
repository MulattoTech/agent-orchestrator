# Inference authority

This package is a local dispatch authority, not a quota estimator. It has no network client and never launches a provider.

## Strict build

The unofficial build sets the following Go linker variable to `1`:

`github.com/aoagents/agent-orchestrator/backend/internal/inferenceguard.RequirePolicy`

An absolute `AO_INFERENCE_POLICY` JSON file is then mandatory. Missing, malformed, or changed account policy fails closed. The normal upstream build retains its previous behavior unless this environment variable or `AO_REQUIRE_INFERENCE_POLICY=1` enables the guard.

The owner policy contains version 1, an account_key, authorization_epoch, an absolute shared authority_path, native_account_sha256, emergency_stop, pause, maximum_effort, maximum_inference, maximum_per_project, maximum_reviewers, turn_budget, reserve_turns, five role model allowlists, and optional explicit owner exceptions. Medium is the maximum automatic effort. Luna is prohibited in serious engineering roles; a lightweight role must be explicitly selected. All selected models and efforts must also be present in the native provider's supported catalog.

All governed AO data roots for the same account must use the same authority_path. A separate SQLite WAL database uses FULL synchronous commits. An exclusive operating-system owner lock refuses a second daemon for that authority, including unsupported locking filesystems. The lock is released by the kernel after a crash. The authority database must be preserved between restarts and data-root changes. An already-running unpatched AO installation remains outside this authority.

The native ChatGPT account observation is bound by SHA-256 of the account type, a NUL separator, and the normalized email. It stores neither raw email nor tokens. The current native account protocol does not provide an immutable account/workspace ID, so two workspaces sharing the same email cannot be distinguished with this observation.

## Dispatch and recovery

Dispatch writes a durable attempt before the provider write. Request identity, resolved model and effort, role, project, owner exception and the authorization epoch are recorded. Requested blank values remain blank in the record, distinct from resolved explicit values. The native driver writes the resolved model and effort into the final request envelope.

Account-global, per-project and reviewer inference capacities are independent of session/worker slots. Queued work consumes no inference capacity. A write with an ambiguous outcome retains its lease, and restarting converts outstanding active leases to uncertain. Such work cannot be replayed by changing its request ID; the controller must reconcile the native turn before releasing capacity. Already admitted IDs, changed session/project/role identity under an existing ID, and conflicting native turn bindings are refused. The owner may correct an unsubmitted selection; the final dispatch event records the requested and resolved values and resolution rationale.

Usage-limit events latch EXHAUSTED across all roles and persist the lockout. A later error without a reset timestamp cannot erase an earlier structured reset. A wall clock reaching the reset time does not clear the lockout. ConfirmAllowance requires fresh, same-account, explicit non-inference evidence after lockout and after any known reset. Emergency stop and pause remain separate owner controls. Quota lockout requests bounded native interrupts for registered governed controllers; operator emergency controls call InterruptActive after persisting the stop. An interrupt acknowledgement does not release a lease until native completion arrives. A soft pause only prevents new admission.

The finite turn_budget is a conservative count of AO-managed turn starts. It is not an estimate of provider allowance, dollars or native internal model responses. reserve_turns are unavailable to ordinary dispatch. Moving queued work to a new explicitly authorized epoch charges that new epoch when it is admitted.

Refusal errors carry stable sentinels MODEL_POLICY_DENIED, QUOTA_BLOCKED, INFERENCE_CAPACITY or DISPATCH_OUTCOME_UNCERTAIN. PreDispatchRefusal identifies an invocation with no provider write. Unknown transport failures must not be requeued as if they were proven refusals.

## Accounting

IngestReceipt accepts immutable independent provider-response receipts with a stable source key, native turn ID, AO session ID, model, effort, timestamp and token counts. Repeated semantically identical receipts are idempotent even when a rotated-file observation has a different timestamp; the first observation timestamp is retained. Conflicting duplicates and cumulative turn/thread counters are rejected. Receipt ingestion does not require external telemetry opt-in or a telemetry exporter.

Snapshot is a non-inference local read of lockout, reset provenance, owner controls, caps, conservative budget use, states and model/effort usage. Its model groups include receipt-only historical data. Missing receipts remain unknown; no dollar attribution is fabricated. A caller must wire a trustworthy local receipt observer or importer; the core IngestReceipt method alone is not continuous collection.

## Native boundary limitations

The guarded Codex Chat driver can enforce AO-visible requests. Terminal sessions, fallback providers and explicit native compaction without a per-request model/effort envelope are denied in strict mode.

An outer native turn can contain many internal model responses, automatic compaction or native subagents. This package does not claim that one outer turn equals one model response, or that the outer-turn cap bounds hidden native responses. Native internal dispatch behavior must be qualified separately before live fleet approval. The isolated candidate must remain emergency-stopped during the exhausted allowance and offline validation.

Tests use temporary SQLite databases, fake clocks, writer spies and synthetic account/catalog observations. They never invoke a real provider.
