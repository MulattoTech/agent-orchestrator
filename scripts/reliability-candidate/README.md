# Unofficial AO reliability candidate

This package prepares an isolated Windows test candidate. It does not install,
update, replace, or start the running AO desktop installation. PowerShell **7.4
or newer** is required. Read these scripts with the final repair evidence and
controlled validation plan.

## Model and account status

The JSON template is structurally valid and deliberately has a blank native
account fingerprint. It cannot authorize a native dispatch or pass the startup
script until the owner supplies an independently verified fingerprint.

The proposed roles use **gpt-6-astra** for orchestration and complex work,
**gpt-6-sol** for routine implementation, review, and lightweight work, and
**gpt-6.1-terra** as an explicit optional implementation/reviewer selection.
**Exact Sol and Terra provider IDs and supported efforts are UNVERIFIED until
the native catalog is checked after the allowance refresh.** Historical Astra
configuration is also not proof of current catalog support or provider billing.
Unsupported selections fail closed. There is no silent fallback to Astra, Luna,
or another provider. Luna is excluded until task-fitness evidence and an
explicit owner policy revision justify its use.

Medium is the automatic effort maximum. The template starts with emergency stop
and pause enabled, one simultaneous inference, project/reviewer inference caps
of one, and only one admitted outer turn available before the reserve. An outer
turn can contain multiple native model responses; this is **not** a billable-call
or dollar ceiling. Native hidden subagents and automatic compaction remain a
live qualification limit.

Keep the same account_key, authorization_epoch, and authority file across
candidate versions unless an owner-reviewed change explicitly authorizes a new
budget. The shared authority is:

~~~text
C:\AO-MTechLabs-Private-20261116\authority\codex-owner-inference.sqlite
~~~

Never delete or relocate that database to clear a quota stop or regain budget.
The exclusive authority owner prevents two governed daemons using it at once.
Existing unpatched AO processes remain outside these candidate controls.

## Build from a complete committed source

Run from the selected clean checkout after all code and these scripts are
committed:

~~~powershell
pwsh -NoProfile -File .\scripts\reliability-candidate\Build-Candidate.ps1 -SourceCommit <full-40-character-commit>
~~~

The recipe:

1. Requires exact HEAD and a clean worktree before and after compilation.
2. Uses the explicitly selected local Go toolchain, cached modules, one package
   build at a time, and Windows/amd64 with CGO disabled.
3. Builds AO.exe with inferenceguard.RequirePolicy=1 and CLI Version/Commit/Date.
4. Builds the standard-library-only fake worker, hashes it, and compiles the
   session-manager test package with its fixed offline TestMain allowlist and the
   worker's exact SHA-256.
5. Writes a **new** C:\AODev\releases\ao-unofficial-UTC-commit12 directory,
   per-file hashes, and a manifest/digest. It leaves failed artifacts intact for
   inspection and never overwrites an earlier release.

The build script executes no resulting binary, daemon, provider, or test.
The go test -c invocation compiles the offline companion. Build manifests report
validation as not run; successful compilation does not qualify live behavior.

Hashes detect accidental changes relative to the locally issued manifest; they
are not a third-party signature. Preserve the final manifest hash in the repair
evidence before using the candidate.

## Offline fleet exercise

From the compiled release directory:

~~~powershell
pwsh -NoProfile -File .\Run-Offline-Fleet.ps1
~~~

This script accepts **no arguments**. It verifies the packaged files and runs
only ao-offline-fleet.exe with no external test selection. The linked TestMain
forces its compiled allowlist. The sibling fake worker is independently checked
against the hash linked into that companion. Inherited AO/account environment
settings are removed only for this fake run, then restored in the invoking
PowerShell process.

Results and logs go to a new C:\AODev\validation\offline-GUID directory.
The fixture manages temporary state, injected runtimes, and up to 40 bounded
standard-library Windows child processes. It does not prove ConPTY, real provider
loops, a full AO desktop/daemon run, or economical engineering output.

## Post-refresh startup is a separate owner action

Start-PostRefresh-Validation.ps1 cannot run before
**2026-11-21 12:30:00 UTC**, and it also requires an explicit authorization
switch. This time gate is deliberately later than the recorded reset hint.
Time alone never clears the durable quota breaker or proves available allowance.

Prepare a copy of policy.template.json outside the immutable release directory.
Fill its native account fingerprint from a verified, non-inference account
identity observation; keep the shared authority and conservative controls intact.
The fingerprint is SHA-256 of the text
"chatgpt", one NUL byte, and the normalized lowercase/trimmed native account
email. Do not substitute a guessed account, API key, reset credit, or another
account's allowance.

The launch parameter checks consistency between the explicitly owner-verified
hash and the owner policy. Matching two input strings is **not native identity
proof**. The script does not independently query the native account. The provider
boundary still verifies native identity before any future turn. An email-derived
fingerprint does not distinguish every same-email workspace.

Windows account storage has an independent pre-live gate. The source refuses
unsafe owners, reparse paths, and ancestor grants such as untrusted DELETE,
DELETE_CHILD, WRITE_DAC, WRITE_OWNER, or generic write/all access. Existing host
evidence identified unsafe grants in C:\AODev and profile/temporary ancestors.
Those permissions are not weakened or repaired by these scripts.

The launcher first inspects the explicitly supplied native home and the ACL of
its existing auth.json **without reading credential contents**. Its credential
preflight conservatively requires private access; unsupported ACL layouts remain
blocked even if a separate native permission scheme might allow them. It does
not log in, copy credentials, modify profile ACLs, or initiate native ACL repair.
An unsafe native home is an independent **NO-GO** until the owner separately
qualifies a safe account root. Private candidate data alone does not solve that
host issue or prove that full account bootstrap will work.

After these checks, the first launch creates the fresh directory
C:\AO-MTechLabs-Private-20261116 directly under the system drive. It changes only
that newly created directory's DACL, using its existing Get-Acl descriptor,
to grant current SID, SYSTEM, and Administrators access with protected
inheritance. It does not change an owner, SACL, existing ancestor, or existing
profile ACL; it never elevates. DACL modification or verification failure stops
the procedure. A failed/new directory is retained for inspection.

Only the validated **policy** is copied into the protected root, together with
an ownership marker. Later launches must use the preserved
C:\AO-MTechLabs-Private-20261116\owner-policy.json. They verify the private root
and matching owner/account marker read-only and never reset its permissions or
authority. An existing unmarked root is refused.

After the separate owner review and the time gate, substitute the real
owner-controlled paths and verified hash:

~~~powershell
pwsh -NoProfile -File .\Start-PostRefresh-Validation.ps1 -AuthorizePostRefreshValidation -OwnerPolicyPath C:\AODev\owner-policy.json -VerifiedNativeAccountSHA256 <verified-64-character-hash> -NativeHome C:\Users\<owner>\.codex
~~~

The script creates a fresh absolute data/run/log directory under
C:\AO-MTechLabs-Private-20261116\validation, refuses an occupied port, and uses only
**127.0.0.1:39431** (plus the daemon's supported IPv6 loopback listener).
It calls the hidden AO.exe daemon command. It never uses ao start, which opens
or downloads the desktop app.

The launch has registered fleet size 40, worker admission initially 2, inference
1, autoscaling off, emergency stop on, and pause on. It disables automatic
harness updates/version refresh, tracker intake, cloud offering, and remote
telemetry. The new data root has no imported projects, queued tasks, sessions,
or mobile listener settings. Installed-harness readiness/version probes can
still occur at daemon startup; this is why the actual candidate daemon is not
started during the offline phase.

**AO_ENABLE_ALLOWANCE_CHECK=0 remains fixed.** Startup reads only health and
the local reliability snapshot; it does not verify allowance, start a thread,
send a model turn, or resume work. An independently reviewed later step must
enable and qualify the non-inference allowance bridge before the first
controlled model turn. Unknown or incomplete native window layouts stay blocked.
Fresh complete five-hour/weekly capacity can arrive in either native field order.

The launcher uses AO_OWNER=app with a unique reliability run ID solely to own
the existing server-side stopAgents=true shutdown contract. It does not start
Electron or connect to the supervisor named pipe/socket. The watchdog arms only
after its first actual connection, so launcher-shell exit does not trigger that
watchdog. App-tree memory observations can include the launching shell parent;
they must not be mistaken for provider memory alone. Windows process creation
and log-handle behavior still require the post-refresh startup qualification.

## Health, stop, and rollback

Successful startup prints its immutable candidate-run.json path and the local
dashboard URL. Readiness requires exact PID, executable path/hash, OS process
start time, run-file ID/port, owned loopback socket, and AO health service identity.
The initial reliability snapshot must confirm emergency stop, pause, inference
cap one, and disabled allowance verification.

To stop only this candidate:

~~~powershell
pwsh -NoProfile -File .\Stop-Candidate.ps1 -RunRecord C:\AO-MTechLabs-Private-20261116\validation\reliability-GUID\candidate-run.json
~~~

The stop script verifies all identity evidence again and sends only the
run-scoped /shutdown mutation with stopAgents=true and the exact run ID. The
server rejects a different run ID. It deliberately omits an unfenced emergency
control PUT: if the port were reused between requests, that PUT could affect a
successor. The script requires
HTTP acknowledgement and waits up to 60 seconds for the exact daemon process
to exit. It never calls a broad process kill, process-name kill, worktree removal,
database deletion, production update, or cleanup of another run.

An unacknowledged stop or timeout preserves the process, state, and logs for
inspection and returns failure. A daemon exit is recorded separately from
individual native-child completion, which remains UNKNOWN until cleanup
evidence confirms it. Do not claim a child stopped from an HTTP 202 alone.

Rollback means stop the isolated candidate and retain its data/evidence. The
production installation remains untouched. Never copy candidate state into
production, replace the production binary, delete the shared authority, reuse
an existing validation data root, or merge a protected branch as part of this
procedure. If identity checks fail, investigate the recorded path/PID/run ID;
do not bypass them with a broad kill.

## Static verification and remaining qualification

Test-CandidateScripts.ps1 parses PowerShell ASTs and reads the JSON template.
It checks compile-only build commands, fixed fake-run arguments, pre-launch
gates, owned stop checks, and conservative policy structure. It never executes
the start/stop scripts, AO, a provider, or an account endpoint.

Only static start/stop checks are authorized in the exhausted-allowance phase.
The final candidate's compiled offline companion is run separately after the
final source commit. Fresh private-directory creation/DACL behavior, native-home
safety, full account bootstrap, actual daemon startup, native catalog/account
observations, allowance freshness, interrupt acknowledgement, native internal
requests, and one bounded real engineering turn remain live qualification gates.
