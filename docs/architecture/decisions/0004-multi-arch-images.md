# 4. Multi-arch (linux/amd64 + linux/arm64) image publish

## Status

Accepted — `cmd/decision-gateway/Dockerfile` builds with `--platform=$BUILDPLATFORM` on the build stage and cross-compiles via `GOARCH=${TARGETARCH:-amd64}`; the CI image-publish job passes `platforms: linux/amd64,linux/arm64` to `docker/build-push-action` so every published tag is a manifest list. Mirror of markup-svc/ADR-0018 + traffic-gen/ADR-0005 — same problem, same fix, same posture.

## Context

The cross-service trace instrumented across the platform (this repo's ADR-0002 + markup-svc/ADR-0017 + traffic-gen/ADR-0004) measured ~1.7ms of network round-trip + connection-pool overhead between traffic-gen → gateway → markup-svc in a 2.0ms total request on an Apple Silicon dev box pulling the published amd64 images. The bulk of that 1.7ms is Rosetta-2 emulation, not actual wire time. Multi-arch images mean the trace's per-hop network cost becomes representative of native performance.

Two design questions; the rationale lives in `markup-svc/ADR-0018`. Quick recap so this ADR stands on its own:

1. **Cross-compile vs QEMU emulation.** Pick cross-compile. `--platform=$BUILDPLATFORM` on FROM + `GOARCH=$TARGETARCH` on `go build` keeps the build stage native (no QEMU slowdown).
2. **Manifest list vs per-arch tags.** Pick manifest list. `docker pull ghcr.io/helmedeiros/decision-gateway:vN` resolves automatically; no operator-visible change.

## Decision

`cmd/decision-gateway/Dockerfile`:

- Build stage `FROM` gains `--platform=$BUILDPLATFORM`.
- `ARG BUILDPLATFORM`, `ARG TARGETOS`, `ARG TARGETARCH` declared.
- `go build` invocation uses `GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}` (defaults preserve `docker build` without buildx).
- `COPY go.mod go.sum ./` — the placeholder comment about go.sum landing in a future release is now stale; OTel deps from v0.0.2 brought go.sum in.

`.github/workflows/ci.yml` image job:

- `docker/build-push-action@v5` step gains `platforms: linux/amd64,linux/arm64`.
- `docker/setup-buildx-action@v3` already present.

## Consequences

### Closed by this ADR

- `docker pull ghcr.io/helmedeiros/decision-gateway:vN` on Apple Silicon returns the arm64 variant; the canonical compose stack runs natively. The Jaeger trace's per-hop network cost on Apple Silicon dev boxes drops to ~50-100µs per hop (matching production amd64 numbers).
- arm64 production targets (Graviton) are unlocked.

### NOT closed by this ADR

- linux/arm/v7 (32-bit ARM). Lands when an operator's target asks.
- Per-platform image-size budget.

### Performance impact

- CI build: +30 seconds (one extra cross-compile invocation); cache hits keep steady-state close to original.
- Pull on Apple Silicon: drops Rosetta-2 emulation; trace's per-hop cost drops to native wire time.
- Runtime: zero difference between native amd64 and native arm64.

### Validation strategy

- Local: `docker buildx build --platform linux/amd64,linux/arm64 -f cmd/decision-gateway/Dockerfile .` produces a manifest list.
- CI: builds + pushes both arches on main + tag.
- Integration smoke: bring up the canonical compose stack on Apple Silicon native; observe trace per-hop cost dropping ~10x.
