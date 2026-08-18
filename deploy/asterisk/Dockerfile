# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG DEBIAN_IMAGE=debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241

FROM ${DEBIAN_IMAGE} AS builder

ARG ASTERISK_VERSION=22.10.1
ARG ASTERISK_SHA256=0953564c44fa49827f3c9d70ca6e80db83828c9848440852c6be44c961855353
ARG PJPROJECT_VERSION=2.17
ARG PJPROJECT_SHA256=04b2eb1f0f01aa0ad1945b167171843448a51aa6b7c3e806496d434f13a112b7
ARG BUILD_JOBS=2

SHELL ["/bin/sh", "-euxc"]

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        build-essential \
        bzip2 \
        ca-certificates \
        curl \
        libcurl4-openssl-dev \
        libedit-dev \
        libjansson-dev \
        libncurses-dev \
        libsrtp2-dev \
        libsqlite3-dev \
        libssl-dev \
        liburiparser-dev \
        libuuid1 \
        libxml2-dev \
        libxslt1-dev \
        patch \
        pkg-config \
        python3 \
        uuid-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

RUN archive="asterisk-${ASTERISK_VERSION}.tar.gz" \
    && curl --fail --location --proto '=https' --tlsv1.2 \
        --output "${archive}" \
        "https://downloads.asterisk.org/pub/telephony/asterisk/${archive}" \
    && printf '%s  %s\n' "${ASTERISK_SHA256}" "${archive}" | sha256sum --check --strict \
    && mkdir source \
    && tar -xzf "${archive}" --strip-components=1 -C source

# Asterisk's bundled pjproject rule otherwise fetches a mutable branch URL and
# verifies only upstream's MD5 sidecar. Pre-populate its exact download cache
# and enforce a separately pinned SHA-256 before configure/make can use it.
RUN pjarchive="pjproject-${PJPROJECT_VERSION}.tar.bz2" \
    && curl --fail --location --proto '=https' --tlsv1.2 \
        --output "/tmp/${pjarchive}" \
        "https://raw.githubusercontent.com/asterisk/third-party/master/pjproject/${PJPROJECT_VERSION}/${pjarchive}" \
    && printf '%s  %s\n' "${PJPROJECT_SHA256}" "/tmp/${pjarchive}" | sha256sum --check --strict

WORKDIR /build/source

# Debian exposes /var/run as a symlink to /run. Asterisk's staged install
# creates a real /var/run directory, which BuildKit correctly refuses to
# overlay onto that symlink. Runtime state is created explicitly under
# /run/asterisk below, so the staged duplicate is removed before image copy.
RUN --network=none ./configure \
        --prefix=/usr \
        --sysconfdir=/etc \
        --localstatedir=/var \
        --with-pjproject-bundled \
        --with-crypto \
        --with-ssl \
        --with-srtp \
        --without-dahdi \
        --without-pri \
        --without-x11 \
    && make menuselect.makeopts \
    && menuselect/menuselect \
        --disable BUILD_NATIVE \
        --disable-category MENUSELECT_CORE_SOUNDS \
        --disable-category MENUSELECT_MOH \
        --disable-category MENUSELECT_EXTRA_SOUNDS \
        menuselect.makeopts \
    && make -j"${BUILD_JOBS}" \
    && make DESTDIR=/opt/asterisk-root install \
    && rm -rf /opt/asterisk-root/var/run \
    && install -d -m 0755 /opt/asterisk-root/usr/share/doc/asterisk \
    && install -m 0644 LICENSE COPYING /opt/asterisk-root/usr/share/doc/asterisk/ \
    && install -m 0644 third-party/pjproject/source/COPYING \
        /opt/asterisk-root/usr/share/doc/asterisk/PJPROJECT-COPYING

# modules.conf prevents implicit startup loading, but ARI's module API can still
# load any module file present on disk. Make the image itself the enforcement
# boundary: validate the canonical manifest, copy only those 35 files into a
# fresh directory, then prove that directory has no additional entry.
COPY canonical-modules.txt /build/canonical-modules.txt

RUN manifest=/build/canonical-modules.txt \
    && modules=/opt/asterisk-root/usr/lib/asterisk/modules \
    && approved=/opt/asterisk-approved-modules \
    && test -f "${manifest}" \
    && test ! -L "${manifest}" \
    && awk 'NF != 1 || $0 !~ /^[a-z0-9_]+[.]so$/ { exit 1 } END { if (NR != 35) exit 1 }' "${manifest}" \
    && LC_ALL=C sort -u "${manifest}" | cmp -s "${manifest}" - \
    && install -d -m 0755 "${approved}" \
    && while IFS= read -r module; do \
         test -f "${modules}/${module}"; \
         test ! -L "${modules}/${module}"; \
         install -m 0755 "${modules}/${module}" "${approved}/${module}"; \
       done <"${manifest}" \
    && rm -rf /opt/asterisk-root/usr/lib/asterisk/modules \
    && mv "${approved}" "${modules}" \
    && LC_ALL=C find "${modules}" -mindepth 1 -maxdepth 1 -printf '%f\n' \
         | LC_ALL=C sort | cmp -s "${manifest}" -

FROM ${DEBIAN_IMAGE} AS runtime

SHELL ["/bin/sh", "-euxc"]

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        libcurl4 \
        libedit2 \
        libjansson4 \
        libncurses6 \
        libsrtp2-1 \
        libsqlite3-0 \
        libssl3 \
        liburiparser1 \
        libuuid1 \
        libxml2 \
        libxslt1.1 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 asterisk \
    && useradd --uid 10001 --gid 10001 --home-dir /var/lib/asterisk \
        --no-create-home --shell /usr/sbin/nologin asterisk

COPY --from=builder /opt/asterisk-root/ /

RUN install -d -o root -g root -m 0755 /var/lib/asterisk \
    && install -d -o asterisk -g asterisk -m 0700 \
        /var/lib/asterisk-private \
        /var/log/asterisk \
        /var/spool/asterisk \
        /run/asterisk

USER asterisk:asterisk

ENTRYPOINT ["/usr/sbin/asterisk"]
CMD ["-f", "-vvv", "-C", "/etc/asterisk/asterisk.conf"]
