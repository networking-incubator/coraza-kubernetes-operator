ARG OPM_VERSION=v1.64.0

FROM quay.io/operator-framework/opm:${OPM_VERSION} AS opm

FROM registry.access.redhat.com/ubi9/ubi-minimal AS builder

COPY --from=opm /bin/opm /bin/opm

RUN mkdir -p /etc/containers && \
    echo '{"default":[{"type":"insecureAcceptAnything"}]}' > /etc/containers/policy.json

COPY coraza-kubernetes-operator /configs/coraza-kubernetes-operator
COPY bundles/ /tmp/bundles/

ARG BUNDLE_IMGS
RUN for img in ${BUNDLE_IMGS}; do \
      /bin/opm render "$img" -o yaml >> /configs/coraza-kubernetes-operator/catalog.yaml; \
    done && \
    /bin/opm validate /configs

FROM opm

COPY --from=builder /configs /configs

RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

LABEL operators.operatorframework.io.index.configs.v1=/configs

ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]
