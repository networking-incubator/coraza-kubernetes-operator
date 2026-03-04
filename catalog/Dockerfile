FROM registry.access.redhat.com/ubi9/ubi-minimal AS builder

COPY --from=quay.io/operator-framework/opm:latest /bin/opm /bin/opm

COPY coraza-kubernetes-operator /configs/coraza-kubernetes-operator

ARG BUNDLE_IMGS
RUN for img in ${BUNDLE_IMGS}; do \
      /bin/opm render "$img" -o yaml >> /configs/coraza-kubernetes-operator/catalog.yaml; \
    done && \
    /bin/opm validate /configs

FROM quay.io/operator-framework/opm:latest

COPY --from=builder /configs /configs

RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

LABEL operators.operatorframework.io.index.configs.v1=/configs

ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]
