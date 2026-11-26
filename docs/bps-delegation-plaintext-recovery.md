# Verified BPS delegation plaintext recovery

## Scope

This change addresses a reproduced source of `invalid_encrypted_content`: a BPS
client-tool call carries a plaintext collaboration message, then the client sends
that same message in an `agent_message.content[].encrypted_content` field.
It does not decrypt arbitrary content, discard conversation history, enable the
broader multi-agent optimization, or claim to repair every encrypted-state error.

The implementation is based on commit
`bf2609203691f70eae11b8e64df6630c6cc181ca`. No production configuration, plugin
quota state, deployment, or credentials are changed by the fix.

## Provenance and conversion

1. The BPS bridge validates the **entire** terminal response and resolves its
   tools against the current client catalog. A new `OnToolHandoff` hook runs
   immediately before the first client-facing tool frame, not after the
   arguments-done event. This avoids racing a client that starts a child as soon
   as tool arguments become available.
2. The host records only completed `collaboration.spawn_agent`,
   `collaboration.send_message`, and `collaboration.followup_task` calls.
   Namespaced and exactly qualified function names are supported. Arbitrary
   same-named tools, custom calls, malformed arguments and ambiguous JSON are
   not evidence.
3. Evidence is keyed by a caller-scoped session family, canonical author and
   recipient paths, and the SHA-256 digest of the exact message. No message
   plaintext or credential is retained in this store. Session identifiers are
   hashed; the store does retain canonical agent paths.
4. A child session must match its parent metadata **and** an exact spawn proof
   from that known parent. Conflicting candidate recipients are rejected.
   Known sessions cannot change their parent. Nested children, sibling messages
   and replies to the root are supported after this binding.
5. Only an exact, in-scope matching `encrypted_content` **part** is changed to
   `input_text/text`. The agent-message envelope, identifiers, routing, order,
   extension fields and other parts remain intact. Genuine or unknown opaque
   content, reasoning state and compaction are not modified by this helper.

The native BPS and Codex executors live in the same CPA host, so the evidence
store can be shared across BPS account rotation and BPS-to-Codex routing without
being keyed by the provider credential. This is not a cross-process cache or a
plugin RPC. Updating only a BPS shared-library artifact will not install the fix;
the CPA host needs this code.

Normalization is before final user payload rules in BPS and the Codex HTTP,
streaming, compact, WebSocket create/preparation, duplex steer and token-count
paths. User overrides and filters remain authoritative and are not restored.

## Boundaries and lifecycle

- Memory-only: at most **4096 message proofs** and **1024 session bindings**.
- Proof TTL: **24 hours**, fixed rather than refreshed by duplicate output.
  Child bindings expire no later than their parent or bootstrap proof.
- Maximum message size: **256 KiB**. Agent paths and metadata have length and
  syntax limits. Unknown, expired, over-limit or ambiguous evidence fails closed.
- Restart, routing to another host process, or capacity exhaustion loses or
  prevents provenance. Existing sessions without evidence are **not** guessed
  back into plaintext. Start a new verified delegation when necessary; persistent
  or distributed recovery would require a separate design.
- The hook is a validated executor handoff boundary, **not a client delivery
  acknowledgment**. A cancellation before validation does not record a proof.
  A cancellation racing a validated handoff may leave bounded evidence for an
  undelivered tool. Its content is still known plaintext; no execution or
  acknowledgment is inferred from that evidence.
- No background sweeper, new network timeout, automatic content-stripping retry,
  quota reset, credential penalty, or production migration is introduced.

An unhandled `invalid_encrypted_content` remains HTTP 400 / `request_scoped`,
non-retryable and not credential-scoped. The public error now gives a fixed safe
history/delegation hint rather than exposing the upstream message. Existing
upstream diagnostics retain the original error code. The normalization diagnostic
contains a count only, not message or encrypted-state contents.

## Automated coverage

The new tests cover:

- full BPS transport decoding and validation -> child request, with streaming and
  non-streaming parents;
- different BPS auth IDs and BPS -> Codex HTTP, streaming, compact, WebSocket
  execute and shared WebSocket preparation;
- immediate child input at the first tool frame, complete-response rejection,
  missing/incomplete terminal output and actual executor cancellation;
- exact caller/parent/author/recipient/content isolation, nested/sibling/reply
  flows, malformed/duplicate fields, metadata rebinding, restart, TTL, capacity
  and concurrent access;
- preservation of unknown agent ciphertext, reasoning and compaction in the
  normalization helper, and user payload override/filter precedence;
- both upstream `error` and `response.failed` classifications with one executor
  request, no retry and no disclosure of upstream private error text.

Validation commands:

```sh
go test ./internal/runtime/executor/... -count=1
go test -race ./internal/runtime/executor/... \
  -run 'TestBPSDelegation|TestValidatedToolHandoff|TestEventError' -count=1
go build -o /tmp/cpa-delegation-server ./cmd/server
git diff --check
```

Full-repository checks were also attempted. The local environment defaults to
`CGO_ENABLED=0`, which cannot compile the native BPS plugin example's
`callHostAPI`. With `CGO_ENABLED=1` that example passes, but seven dynamic-library
integration tests in `internal/pluginhost` fail during `dlopen` preflight with
Go runtime faults such as `bad sweepgen in refill` or `addspecial on invalid
pointer`. Both `TestBPSSharedLibraryImportsAndPersistsIndependentGrant` and
`TestUnixPluginReloadAfterShutdownUsesReplacedFile` reproduce the same failure
on the untouched baseline. These unrelated host/runtime failures were not
worked around or included in this fix. **The full suite is not green.**

## Opt-in real upstream verification

`TestBPSDelegationLive` is skipped by default. It takes a private credential-file
path via `CPA_BPS_DELEGATION_PROBE_AUTH`, makes four synthetic upstream requests,
and never executes the returned collaboration tool. Run only with explicit
permission and an appropriate test credential:

```sh
CPA_BPS_DELEGATION_PROBE_AUTH=/private/path/auth.json \
  go test ./internal/runtime/executor \
  -run '^TestBPSDelegationLive$' -v -count=1 -timeout=0
```

Verified on **2026-11-26, 22:14:48–22:15:01 Asia/Shanghai**, with an isolated
Linux/arm64 test executable using the modified CPA executor, `gpt-6.1-sol` and
low reasoning effort:

| Synthetic request | Result | Elapsed |
| --- | --- | ---: |
| Real parent produces a validated spawn-agent tool | Completed; no tool execution | 4456 ms |
| Exact message in encrypted field, different/unproven caller | `invalid_encrypted_content` / 400 | 1668 ms |
| Same payload, proven caller + parent relationship | Completed with expected sentinel | 3278 ms |
| Ordinary text control | Completed with expected sentinel | 3471 ms |

The successful test uses an actual upstream parent output, not a manually seeded
proof. Durations are observations, not a performance benchmark. An earlier
probe stopped after two requests because its assertion searched the sanitized
public error string for the raw upstream code; the assertion and fixed safe
error hint were corrected before the successful four-request run. Credential
files were private temporary files and were removed afterward. Production was
not restarted or deployed, and historical customer requests were not replayed.
