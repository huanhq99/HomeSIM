# syntax=docker.io/docker/dockerfile:1.20.0@sha256:26147acbda4f14c5add9946e2fd2ed543fc402884fd75146bd342a7f6271dc1d

ARG GO_BUILDER_IMAGE
FROM --platform=linux/amd64 ${GO_BUILDER_IMAGE} AS build

ARG SOURCE_DATE_EPOCH
ARG SOURCE_REVISION
ARG SOURCE_CREATED_RFC3339
ARG SOURCE_TREE_SHA256
ARG SOURCE_LOCK_SHA256
ARG TOOLCHAIN_LOCK_SHA256

ENV CGO_ENABLED=0 \
    GOARCH=amd64 \
    GOOS=linux \
    GOPROXY=https://proxy.golang.org \
    GOSUMDB=sum.golang.org \
    GOTOOLCHAIN=local \
    GOWORK=off

WORKDIR /src
COPY go.mod go.sum ./
COPY third_party ./third_party
COPY internal/publicedge ./internal/publicedge
COPY internal/publicedgeserver ./internal/publicedgeserver
COPY cmd/djonehub-edge ./cmd/djonehub-edge
COPY image/healthcheck.go ./image/healthcheck.go

# The only remote source fetched during the build is the module content pinned
# by go.mod/go.sum. Compilation itself is network-isolated. Two clean builds
# must be byte-identical before either binary enters the runtime rootfs.
RUN go mod download golang.org/x/net@v0.50.0
RUN --network=none set -eu; \
    test -n "${SOURCE_TREE_SHA256}"; test -n "${SOURCE_REVISION}"; \
    test -n "${SOURCE_CREATED_RFC3339}"; test -n "${SOURCE_DATE_EPOCH}"; \
    test -n "${SOURCE_LOCK_SHA256}"; test -n "${TOOLCHAIN_LOCK_SHA256}"; \
    mkdir -p /out/first /out/second /out/rootfs/etc/ssl/certs; \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags='-buildid= -s -w' \
      -o /out/first/djonehub-edge ./cmd/djonehub-edge; \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags='-buildid= -s -w' \
      -o /out/first/djonehub-edge-healthcheck ./image/healthcheck.go; \
    go clean -cache; \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags='-buildid= -s -w' \
      -o /out/second/djonehub-edge ./cmd/djonehub-edge; \
    go build -mod=readonly -trimpath -buildvcs=false -ldflags='-buildid= -s -w' \
      -o /out/second/djonehub-edge-healthcheck ./image/healthcheck.go; \
    cmp /out/first/djonehub-edge /out/second/djonehub-edge; \
    cmp /out/first/djonehub-edge-healthcheck /out/second/djonehub-edge-healthcheck; \
    cp /out/first/djonehub-edge /out/rootfs/djonehub-edge; \
    cp /out/first/djonehub-edge-healthcheck /out/rootfs/djonehub-edge-healthcheck; \
    cp /etc/ssl/certs/ca-certificates.crt /out/rootfs/etc/ssl/certs/ca-certificates.crt; \
    chmod 0555 /out/rootfs/djonehub-edge /out/rootfs/djonehub-edge-healthcheck; \
    chmod 0444 /out/rootfs/etc/ssl/certs/ca-certificates.crt; \
    touch -d "@${SOURCE_DATE_EPOCH}" \
      /out/rootfs/djonehub-edge \
      /out/rootfs/djonehub-edge-healthcheck \
      /out/rootfs/etc/ssl/certs/ca-certificates.crt; \
    find /out/rootfs -type d -exec touch -d "@${SOURCE_DATE_EPOCH}" {} +

FROM scratch
ARG SOURCE_DATE_EPOCH
ARG SOURCE_REVISION
ARG SOURCE_CREATED_RFC3339
ARG SOURCE_TREE_SHA256
ARG SOURCE_LOCK_SHA256
ARG TOOLCHAIN_LOCK_SHA256
LABEL org.opencontainers.image.title="DJOneHub public edge" \
      org.opencontainers.image.description="Fail-closed read-only public control edge" \
      org.opencontainers.image.source="https://github.com/example/maccellular" \
      org.opencontainers.image.revision="${SOURCE_REVISION}" \
      org.opencontainers.image.created="${SOURCE_CREATED_RFC3339}" \
      io.maccellular.source-tree-sha256="${SOURCE_TREE_SHA256}" \
      io.maccellular.source-lock-sha256="${SOURCE_LOCK_SHA256}" \
      io.maccellular.toolchain-lock-sha256="${TOOLCHAIN_LOCK_SHA256}" \
      io.maccellular.runtime="scratch-no-shell" \
      io.maccellular.surface="public-read-only"
COPY --from=build /out/rootfs/ /
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/djonehub-edge"]
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 CMD ["/djonehub-edge-healthcheck"]
