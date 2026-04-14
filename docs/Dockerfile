FROM hugomods/hugo:exts@sha256:dcbc5b53c4e53bb74a7c986caf58a3954395a1e9d8dc9bb492f2c610a7cf6eb0

# Install PostCSS dependencies. Installed outside /src so volume mounts
# don't shadow them. Versions are declared in package.json and tracked
# by Dependabot.
WORKDIR /deps
COPY package.json package-lock.json ./
RUN npm ci
ENV NODE_PATH=/deps/node_modules
ENV PATH="/deps/node_modules/.bin:${PATH}"

# Pre-fetch Hugo modules for faster builds.
WORKDIR /src
COPY go.mod go.sum hugo.yaml ./
RUN hugo mod get

ENV GOBIN=/usr/bin
RUN go install github.com/elastic/crd-ref-docs@v0.3.0