# syntax=docker/dockerfile:1.7
# Build from next/: docker build -f deploy/gateway/Dockerfile -t sup2api-gateway .
# The build stage cross-compiles for the target platform; --target binaries
# exports only the two binaries (the release workflow archives them).
# Same pinned compiler as next/Dockerfile.
ARG GO_IMAGE=golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734
ARG RUNTIME_IMAGE=debian:trixie-slim
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG BUILD_MAX_PROCS=2
ARG VERSION=0.0.0-dev
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOWORK=off GOMAXPROCS=${BUILD_MAX_PROCS} GOFLAGS=-p=${BUILD_MAX_PROCS} GOOS=${TARGETOS} GOARCH=${TARGETARCH}
WORKDIR /src
COPY runtime-contract ./runtime-contract
COPY gateway ./gateway
WORKDIR /src/gateway
RUN --mount=type=cache,id=sub2api-next-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=sub2api-next-gobuild,target=/root/.cache/go-build \
    go build -trimpath -ldflags "-X main.Version=${VERSION}" -o /out/sub2api-gateway ./cmd/sub2api-gateway && \
    go build -trimpath -o /out/sub2api-release ./cmd/sub2api-release

FROM scratch AS binaries
COPY --from=build /out/ /

FROM ${RUNTIME_IMAGE}
ARG VERSION=0.0.0-dev
LABEL org.opencontainers.image.title="sub2api-gateway" \
      org.opencontainers.image.version="${VERSION}"
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 sub2api \
    && useradd -u 1000 -g sub2api -d /var/lib/sub2api -s /usr/sbin/nologin sub2api \
    && mkdir -p /var/lib/sub2api /etc/sub2api \
    && chown 1000:1000 /var/lib/sub2api
COPY --from=binaries / /usr/local/bin/
RUN ln -s sub2api-gateway /usr/local/bin/sub2api-shell
USER 1000:1000
WORKDIR /var/lib/sub2api
EXPOSE 8080 7443
STOPSIGNAL SIGTERM
ENTRYPOINT ["sub2api-gateway"]
CMD ["serve", "-config", "/etc/sub2api/shell.json"]
