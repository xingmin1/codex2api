# Codex Proxy Contracts

> Executable cross-layer contracts for Responses translation, transport fallback, retry ownership, and persisted runtime settings.

## Scenario: Preserve Local Codex Semantics During Upstream Merges

### 1. Scope / Trigger

- Trigger: Any change to Responses HTTP/WebSocket/compact handling, relay-style account routing, encrypted content recovery, or `SystemSettings` fields.
- Scope: `proxy/`, `auth/`, `admin/`, `database/`, and `frontend/src/` must be reviewed as one contract.
- Goal: Accept compatible client payloads without changing account ownership, retry budgets, persisted settings, or protocol-visible terminal events.

### 2. Signatures

- Request preparation:
  - `PrepareResponsesBody(rawBody []byte) ([]byte, string)`
  - `PrepareResponsesWebSocketBody(rawBody []byte) ([]byte, string)`
  - `PrepareCompactResponsesBody(rawBody []byte) ([]byte, string)`
- Entry points:
  - `(*Handler).Responses(c *gin.Context)`
  - `(*Handler).ResponsesWebSocket(c *gin.Context)`
  - `(*Handler).ResponsesCompact(c *gin.Context)`
- Account transport:
  - `(*auth.Account).IsRelayStyle() bool`
  - `ExecuteRelayStyleRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error)`
- Persisted settings:
  - `(*database.DB).GetSystemSettings(ctx context.Context) (*database.SystemSettings, error)`
  - `(*database.DB).UpdateSystemSettings(ctx context.Context, settings *database.SystemSettings) error`
  - `(*admin.Handler).GetSettings(c *gin.Context)`
  - `(*admin.Handler).UpdateSettings(c *gin.Context)`

### 3. Contracts

- A text-only Responses `role=system` item is removed from `input[]` and appended to top-level `instructions`. A system item containing non-text content is converted to `role=developer` instead.
- HTTP, WebSocket, and compact Codex preparation share that system-message rule. OpenAI relay preparation preserves the original Responses payload shape.
- A WebSocket close `1009` before any client-visible output retains the current account lease and retries that account over HTTP before normal same-account retry logic. This protocol fallback does not consume the ordinary transport retry budget.
- Relay-style accounts, including Grok, are detected through `IsRelayStyle()` and executed through `ExecuteRelayStyleRequest`; Codex-specific cooldown and account-disable semantics must not be applied to them.
- Retryable `response.failed` events before visible output may be suppressed only when another attempt is guaranteed. Once output is committed, the terminal event must be forwarded and the request must not be replayed transparently.
- Invalid `encrypted_content` recovery is bounded to one sanitization/replay cycle for the request. It must keep the selected account when the same-account path is still valid.
- Native compact responses must preserve the compaction request fields and produce exactly one valid compaction output item before reporting success.
- Every `SystemSettings` field exposed by the admin API must round-trip through frontend types, admin request/response structs, database scan/select/update code, and runtime application. PostgreSQL column order, scan order, placeholders, and argument order must remain identical.
- Partial settings updates preserve unrelated persisted fields; a failed persistence operation must not apply the new runtime value.

### 4. Validation & Error Matrix

| Condition | Required behavior |
|-----------|-------------------|
| Text-only system item | Move text to top-level `instructions`; remove the item from `input[]` |
| Non-text system item | Preserve content and change the role to `developer` |
| WebSocket `1009` before visible output | Keep the lease, force HTTP on the same account, do not consume normal retry budget |
| WebSocket `1009` after visible output | Forward terminal failure; do not transparently replay |
| Retryable `response.failed` before visible output | Suppress only when a bounded retry will run |
| Invalid `encrypted_content` before visible output | Strip invalid encrypted fields once, then replay according to the same-account policy |
| `ws_busy_acquire` | Do not enter ordinary same-account transport retry |
| Relay/Grok 429 or transport failure | Use relay-style accounting and cooldown mapping, not Codex account punishment |
| Invalid settings value | Return a client validation error and leave database/runtime state unchanged |
| Database write failure | Return failure and do not apply the requested runtime setting |
| PostgreSQL scan/update count mismatch | Treat as a release blocker; fix query, scan, placeholder, and argument order together |

### 5. Good / Base / Bad Cases

- Good: A WebSocket request closes with `1009` before output, reuses the same account over HTTP, succeeds, and releases the lease once.
- Good: A partial admin settings update changes one field while all Grok, retry, compaction, and quality fields survive a database round trip.
- Base: A normal Codex Responses request uses the selected transport and forwards one terminal success event without retry state.
- Bad: A handler releases or unbinds the account before HTTP fallback, causing the retry to select a different account.
- Bad: A new settings field is added to the frontend and SQLite path but omitted from PostgreSQL scan or update arguments.
- Bad: A stream forwards `response.failed` and then starts a transparent retry, producing two terminal outcomes for one client request.

### 6. Tests Required

- Translator tests must assert system-message behavior for HTTP, WebSocket, compact, non-text fallback, and OpenAI relay preservation.
- Handler and WebSocket tests must assert lease identity, retry-budget counters, first-output boundaries, `1009` HTTP fallback ordering, encrypted-content one-shot recovery, and terminal-event cardinality.
- Relay/Grok tests must assert `IsRelayStyle()` routing and that Codex-specific penalties are not applied.
- SQLite and PostgreSQL settings tests must assert full round trips, partial-update preservation, validation rollback, and exact query/argument alignment.
- Frontend validation must include TypeScript type checking and component tests/build after settings or account-detail fields change.
- Release validation must run `go test ./... -count=1`, frontend typecheck/tests/build, a Nix package build, isolated `/health`, and SIGTERM graceful-shutdown verification before production cutover.

### 7. Wrong vs Correct

#### Wrong

```go
if websocketMessageTooBig {
    lease.Release()
    retryNormally()
}
```

This loses account identity and incorrectly charges a protocol fallback against the normal retry path.

#### Correct

```go
if websocketMessageTooBigBeforeOutput {
    fallback.Retain(account, proxyURL, wsElapsed, "peer_close")
    if fallback.ForceHTTP() {
        continue
    }
}
```

The fallback owns the existing lease until the same account completes over HTTP or reaches a terminal result.
