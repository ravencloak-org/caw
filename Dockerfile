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

# ---- Final -----------------------------------------------------------------
# distroless/static: no shell, no libc — just the static binary. nonroot user.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

# Static binary only — keeps the image tiny.
COPY --from=builder /out/hub /hub

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/hub"]
