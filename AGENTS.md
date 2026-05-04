# AGENTS.md — conversation-chat

Go / Gin service that owns the full LLM conversation lifecycle: session
creation, per-turn processing (tool loop + escalation state machine),
history persistence (MongoDB), and session caching (Redis).

---

## Build & Run

```bash
go build ./...
go run ./cmd/server
```

Required env vars (see `internal/config/config.go`):

| Var | Default | Purpose |
|---|---|---|
| `SERVER_PORT` | `8082` | HTTP listen port |
| `REDIS_URL` | `redis://localhost:6379/0` | Session cache |
| `MONGO_URI` | `mongodb://localhost:27017` | Persistent store |
| `MONGO_DB` | `conversatory` | Database name |
| `ACR_SERVICE_URL` | `http://localhost:8081` | Agent config registry (→ agent-runtime) |
| `TENANT_SERVICE_URL` | `http://localhost:8080` | Tenant metadata (→ agent-runtime) |
| `OPENAI_API_KEY` | — (required) | LLM API key |
| `OPENAI_BASE_URL` | — (required) | LLM base URL (OpenRouter, etc.) |
| `AUTH_STUB` | `false` | Bypass auth service; any Bearer token accepted |
| `RABBITMQ_URL` | unset | AMQP connection string; worker disabled if absent |
| `LLM_CB_FAILURE_THRESHOLD` | `5` | Consecutive LLM errors before opening circuit |
| `LLM_CB_INTERVAL_SECONDS` | `120` | Rolling failure-count window (seconds) |
| `LLM_CB_OPEN_TIMEOUT_SECONDS` | `60` | Time circuit stays open before probing (seconds) |
| `LLM_CB_MAX_HALF_OPEN_REQUESTS` | `1` | Probe requests allowed in half-open state |

---

## HTTP API

| Method | Path | Auth role | Purpose |
|---|---|---|---|
| GET | `/api/v1/health` | public | Liveness (Redis + Mongo ping) |
| POST | `/api/v1/sessions` | `app_admin` / `internal` | Open session |
| GET | `/api/v1/sessions/:sid` | operator+ | Get session metadata |
| POST | `/api/v1/sessions/:sid/close` | `app_admin` / `internal` | Close session |
| POST | `/api/v1/sessions/:sid/turns` | `app_admin` / `internal` | Process one turn |
| GET | `/api/v1/sessions/:sid/history` | operator+ | Full conversation history |
| GET | `/api/v1/sessions/:sid/state` | operator+ | Current session state |
| POST | `/api/v1/sessions/:sid/operator-accept` | operator+ | Claim escalated session |
| POST | `/api/v1/sessions/:sid/operator-resolve` | operator+ | Resolve operator session |

---

## LLM turn loop (`internal/service/chat_service.go`)

`ProcessTurn` runs up to 5 rounds:

1. Load `ContextEnvelope` (session config snapshot) from Redis
2. Load conversation history from Redis
3. Append user turn
4. Call LLM via `llmClient.Complete()` (wrapped by circuit breaker — see below)
5. Parse structured JSON response `{ action, message }`
6. If `action = "tool_call"`: execute via HTTP to the data-source URL,
   append result to history, loop
7. If `action = "none"` / `"close_session"`: persist turns to MongoDB
   (background goroutine), return reply text
8. If `action = "escalate"`: transition state, add to operator queue

---

## LLM circuit breaker (`internal/clients/llm/circuit_breaker.go`)

`CircuitBreakerClient` wraps any `LLMClient` with a three-state circuit
breaker and is a transparent drop-in via the `LLMClient` interface.

### States

```
            FailureThreshold             OpenTimeout
 Closed ──────────────────────► Open ───────────────► Half-Open
   ▲                                                       │
   │  probe succeeds                                       │ probe fails → Open
   └───────────────────────────────────────────────────────┘
```

- **Closed** — normal; consecutive failures counted in a rolling `Interval` window
- **Open** — fail-fast; returns `apperrors.ErrLLMCircuitOpen` without calling the provider
- **Half-Open** — one probe request allowed; success closes the circuit

### Failure classification

Only non-nil errors from `llmClient.Complete` count as failures (network errors,
HTTP 5xx, timeouts). JSON validation errors happen *after* a successful HTTP
call inside `callLLM` and never reach this layer, so they are not counted.

### Timeout defaults — sized for long tool chains

A single conversation turn can trigger up to 5 LLM round-trips, each taking
20–40 s with a large model. The defaults are intentionally wide to avoid false
trips during normal long-running chains:

| Parameter | Default | Env var | Rationale |
|---|---|---|---|
| `FailureThreshold` | 5 | `LLM_CB_FAILURE_THRESHOLD` | Requires 5 consecutive provider errors before opening |
| `Interval` | 120 s | `LLM_CB_INTERVAL_SECONDS` | Rolling window wider than worst-case turn duration |
| `OpenTimeout` | 60 s | `LLM_CB_OPEN_TIMEOUT_SECONDS` | Time open before probing; enough for provider recovery |
| `MaxHalfOpenRequests` | 1 | `LLM_CB_MAX_HALF_OPEN_REQUESTS` | One probe at a time; prevents thundering herd |

### User-facing messages

| Condition | Spanish message |
|---|---|
| Circuit open | `"El servicio de IA no está disponible en este momento. Por favor intenta en unos minutos."` |
| Other LLM error | `"Lo siento, ocurrió un error. Por favor intenta de nuevo."` |
| Max tool iterations exceeded | `"Lo siento, no pude completar la operación. Por favor intenta de nuevo."` |

### State change logging

Every state transition is logged at `WARN` level with fields `from`, `to`,
and `consecutive_failures`. Watch these logs to detect provider instability.

---

## Async worker (`internal/worker/worker.go`)

The worker enables asynchronous LLM processing triggered by
`UN_agent_runtime_ms` via RabbitMQ. It runs as a background goroutine
alongside the HTTP server.

### Flow

```
consume chat_requests
    → unmarshal ChatJob { job_id, session_id, message, chat_id, tenant_id }
    → chatService.ProcessTurn(ctx, session_id, TurnRequest{...})
    → publish ChatResult { job_id, chat_id, session_id, text } → chat_results
    → Ack delivery
```

On `ProcessTurn` error: publishes a result with `text: ""` so the
caller (chat-orch) is not left hanging, then Nacks with `requeue: false`.

### Queue shapes

**Consumed from `chat_requests`:**
```json
{
  "job_id":    "b3d1a4c2-...",
  "chat_id":   123456789,
  "tenant_id": "demo-tenant",
  "session_id": "sess_abc...",
  "message":   "user text"
}
```

**Published to `chat_results`:**
```json
{
  "job_id":    "b3d1a4c2-...",
  "chat_id":   123456789,
  "session_id": "sess_abc...",
  "text":      "LLM reply text"
}
```

### Startup behaviour

- Retries RabbitMQ connection up to 5 times with 2 s backoff
- If `RABBITMQ_URL` is empty, the worker is skipped (HTTP path still works)
- Worker goroutine logs and stops on channel close; the HTTP server
  continues unaffected

---

## Data stores

| Store | Key pattern | Content |
|---|---|---|
| Redis | `ctx:{sid}` | `ContextEnvelope` (session config snapshot) |
| Redis | `hist:{sid}` | Conversation history (list of `Turn`) |
| Redis | `state:{sid}` | Session state string |
| Redis | `op_queue:{tenant_id}` | Escalation queue (sorted set) |
| MongoDB | `sessions_{tenantSlug}` | `SessionRecord` with full audit trail |

---

## Common tasks

- **Change LLM system prompt / tools:** edit `internal/agents/hospital.ts`
  in **agent-runtime** (conversation-chat reads it via ACR client at session open).
- **Add a new session handler:** add to `internal/handler/`, wire in
  `internal/router/router.go`.
- **Inspect audit collection:**
  ```bash
  docker compose exec mongo mongosh conversatory \
    --eval 'db.getCollectionNames()'
  ```
