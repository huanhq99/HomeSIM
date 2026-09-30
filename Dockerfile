ARG RUNTIME_IMAGE=debian:bookworm-slim
FROM golang:1.26.3-bookworm AS build
WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /homesim ./cmd/homesim

FROM ${RUNTIME_IMAGE}
ARG DEBIAN_MIRROR=deb.debian.org
RUN sed -i "s|deb.debian.org|${DEBIAN_MIRROR}|g" /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::http::Timeout=25 -o Acquire::Retries=1 update && apt-get -o Acquire::http::Timeout=25 -o Acquire::Retries=1 install -y --no-install-recommends alsa-utils ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /homesim /usr/local/bin/homesim
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/homesim/
ENV HOMESIM_DATA=/data HOMESIM_LISTEN=:8580
EXPOSE 8580/tcp 8590-8600/udp
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s CMD ["/usr/local/bin/homesim", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/homesim"]
