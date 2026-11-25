# BPS authorization plugin

[简体中文](README_CN.md)

This Go C ABI plugin adds independent BPS browser and device authorization to CPA. It adapts
the OAuth implementation from [bps-local-public](https://github.com/sunboyzoey/bps-local-public)
and reuses CPA's plugin loader, command-line flags, credential store, proxy
configuration, v8 management sessions, and manual credential refresh.

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

Use `gpt-6-astra`, `gpt-6-sol`, `gpt-6.2-astra`, or `gpt-6.2-sol`.
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
Duplicate execution IDs are ignored. Failed or unpriced usage, quota resets,
decreasing utilization, plan changes, and pricing changes restart calibration.
Samples are in memory only and reset when the plugin restarts or is re-registered.

These amounts are **estimated API value, not an official or redeemable account
balance**. External usage on the same upstream account, changing model mix, rounded
quota percentages, and asynchronous reporting affect accuracy. They do not apply
Sub2API user discounts or group multipliers, and do not convert upstream credits
into dollars. Each quota window is estimated independently; do not sum windows.

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
