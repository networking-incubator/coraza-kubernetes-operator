FROM hugomods/hugo:dart-sass-go-git-0.165.0@sha256:c2b698012fd3f246161bd4dbb8da4f4064cfd1ac92d8eedf7b926eb619c79899

# The dart-sass-go-git image ships Hugo extended 0.165.0, Go, Dart Sass,
# and Git. Node/npm are needed for PostCSS and are installed via apk.
RUN apk add --no-cache nodejs npm

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