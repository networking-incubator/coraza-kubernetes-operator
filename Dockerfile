# ------------------------------------------------------------------------------
# Builder
# ------------------------------------------------------------------------------

FROM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS builder

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
# coraza.disabled_operators.validateSchema: envoy-coraza is built without this
# operator, so the operator does not compile it either.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -o manager -tags 'no_fs_access coraza.no_memoize coraza.disabled_operators.validateSchema' ./cmd/manager

# ------------------------------------------------------------------------------
# Final
# ------------------------------------------------------------------------------

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
