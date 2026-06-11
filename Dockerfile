# syntax=docker/dockerfile:1

# ---- Builder ---------------------------------------------------------------
# Pure-Go build (modernc.org/sqlite) => CGO_ENABLED=0, no C toolchain needed.
FROM golang:1.26 AS builder

WORKDIR /src

# Cache module downloads as their own layer.
COPY go.mod go.sum ./
RUN go mod download

# Build the static hub binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/hub ./cmd/hub

# Pre-create the data dir so a fresh named volume mounted at /data inherits
# nonroot (uid 65532) ownership — distroless has no shell to chown at runtime.
RUN mkdir -p /data

# ---- Final -----------------------------------------------------------------
# distroless/static: no shell, no libc — just the static binary. nonroot user.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

# Static binary only — keeps the image tiny.
COPY --from=builder /out/hub /hub

# Data dir owned by the nonroot uid (65532) so SQLite can create the DB file
# even when a fresh named volume is mounted over /data.
COPY --from=builder --chown=65532:65532 /data /data

# Default the DB onto the data dir so `docker run` works without extra env.
ENV CAW_DB=/data/caw.db

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/hub"]
