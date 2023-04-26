# 8. Hot reload of the route table via `POST /admin/routes`

## Status

Accepted — `internal/proxy.Holder` wraps a `*Handler` behind a `sync.RWMutex` (mirroring markup-svc/ADR-0015's `swap.Decider` shape). `Holder.ServeHTTP` takes the RLock just long enough to copy the active handler pointer + release; `Holder.Replace(router)` rebuilds the underlying `*Handler` using the captured `BuildConfig` and WLocks the swap. `httpapi.RoutesAdmin` mounts on `POST /admin/routes` (replace) + `GET /admin/routes` (read current table). `cmd --routes-admin` (default off for backwards compatibility) wires the Holder into the request path and mounts the admin endpoint.

## Context

Operators changing the gateway's route table today (`--route=PREFIX=>BACKEND` flags) require a rolling restart of the gateway. That's tolerable for occasional reconfigurations but breaks two real workflows: (1) shifting a route to a canary backend mid-day, (2) rolling a backend out of service for maintenance by re-pointing its prefix. The markup-svc/ADR-0015 pattern (Holder + admin endpoint + atomic-swap) already exists in the platform for guardrails hot-reload; this ADR ports the same shape to the route table.

Two design questions.

### 1. Atomic pointer vs sync.RWMutex

markup-svc/ADR-0015 chose `sync.RWMutex` because `atomic.Pointer[T]` is Go 1.19+ and the platform's baseline is 1.18. The performance was measured at ~10 ns per `Decide` lock pair on the markup-svc hot path.

The same constraint holds here. The lock-pair cost on every request is acceptable (~30 ns including the dispatch hand-off through a wrapper), and the implementation matches the existing platform pattern.

**Pick `sync.RWMutex`.** Matches the platform.

### 2. Hot reload semantics — replace the whole table vs add/remove/edit individual routes

Two API shapes:

- **Whole-table replace**: `POST /admin/routes {"routes":[...]}` swaps the entire table atomically. Pros: atomic, idempotent (operator-side computed desired state), no half-state races. Cons: the operator has to send the full table even for a single-route change.
- **Per-route ops**: `PATCH /admin/routes`, `DELETE /admin/routes/<prefix>`, `PUT /admin/routes/<prefix>`. Pros: smaller patches. Cons: every op needs validation against the table-after-change; needs additional rules (e.g., what if the patch leaves zero routes?); per-route races between concurrent ops.

**Pick whole-table replace.** Idempotent + atomic + composable with existing config-file-driven operator workflows (operator regenerates the desired state from a source-of-truth file and POSTs it). Matches the markup-svc/ADR-0015 guardrails pattern: POST replaces the whole rule set.

## Decision

`internal/proxy/holder.go`:

```go
type BuildConfig struct {
    BackendTimeout   time.Duration
    Pool             PoolConfig
    Protocol         UpstreamProtocol
    TransportWrapper func(http.RoundTripper) http.RoundTripper
}

type Holder struct {
    cfg     BuildConfig
    mu      sync.RWMutex
    current *Handler
}

func NewHolder(initialRouter *gateway.Router, cfg BuildConfig) (*Holder, error)
func (h *Holder) ServeHTTP(w http.ResponseWriter, r *http.Request)
func (h *Holder) Replace(router *gateway.Router) error
func (h *Holder) Routes() []gateway.Route
```

The Holder closes over `BuildConfig` once at construction so every rebuilt `*Handler` keeps the same `BackendTimeout` / `Pool` / `Protocol` / OTel wrapper. `Replace` builds the new `*Handler` before taking the WLock so the lock window is just the pointer swap.

`internal/httpapi/routes_admin.go`:

`RoutesAdmin(h *proxy.Holder, errLog io.Writer) http.Handler`. `GET` returns the current table as `{"routes":[{"prefix":"...","backend":"..."}]}`. `POST` decodes the same shape, validates each backend URL (`url.Parse` + non-empty Scheme + Host), constructs a fresh `gateway.Router` (which catches duplicate-prefix / empty-prefix / nil-backend errors), and calls `Holder.Replace`. Validation failures return `400` with a JSON error body and leave the old table serving. The 4-test suite covers GET, POST success, invalid backend, empty array.

`cmd/decision-gateway`:

- New `--routes-admin` flag (default off). When set, the cmd builds a `proxy.Holder` instead of a plain `*proxy.Handler`, mounts `/admin/routes` on the mux, and wires the Holder into the catch-all path.
- When off, the legacy `proxy.New(...)` path stays — no Holder, no admin endpoint, no lock pair. Same v0.0.7 binary behavior.

## Consequences

### Closed

- Operators reconfigure routes without restart. `curl -X POST http://gateway:8090/admin/routes -d '{"routes":[...]}'` swaps atomically; in-flight requests on the previous routes complete normally; new requests use the new table.
- The admin endpoint mirrors the markup-svc/ADR-0015 / ADR-0008 patterns so operators familiar with one already know the other.
- Validation rejects bad input before the swap. Empty array, invalid URL, duplicate prefix, empty prefix all return `400` and leave the previous table serving.

### Not closed

- Authentication on the admin endpoint. v0.0.x runs the admin path open per the dev posture; production deployments gate via mTLS or a bearer-token middleware. Out of scope today; same gap markup-svc/ADR-0015 leaves.
- Audit log of who reloaded what. The `gateway.access` JSON event still records the POST but no operator-identity is captured. Production deployments layer an audit middleware.
- Per-route weighted distribution (canary traffic splitting). Lands in a separate ADR if a real consumer asks; this ADR ships only the static route table.
- Persistence of the live route table. A restart reverts to whatever the `--route` flags say. Production deployments would write the live table to durable storage and replay on startup; out of scope for the dev compose.

### Performance impact

- `--routes-admin` not set: zero ns delta vs v0.0.7. The legacy `*proxy.Handler` is used directly; no lock pair per request.
- `--routes-admin` set: per-request RLock + RUnlock pair (~30 ns) inside `Holder.ServeHTTP` for the pointer copy. Below the engine + network noise floor of the rest of the platform stack.
- The Replace call: ~50 µs to rebuild the per-route `*httputil.ReverseProxy` map (one `newReverseProxy` per route × the existing transport setup). Operator-triggered, not on the hot path.
