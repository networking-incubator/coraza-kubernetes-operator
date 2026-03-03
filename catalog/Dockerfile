FROM registry.operatorhub.io/opm:latest

COPY coraza-kubernetes-operator /configs/coraza-kubernetes-operator

RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

LABEL operators.operatorframework.io.index.configs.v1=/configs
