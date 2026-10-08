# syntax=docker/dockerfile:1

# Keep in sync with the go version in mise.toml.
# Build on the native platform and cross-compile, so multi-arch images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.1@sha256:162be5298a40ed317005c8339c6de4d10d3eef336d66dc8e9259b03ab9d3a6d2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mcp-profiles ./cmd/mcp-profiles

# The image runs only streamable HTTP upstreams: it has no runtime for stdio upstreams such as npx.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
LABEL org.opencontainers.image.source=https://github.com/386jp/mcp-profiles
COPY --from=build /out/mcp-profiles /mcp-profiles
ENV MCP_PROFILES_CONFIG=/etc/mcp-profiles/config.json
EXPOSE 8000
ENTRYPOINT ["/mcp-profiles"]
