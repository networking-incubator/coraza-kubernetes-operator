# ------------------------------------------------------------------------------
# Builder
# ------------------------------------------------------------------------------

FROM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download

COPY . .

# coraza.no_memoize: validation compiles untrusted rule text, and a failed parse
# returns a nil WAF that cannot release the memoizer it registered, so it leaks.
# The operator runs no WAF transactions, so nothing benefits from the cache.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -o manager -tags 'no_fs_access coraza.no_memoize' ./cmd/manager

# ------------------------------------------------------------------------------
# Final
# ------------------------------------------------------------------------------

FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
