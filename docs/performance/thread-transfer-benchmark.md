# Thread-transfer benchmark

A local, deterministic measurement of how many bytes and messages it takes the
daemon to send a Chat thread to a client: the history a client loads when it
opens a chat, and the live updates while a tool-heavy turn runs. It compares a
baseline commit with the current working tree and checks both against reviewed
ceilings.

It is local-only. Nothing is published, no provider runs, no account or real
transcript is read, and nothing listens beyond `127.0.0.1`.

## One command

```bash
scripts/transfer-benchmark.sh                       # baseline: origin/main (else main), no fetch
scripts/transfer-benchmark.sh --baseline-ref <ref>  # any commit
scripts/transfer-benchmark.sh --out-dir /tmp/tb     # where run, report and log files go
```

It prints the Markdown report and writes `baseline.json`, `candidate.json`,
`report.json`, `report.md`, and both harness logs to the output directory
(default `$TMPDIR/ao-transferbench-results/<UTC time>`).

Exit codes: `0` pass, `1` a ceiling, relative cap, or semantic check failed,
`2` invalid input, incomparable runs, or setup failure.

To replay the harness alone, as an ordinary test:

```bash
cd backend
AO_TRANSFERBENCH=1 GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly GOPROXY=off \
  go test -count=1 -p 1 -run '^TestTransferBenchmark$' ./internal/httpd/
```

Without `AO_TRANSFERBENCH=1` (or the script's variables) the test skips, so
`go test ./...` does not pay for the replay.

For analysis, `AO_TRANSFERBENCH_DUMP_DIR=<dir>` also writes every counted
request with its decoded body, every event frame, each live-turn step, and how
each refresh resolved (local refetch, merged tail, tail then full page) as JSON
lines, one file per scenario. It never changes a measurement.

## What it measures, mapped to the real protocol

There is no thread WebSocket and no delta payload in AO. A Chat client reads
history as paged JSON and learns about changes from the CDC event stream, then
reads again:

| Path | Transport in AO | In this benchmark |
|---|---|---|
| History snapshot | `GET /api/v1/sessions/{id}/conversation` (`controllers/conversations.go` `snapshot`), paged by `beforeSequence`/`afterSequence`/`limit`, remote output cut by `outputLimit` | Actual handler, service, store, DTO and `envelope.WriteJSON` |
| Live change signal | SQLite triggers write `session_updated` rows to `change_log` (payload `id`, `sessionId`, `conversationId`, `activity`, …); `cdc.Poller` publishes; `GET /api/v1/events` (`httpd/events.go`) writes `id`/`event`/`data` SSE frames | Actual triggers, poller, broadcaster and SSE writer |
| Live refresh | Renderer refetches: local invalidates the chat (refetches every loaded page, `limit=200`); remote reads a tail (`afterSequence=floor&limit=30&outputLimit=512`) and merges it (`useConversation.ts` `refreshRemoteConversation`) | Go **simulator** of the renderer's rules, applied to real responses |
| Local desktop | Loopback listener, HTTP/1.1, never compressed | `local`: the production router on a loopback `httptest` server |
| Remote desktop and mobile | LAN listener chain `lanControlBlock(auth(remoteJSONCompression(router)))`: bearer auth, gzip (BestSpeed) or zlib-deflate for `application/json` only; SSE never compressed | `remote-lan`: that exact handler (`NewMobileLAN(...).handler`) on a loopback `httptest` server, with `Accept-Encoding: gzip, deflate, br, zstd` |
| Remote over a tunnel | `/api/v1/tunnel` carries HTTP/2 inside one binary WebSocket with compression disabled on both ends (`lan_tunnel.go`, `remote-proxy.ts`), the same LAN chain inside; or cloudflared TLS | **Not measured.** Inner request bodies are the `remote-lan` bodies; HTTP/2, WebSocket, TLS and TCP framing are excluded. No WebSocket compression exists to model. |

### The measured seam

```
fixture steps (normalized ports.ChatEvent)
  -> benchConversation.Events()            (stands where a persistent Codex/ACP host stands)
  -> chat Controller: pumpProviderEvents, projectUnit, provider-event acknowledgement
  -> SQLite store (production migrations) + change_log triggers
  -> lifecycle.Manager activity reduction  (session activity rows and their CDC)
  -> cdc.Poller.Poll -> cdc.Broadcaster -> EventsController SSE  ----\
  -> ConversationsController.snapshot -> chat Service.SnapshotPage  --+-> router [-> LAN chain] -> loopback socket
                                                                       -> simulated renderer
```

The seam starts after provider adapters normalize native frames, like T3's
transfer budget ("does not measure provider adapter ingestion"). Codex
app-server JSON-RPC parsing, ACP parsing, and the persistent provider host
process are not exercised. The fixtures follow what those adapters emit (see
Fixtures).

The harness mirrors the daemon's wiring where it cannot call it: the chat
service's `Reader`/`PageReader` are the daemon's adapters reproduced by copying
every `chatsvc.ConversationRows` field from the store snapshot field of the same
name, failing if one is missing, so the harness cannot serve smaller pages than
the daemon.

## Phases

| Phase | Client action | Requests |
|---|---|---|
| `history-open` | Open the chat: newest page (`limit=200` local; `limit=30&outputLimit=512` remote). `limit` counts messages; the activities between them ride along within the store's page byte budget | 1 |
| `history-full` | Open, then load older pages until `hasMoreBefore` is false (`limit=200` local; `limit=100&outputLimit=512` remote) | 1 + older pages |
| `live-turn` | Chat already open (not counted). Connect the event stream at `after=latest`, `POST` the user message, then one refresh cycle per committed step of a tool-heavy turn | POST + refreshes |
| `expand-output` (remote only) | Expand every tool output the remote page cut, as `fetchActivityOutput` does (`limit=1` page without `outputLimit`). An upper bound: a reader expands only what they open | one per cut output |

History is 60 completed turns per provider (140 Codex and 120 Claude
messages), sized so the remote open page holds only part of it and a full read
pages; the harness fails if a remote open page holds everything. The live turn
adds reasoning,
streamed assistant text, several commands with long output, file edits, and a
tool call.

## Metrics

All counted at the simulated client's TCP sockets over loopback.

| Metric | Meaning |
|---|---|
| `requests` | Conversation HTTP requests (the stream connection is not one) |
| `cdcRows` | `change_log` rows the live turn produced |
| `sseFrames` | Event-stream frames received, including the initial cursor frame |
| `sseBytes` | Event-stream body after de-chunking (never compressed) |
| `headerBytesIn` | Response status lines and headers |
| `framingBytesIn` | HTTP/1.1 chunked framing |
| `bodyEncodedBytes` | JSON bodies as transferred (gzip on `remote-lan`) |
| `bodyDecodedBytes` | JSON bodies after decoding |
| `wireBytesIn` | Everything read: headers + framing + encoded bodies + SSE. Validated as that exact sum |
| `wireBytesOut` | Everything written: request lines, headers, the POST body |

### Actual vs modeled

Actual: every response byte is produced by production code at the measured
commit, including gzip by the daemon's own middleware, and HTTP/1.1 framing as
Go's server writes it. Decoded sizes are real decompressions.

Modeled: when the client asks (the request schedule), request headers (Go's
client plus the renderer's `Accept`, `Accept-Encoding` and bearer headers, not
Chromium's full header set), and everything outside the socket (TLS, TCP/IP,
HTTP/2, WebSocket). No compression is modeled.

### Scheduled replay, not wall-clock timing

The product coalesces by time: the projector batches same-stream deltas for up
to 50 ms, and the renderer batches invalidations in a 150 ms window with one
refresh in flight. Wall-clock coalescing would make counts vary run to run, so
the benchmark fixes a schedule:

- Each fixture step is one event and one projector unit. The controller
  acknowledges an event before its after-commit work (activity reporting, turn
  lifecycle), which can still write rows, so the harness then delivers the same
  event again: the archive skips it as a replay and writes nothing, and its
  acknowledgement can only arrive after the first delivery's unit has fully
  returned. A row written after that barrier fails the run.
- After each step the harness polls CDC once, the client reads exactly the
  frames up to the newest `change_log` sequence, applies the renderer's routing,
  and performs at most one refresh.
- Heartbeat frames are disabled; the service clock is injected and advances one
  whole second per step; ids are UUID-shaped and deterministic; session
  timestamps written by the lifecycle reducer, which has no exported clock, are
  truncated to whole seconds so RFC 3339 lengths do not vary.

Request and frame counts are therefore "scheduled replay" counts: one refresh
per committed step is the worst case for a client whose round trip is shorter
than the provider's gap between units. Real coalescing can only lower them.

## Semantic validation

Bytes cannot pass by losing content. Each scenario reduces the client's
reconstructed view and a fresh, unpaged server read over the same window to a
canonical text: ordered rows (kind, role or activity kind, status, sha256 of
message text or activity summary plus settled detail text, output size) and the
states of the turns they belong to. Then:

- the server text must match the fixture-only oracle that
  `scripts/transfer-benchmark-oracle.py` derives without running the product
  (`testdata/expected/oracle-*.txt`): the whole history and every cut output
  exactly, and the live turn's rows as the view's final rows. The oracle is an
  independent Python re-implementation of the fixture rules, so an expected
  state can never be blessed from product output alone;
- the server text must equal the committed expected state
  (`backend/internal/transferbench/testdata/expected/*.txt`);
- the client text must equal the server text;
- every message and activity the fixture produces must appear (whole history,
  or the live turn when the view is a bounded page), and every full output in
  the view must be one the fixture produced;
- cut remote outputs must report `outputBytes` equal to the full size, and
  expanding them must return exactly the fixture output.

A mismatch fails the harness and poisons the run's `clientDigest`, so a
comparison reports `candidate-state-mismatch` (exit 1) even if bytes fell.

Expected states change only when regenerated explicitly with
`AO_TRANSFERBENCH_UPDATE_EXPECTED=1`, never by the runner.

## Baseline and candidate identity

- The baseline is `git archive <sha>` of `backend/` and the two renderer files,
  extracted to `$TMPDIR/ao-transferbench-baseline/<sha>`. No fetch, no worktree
  registration, the source checkout untouched.
- Only harness files are copied in: `backend/internal/transferbench/**` and
  `backend/internal/httpd/transferbench_harness_test.go`. The overlay only adds;
  a baseline that already has a different harness file is refused, never
  shadowed.
- `verify-export` then checks every file in the baseline tree against the
  commit's git blob id, and that nothing but the overlay was added.
- Both sides run with `GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly
  GOPROXY=off`, `-count=1 -p 1`, and the runner asserts each side compiles its
  own module directory.
- Each run records the source commit, a digest of uncommitted changes to the
  measured paths, Go version and platform, digests of the harness, fixtures plus
  expected states, the ceilings file, and the renderer source, the renderer
  policy constants it applied, and a fresh nonce.

## Comparison rules

`backend/internal/transferbench` (standard library only) decodes runs and
ceilings strictly: 64 KiB cap, regular files only, no byte-order mark or
trailing data, no duplicate, unknown, missing or case-variant keys, integers
only as plain non-negative decimals up to 10^9, allowlisted provider, phase and
transport ids, and `wireBytesIn` equal to its parts.

Incomparable (exit 2): different harness, fixtures, toolchain or ceilings
digest; a dirty baseline; refs other than requested; a missing scenario or
ceiling row; differing expected states; a baseline that did not reproduce its
expected state; or renderer source that differs from what the simulator models.
`--allow-client-change` shows the numbers for such a pair, but the report is
labeled unverified and exits 2 unless something failed: it is never a pass. No
relative change is reported for an incomparable pair.

Fail (exit 1): any candidate metric above its absolute ceiling (equal passes),
above `baseline + baseline × maxIncreasePermille / 1000` (integer arithmetic),
non-zero where the baseline had zero, or any semantic mismatch.

Ceilings live only in `backend/internal/transferbench/ceilings.json`, reviewed
like code, bound to one fixture set by digest. A run never writes them and none
is derived from a baseline at run time.

## Fixtures

`backend/internal/transferbench/testdata/fixtures/{codex,claude}.json` are
scripts of normalized events with invented content for a fictional project.

- Codex shapes follow `codexappserver/normalize.go` (`activityFor`,
  `activityDetail`, `encodeDetail`): reasoning items, aggregate command output
  with `outputSource`, `files` patches, MCP `server`/`toolName`/`arguments`.
- Claude shapes follow the ACP adapter (`acp/client.go` `toolEvent`): every tool
  update re-emits the whole accumulated detail (`protocol`, `toolKind`,
  `locations`, `input`, `output`, `content`, `providerToolName`); edits carry
  `patch`, `oldText`, `newText` and diff content. Tool content blocks are
  representative of claude-code-acp, not captured frames.

## Limitations

- Provider adapters and persistent hosts are not exercised; fixture realism is
  only as good as the shapes above.
- The renderer is simulated in Go from `useConversation.ts` and
  `event-transport.ts` at the commit in `simulatorClientSHA256`. If those files
  change, runs are incomparable until the simulator is reviewed and its binding
  updated.
- Only the conversation is measured. Workspace and session-list refetches that
  the same events trigger are not.
- Timing, latency, CPU and memory are not measured.
- Byte counts are exact for this machine and Go version; gzip output depends on
  the Go version, so runs from different toolchains are incomparable.

## Future CI use

Not wired today. A trusted workflow should treat run artifacts from
untrusted code as data: validate them with this package from the trusted
branch, take ceilings from the trusted side, never execute or source anything
from the artifact, and render only allowlisted ids and numbers. The measured
values themselves are self-reported by the harness, so such a gate is only as
strong as the review of the harness, fixtures and ceilings.
