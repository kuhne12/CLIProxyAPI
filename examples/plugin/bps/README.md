# BPS authorization plugin

[简体中文](README_CN.md)

This Go C ABI plugin adds independent BPS browser and device authorization to CPA. It adapts
the OAuth implementation from [bps-local-public](https://github.com/sunboyzoey/bps-local-public)
and reuses CPA's plugin loader, command-line flags, credential store, proxy
configuration, v8 management sessions, and manual credential refresh.

## Compatibility fixes (0.5.4)

Upgrade CPA and the BPS shared library together. This patch preserves an explicit
`context_management` array through the shared Responses-to-Codex conversion.
Final user payload overrides and filters still run after every BPS conversion.

Pre-generation WebSocket handshakes now follow the upstream transient-error
classification, with at most three attempts and 250/500 ms cancellable backoff.
Permanent failures and an exhausted transport budget stop CPA's outer retry
loop without penalizing the credential. Explicit HTTP rejections are handled by
CPA's existing scheduler; `Retry-After` seconds and HTTP dates are preserved.
Native plugin errors now expose this hint through the scheduler's retry contract.
Caller cancellation controls the handshake budget; the standalone CLI's fixed
handshake and post-connection deadlines are not copied into CPA. A submitted
generation is never replayed by these handshake retries.

The dated `web_search_2025_08_26_preview` hosted-tool alias is recognized and
omitted in automatic mode, with an explicit limitation in the BPS protocol
message. Forced hosted-tool choices remain rejected rather than silently
claiming to perform a search. Normal GPT model IDs remain unchanged.

This patch does not change upstream product context, account entitlements, or
USD estimate semantics. Per-account start pacing and installation-specific task
namespaces remain separate follow-up work.

## Settings panel (0.5.6)

Open **BPS 设置** in the management center’s plugin menu to configure cache-write accounting, the image relay origin, and per-model quota prices and multipliers. The panel uses the existing v8 configuration API. Changes to request settings and model prices are saved separately and hot-reloaded. A stale edit requires reloading the latest settings.

## Build and configure

Run from the **CPA repository root** with Go 1.26+ and a C compiler. This plugin
uses CPA's root Go module. Both the server and plugin need CGO and must target
the same OS and CPU architecture.

```sh
mkdir -p bin plugins
CGO_ENABLED=1 go build -o bin/cli-proxy-api ./cmd/server
CGO_ENABLED=1 go build -buildmode=c-shared -o plugins/bps.dylib ./examples/plugin/bps/go
```

Use `bps.so` on Linux/FreeBSD and `bps.dll` on Windows. On Apple Silicon, prefix
**both** commands with `GOARCH=arm64` if your Go environment targets `amd64`.
The generated C header is not needed at runtime. Keep the basename `bps` so it
matches the plugin configuration key.

Merge this into your v8 configuration (adjust paths):

```yaml
config-version: 8
oauth:
  auth-dir: "./auths"
plugins:
  enabled: true
  dir: "./plugins"
  configs:
    bps:
      enabled: true
```

OAuth requests honor CPA's global `requests.proxy-url`; refresh also honors the
credential's `proxy_url` override. `direct`, HTTP(S), and SOCKS proxies use CPA's
existing transport helper. No OAuth response bodies or tokens are logged.

## Command line

```sh
./bin/cli-proxy-api --config config.yaml --bps-login --no-browser
./bin/cli-proxy-api --config config.yaml --bps-import /absolute/path/oauth.json
```

Browser OAuth is the default. Open the printed URL, sign in and approve access,
then paste the complete callback URL into the terminal. No device code or
server-side browser is required. Omitting `--no-browser` attempts to open the
system browser. The command waits up to 15 minutes; Ctrl+C cancels it.

The BPS client has a fixed registered redirect at
`https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback`.
It cannot be replaced with a CPA URL. Copy the address containing `code` and
`state`, even if the callback page shows an error, and submit it only to your CPA.
Device login remains available with `--bps-login-method device`.

Import reads an existing BPS OAuth JSON file without modifying the source. It
requires the BPS `client_id` (`app_fnr0pYvVwwFDocDumLG3H2Bp`) and rejects Codex
credentials. Original flat bps-local files are supported, as are nested `tokens`
files with an explicit BPS client ID. Account identity and absolute expiry must
be recoverable from the file or token claims. Claims are used for bookkeeping,
not as proof of upstream authorization.

CPA saves the result as `type: bps`, using a stable `bps-<identity-hash>.json`
filename. With the file store, new files have mode `0600`. Re-importing the same
identity updates its destination; `prefix`, `proxy_url`, `disabled`, and other
credential settings are preserved. The command prints the saved destination.

## v8 management API

Set `management.secret-key` to enable management routes. The examples below use
the existing management bearer key, not a proxy client API key:

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/oauth/auth-url?provider=bps&method=browser'
```

The response contains `status`, `url`, `state`, `interval` (seconds), and
`expires_at`, with no `user_code`. Omitting `method` also selects browser login.
After signing in at `url`, paste the complete callback into the existing CPA API:

```sh
curl -X POST -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"provider":"bps","redirect_url":"PASTE_FULL_CALLBACK_URL_HERE"}' \
  'http://127.0.0.1:8317/v8/management/oauth/callback'
```

Poll with the original state to exchange the code and save the credential:

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/oauth/status?state=RETURNED_STATE'
```

`wait` means pending, `ok` means credentials have been saved, and `error` means
the login must be restarted. Polling drives completion; no poller runs in the background. Browser sessions
use independent state and PKCE, reuse CPA callback files, and never retry a
possibly consumed authorization code. Explicit `method=device` returns a
`user_code` and enforces device polling intervals, including `slow_down`.
Concurrent polls exchange and save only once. Private device IDs, PKCE verifiers,
and tokens are never part of these responses.

Cancel a pending login:

```sh
curl -X DELETE -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/oauth/session?state=RETURNED_STATE'
```

`cancelled: true` prevents a later in-flight result from being saved. Once a
successful result has claimed the save, cancellation returns false and the save
finishes even if the polling client disconnects. Restarting or unloading the
plugin invalidates pending login sessions.

List saved credentials and manually refresh one using its returned filename:

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/credentials'
curl -X POST -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"name":"bps-IDENTITY_HASH.json"}' \
  'http://127.0.0.1:8317/v8/management/credentials/refresh'
```

Refresh preserves account identity, settings, and the existing filename. It
saves rotated credentials through CPA. A token exchange is never automatically
retried: a failed response can leave rotation outcome unknown. Definitive
`invalid_grant` requires a new login. Only one process should manage refresh for
a grant; sharing the same grant across independent processes adds no rotation
coordination.

## Inference (0.3.0)

Upgrade CPA and the plugin together. The plugin publishes credential-bound model
routes and the matching CPA build executes BPS WebSocket requests using the
bps-local request/tool/stream adapter. Existing `type: bps` files work directly;
no credential relabeling, new login, or standalone bridge is needed.

From 0.5.7, each BPS credential discovers its model IDs, labels, and reasoning
levels from the authenticated upstream `model_catalog` at
`/basispoints/api/responses/access?include_models=true`. There is no fixed model list.
Discovery runs at credential registration and on a five-minute background refresh
in standalone CPA mode. A one-minute, credential-scoped memory cache coalesces
duplicate registration calls. Inference does not wait for model discovery.
Temporary network errors, 429/5xx responses, and malformed catalogs preserve the
last successfully registered models for that process. A first-start failure
publishes no models until a later retry succeeds. HTTP 401/403, `allowed:false`,
and an explicitly empty catalog remove the credential's models. Entries in
`restricted_models` are excluded. No static list is used as an outage fallback.
The settings page also reads model suggestions from the registered credentials.
From plugin 0.3.1, model IDs match the upstream GPT IDs without a provider prefix.
If another provider offers the same ID, CPA uses its normal credential routing.
Select the BPS credential explicitly for provider-specific verification.
These are selectable routes, not an entitlement claim. BPS enforces permissions
and capacity. Both `/v1/chat/completions` and `/v1/responses` support streaming
and non-streaming output. For example:

```sh
curl http://127.0.0.1:8317/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-astra","messages":[{"role":"user","content":"Reply OK"}],"reasoning_effort":"low"}'
```

The executor preserves CPA proxy selection, cancellation, usage reporting,
canonical reasoning handling, and final payload rules. It allows four concurrent
requests and 64 queued requests per account. It never automatically replays a
sent generation. Product context injected by the upstream can contribute roughly
22k input tokens even for a short prompt; reported usage includes those tokens.

### WebSocket response framing (0.5.2)

The matching CPA build accepts compact and pretty-printed WebSocket JSON frames.
It preserves text newlines and reports upstream model-access or quota errors
instead of masking them as unsupported response/tool errors. Upgrade CPA with
the plugin; the available model catalog does not guarantee account access.

## Account quota (0.4.0)

The plugin registers CPA's quota capability, so BPS authorization files expose
the existing management UI quota query action. It uses the same authenticated
`https://chatgpt.com/backend-api/wham/usage` endpoint as Codex; BPS's official
application links to the Codex usage settings page for account usage and credits.
Queries run through CPA's host HTTP callbacks, preserving request cancellation
and credential/global proxy settings, without generating model tokens.

The result includes the reported plan, remaining percentages and reset times
for available quota windows, and an optional credits balance. Missing or null
values remain unknown; a missing balance is never presented as zero. Credits
are not converted to money. Manual quota reset is not supported.

```sh
curl -H 'Authorization: Bearer YOUR_MANAGEMENT_KEY' \
  'http://127.0.0.1:8317/v8/management/plugins/bps/quota?auth_index=YOUR_AUTH_INDEX'
```

Find `auth_index` in `/v8/management/credentials`; BPS entries also advertise
`supports_quota: true` and `quota_plugin_id: bps`.

### Estimated quota value in USD (0.5.1)

Configure `plugins.configs.bps.quota-model-prices` to enable estimated total and
remaining API value in the existing quota UI. The map is keyed by exact response
model ID (request model when the upstream omits it). Each entry accepts `input`,
`output`, `cache-read`, and `cache-write` in **USD per million tokens**. Input and
output are required; cache prices are required when those token classes occur.
Prices are administrator-supplied, with no built-in rates or automatic refresh.

Optional `priority-multiplier` and `flex-multiplier` apply to the corresponding
service tier; unknown tiers or missing multipliers invalidate the sample. Optional
`long-context-threshold` requires `long-context-input-multiplier` and
`long-context-output-multiplier`: above the threshold, the former applies to
ordinary input and cache tokens, and the latter to output. Omit these fields for
flat context pricing. See the Chinese README for a complete configuration example.

Like Sub2API's window-cost estimate, calibration divides priced usage by quota
utilization. This implementation uses deltas between quota observations, so a
new installation never mistakes partial local history for full-window costs:

```text
estimated total USD = observed USD cost delta * 100 / used percentage-point delta
estimated remaining USD = estimated total USD * current remaining fraction
```

The first query establishes a baseline. After normal BPS traffic increases usage
by at least one percentage point, query again to obtain USD metrics. Until then,
the window description reports sampling progress; unknown values never become
zero dollars. Queries do not generate model requests. The usage callback does no
network or disk I/O and retains only bounded numeric counters and hashed IDs.

Accounting follows `cache-creation-as-input`, counts total input only once, and
does not add reasoning tokens to output a second time. Credentials and primary
quota windows are isolated; code-review and additional limits are not priced.
Duplicate execution IDs are ignored. Failed or unpriced usage restarts the observation
interval while retaining an already completed estimate. Quota resets, decreasing
utilization, plan changes, and pricing changes invalidate the completed estimate.
Since 0.5.3, completed estimates are atomically stored in
`plugins/bps-quota-estimates.json` with private permissions and hashed credential
identifiers. Keep this directory persistent. After restart, the next quota query
checks the saved plan/window and begins a fresh observation interval; traffic
while CPA was offline is never included in a new calibration. Partial samples
remain memory-only. Read/write failures fall back to fresh/in-memory estimation.

These amounts are **estimated API value, not an official or redeemable account
balance**. External usage on the same upstream account, changing model mix, rounded
quota percentages, and asynchronous reporting affect accuracy. They do not apply
Sub2API user discounts or group multipliers, and do not convert upstream credits
into dollars. Each quota window is estimated independently; do not sum windows.

## Inline images (0.5.3)

The native executor now includes the bounded ImageRelay from bps-local-public.
To accept data:image/base64 input, point a public HTTPS origin at this CPA server
and configure the BPS plugin:

```yaml
plugins:
  configs:
    bps:
      enabled: true
      image-relay-origin: https://cpa.example.com
```

The reverse proxy must forward GET and HEAD requests for `/api/bps-images/` to CPA
without a separate login screen. Authenticated generation rewrites inline images
into unguessable signed HTTPS URLs. Each image expires after ten minutes; the
handler serves only registered images, never a filesystem directory. Files use
private permissions and are deleted on expiry or graceful shutdown. Existing
HTTPS image URLs are unchanged. An empty origin keeps HTTPS-only behavior.

PNG, JPEG, GIF and WebP are validated before storage. Limits are 20 MiB per image,
32 MiB and 128 inline images per request, 64 million pixels per image, and a
1 GiB / 512-image service-wide store. Downloads have bounded concurrency.
Chat Completions and Responses streaming/nonstreaming share this implementation,
including supported image content in tool results. Image conversion precedes
final user payload rules, so configured overrides and filters remain authoritative.
Changing the origin affects new links; already issued links remain valid through
the original reverse proxy until expiry. No third-party image hosting is used.

## Cache creation as regular input (0.5.0)

Upgrade both CPA and the plugin to the matching build. To charge BPS cache
creation at the downstream gateway's ordinary input rate, enable the optional
plugin setting (it defaults to `false`):

```yaml
plugins:
  configs:
    bps:
      enabled: true
      cache-creation-as-input: true
```

The executor clears cache-creation counts and TTL details in downstream usage
before translating Chat Completions and Responses, for both streaming and
non-streaming requests. Total input already includes cache creation, so it is
not added again. Cache reads, output, reasoning, and total tokens are unchanged.
For example, 1,000 input tokens including 100 cache reads and 200 cache writes
are reported downstream as 1,000 input tokens, 100 cache reads, and zero cache
writes; a gateway that subtracts cache reads bills 900 ordinary input tokens.

CPA's internal usage reporting retains the original upstream breakdown for
auditing. This setting changes downstream classification only: it does not
disable upstream caching, remove BPS product context, change upstream charges,
set downstream prices, or rewrite historical bills. It applies to new requests;
in-flight streams keep their original setting. Disable it to restore the
upstream cache-creation classification. Model IDs remain unchanged.

## Replaying compacted history

Upstream requests inline compaction through `context_management` (a default
threshold of 200,000 tokens). This adapter additionally avoids uploading known
covered history again when clients replay a returned checkpoint.

When a completed BPS generation returns an inline compaction item, CPA remembers
its fingerprint and the fingerprint of the input it covers. If the same caller
and credential replay that checkpoint in the same session with the matching input
prefix, CPA omits covered assistant messages, reasoning, previous checkpoints,
and completed tool exchanges before sending the next WebSocket request. It keeps
user and developer instructions, tool declarations, configuration updates, and
calls needed by later results. Final payload rules still apply.

The process keeps fingerprints for up to 1,024 checkpoints. On a cache miss,
restart, or edited prefix, CPA sends the supplied history. Clients can reduce
the upload to CPA itself by applying their inline compaction before sending the
next request. Preserve the complete output window of a standalone compact call.

## Request timing

The matching CPA executor records TTFT starting immediately before upstream I/O,
excluding local slot wait and request preparation. It uses CPA's Responses event
classification for text, reasoning, validated tool arguments and terminal-only
responses. Metadata events do not count as substantive output; terminal events
provide a fallback when no output token was observed. Tool TTFT includes the
bridge's existing final-response validation wait.

Each request writes one `BPS request timing` structured log entry, correlated by
the normal request ID, including failures and cancellation. `queue_ms`,
`prepare_ms`, `ws_dial_ms`, `ws_write_ms` and `upstream_open_ms` are stage durations.
Dial duration sums actual handshake attempts; upstream-open duration also includes
retry backoff and writing. These overlapping durations must not be added together.
`input_bytes` measures the executor input and `upstream_bytes` measures the final
WebSocket payload after user payload rules.

The transport observation boundaries follow upstream's
[WebSocket timing implementation](https://github.com/sunboyzoey/bps-local-public/blob/c66fb41006baf7ede70af69a3e2f81af6a6c246b/internal/proxy/websocket_timing.go).
The intervals between write completion, upstream events and output remain local
observations; they cannot independently separate upstream queueing, context
processing, reasoning and network transit without additional upstream timestamps.

Other `*_ms` milestones are offsets from executor entry: `first_wire_event`,
`first_response_event`, `upstream_sent`, `first_upstream_text`,
`upstream_tool_arguments_done`, `upstream_tool_item_done`, `upstream_terminal`,
`first_handoff`, `first_text_handoff`, `tool_handoff`, `terminal_handoff`,
`response_ready` (nonstreaming), and `executor_done`. Missing events are omitted.
`tool_delivery_wait_ms` is the interval from the first upstream tool arguments-done
event to the first validated tool-argument handoff. Handoff means delivery to CPA's
stream channel, not receipt by the remote client. Write completion similarly marks
the local write call, not upstream processing or receipt confirmation.

With `requests.passthrough-headers: true`, successful responses also expose the
available pre-stream timings as `X-BPS-Queue-Ms`, `X-BPS-Prepare-Ms`,
`X-BPS-Ws-Dial-Ms`, `X-BPS-Ws-Write-Ms`, and `X-BPS-Upstream-Open-Ms`, plus input/wire
sizes and handshake count/status. Later milestones remain in the log. No prompts,
tool arguments, tokens, cookies, or raw upstream errors enter these observations.
This instrumentation does not alter request payloads, tool delivery, retries, or
network timeout behavior.

### Compaction and response validation diagnostics

These diagnostics run in CPA's native BPS executor. Install a CPA server build
containing this change; replacing only the BPS authorization shared library does
not enable them. No plugin configuration or full request logging is required.
At the normal info level, `BPS request timing` also records:

- `scope_hash` for the isolated account/caller/session scope, and the bridge's
  `tool_catalog_count`, `tool_catalog_hash`, and up to 64 sorted tool names.
  The hash covers the complete merged declarations, including descriptions of exec helpers, even when
  names are truncated.
- `history_status`: `no_checkpoint`, `input_not_array`, `checkpoint_not_found`,
  `invalid_checkpoint`, `checkpoint_without_prefix`, `prefix_mismatch`, `trimmed`, `nothing_to_trim`, or
  `trim_failed`. Item/byte counts before and after trimming, covered item count,
  cache entry count, checkpoint ID hash and iteration override explain the choice.
  Checkpoints live only in process memory; restart clears this cache.
- `compaction_seen` and `compaction_confirmation`: `confirmed` means the validated
  terminal response registered a checkpoint; `not_confirmed` means a checkpoint
  was received but not registered; `payload_input_changed` means final payload
  rules changed the input and made confirmation unsafe. While a request is still
  being validated, a warning may show `pending_validation`.

A bridge validation failure immediately writes one warning,
`BPS response validation failed`, before the public error is normalized. It
includes `validation_stage`, a locally authored `validation_reason`, native tool
type/name, decoded client tool name when available, argument size/type, transport
shape, pending tool count and at most eight missing tool name/identity hashes.
Use the normal CPA request ID to join this warning with its timing record and
access log. A `tool_translation` failure differs from
`terminal_tool_completeness` (an earlier tool item is absent from the terminal
output) and `structured_output` validation failure. The client-facing error
remains unchanged.

These logs contain no prompts, code, argument values, credentials, encrypted
compaction state or raw external parser errors. Checkpoint/call/item identities
are SHA-256 hashes; invalid or overlong tool names are hashed. Argument byte size
is `-1` when the native argument value is not a string. An unclassified internal
error is reported with a generic reason rather than logging its raw message.
The bridge catalog is collected before final payload rules;
`payload_input_unchanged=false` identifies input changes made by those rules.

## Scope and compatibility

- Authorization, import, persistence, manual refresh, model registration and
  inference and account quota queries are supported. Automatic scheduled token
  refresh is not configured.
- The upstream adapter supports text, HTTPS image URLs, client tools, and
  validated structured output. Inline image hosting, hosted search, workbook
  operations, `previous_response_id`, and token counting are unsupported.
- The matching CPA build is required. The management frontend's login flow still
  uses the documented CLI/API and manual browser callback submission.

## Verification and attribution

```sh
CGO_ENABLED=1 go test -race ./examples/plugin/bps/go/... ./internal/runtime/executor/helps/bpsbridge
CGO_ENABLED=1 go test ./internal/runtime/executor -run TestBPS
CGO_ENABLED=1 go test ./internal/pluginhost -run '^TestBPSSharedLibraryImportsAndPersistsIndependentGrant$' -v
```

Tests use local OAuth fixtures and synthetic credentials, including device
pending/approval/denial, PKCE, expiration, concurrency, refresh rotation, import,
and actual shared-library loading and persistence. Live account authorization
and generation permission require a separate account test.

See [NOTICE](NOTICE) and [LICENSE.upstream](LICENSE.upstream) for source attribution.
