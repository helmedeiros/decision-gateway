# Dockerfile for the decision-gateway binary. Mirrors the
# traffic-gen and markup-svc pattern (see traffic-gen/cmd/traffic-gen/Dockerfile
# + markup-svc/cmd/markup-server/Dockerfile + markup-svc ADR-0013):
# two-stage golang:1.18 build + distroless static :nonroot runtime
# so the image runs as user 65532 with no shell, no package manager,
# and no writable filesystem outside the working directory.
#
# Build context is the repo root: `docker build -f cmd/decision-gateway/Dockerfile .`

FROM golang:1.18 AS build
WORKDIR /src

# The module has no external dependencies today; this COPY still
# benefits the build layer cache when a go.sum lands in a future
# release that adds one. The Dockerfile stays the same shape so
# future-Helio does not have to remember to update it.
COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /out/decision-gateway ./cmd/decision-gateway

FROM gcr.io/distroless/static-debian11:nonroot
COPY --from=build /out/decision-gateway /usr/local/bin/decision-gateway

# Distroless :nonroot ships USER 65532:65532. The explicit USER line
# documents the inherited posture and survives a base-image change
# that might drop the implicit default.
USER 65532:65532

EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/decision-gateway"]
