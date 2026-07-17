FROM oven/bun:1@sha256:0733e50325078969732ebe3b15ce4c4be5082f18c4ac1a0f0ca4839c2e4e42a7 AS builder

ARG APP_VERSION

WORKDIR /build/web
COPY web/package.json web/bun.lock ./
COPY web/default/package.json ./default/package.json
COPY web/classic/package.json ./classic/package.json
RUN set -eux; \
    for attempt in 1 2; do \
      if bun install --frozen-lockfile; then \
        exit 0; \
      fi; \
      echo "bun install failed (attempt ${attempt}/2); clearing download cache" >&2; \
      bun pm cache rm || rm -rf /root/.bun/install/cache; \
      sleep $((attempt * 5)); \
    done; \
    exit 1
COPY ./web/default ./default
COPY ./VERSION /build/VERSION
RUN if [ -n "$APP_VERSION" ]; then printf '%s\n' "$APP_VERSION" > /build/VERSION; fi
RUN cd default && DISABLE_ESLINT_PLUGIN='true' VITE_REACT_APP_VERSION=$(cat /build/VERSION) bun run build

FROM oven/bun:1@sha256:0733e50325078969732ebe3b15ce4c4be5082f18c4ac1a0f0ca4839c2e4e42a7 AS builder-classic

ARG APP_VERSION

WORKDIR /build/web
COPY web/package.json web/bun.lock ./
COPY web/default/package.json ./default/package.json
COPY web/classic/package.json ./classic/package.json
RUN set -eux; \
    for attempt in 1 2; do \
      if bun install --filter ./classic --frozen-lockfile; then \
        exit 0; \
      fi; \
      echo "bun install for classic failed (attempt ${attempt}/2); clearing download cache" >&2; \
      bun pm cache rm || rm -rf /root/.bun/install/cache; \
      sleep $((attempt * 5)); \
    done; \
    exit 1
COPY ./web/classic ./classic
COPY ./VERSION /build/VERSION
RUN if [ -n "$APP_VERSION" ]; then printf '%s\n' "$APP_VERSION" > /build/VERSION; fi
RUN cd classic && VITE_REACT_APP_VERSION=$(cat /build/VERSION) bun run build

FROM golang:1.26.1-alpine@sha256:2389ebfa5b7f43eeafbd6be0c3700cc46690ef842ad962f6c5bd6be49ed82039 AS builder2
ARG APP_VERSION
ARG GOPROXY=https://goproxy.cn,direct
ARG GOPROXY_FALLBACK=https://proxy.golang.org,direct
ENV GO111MODULE=on CGO_ENABLED=0
ENV GOPROXY=${GOPROXY}

ARG TARGETOS
ARG TARGETARCH
ENV GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64}
ENV GOEXPERIMENT=greenteagc

WORKDIR /build

ADD go.mod go.sum ./
RUN set -eux; \
    if ! timeout 180 env GOPROXY="$GOPROXY" go mod download; then \
      echo "Primary Go module proxy failed; retrying with $GOPROXY_FALLBACK" >&2; \
      timeout 180 env GOPROXY="$GOPROXY_FALLBACK" go mod download; \
    fi

COPY . .
RUN if [ -n "$APP_VERSION" ]; then printf '%s\n' "$APP_VERSION" > VERSION; fi
COPY --from=builder /build/web/default/dist ./web/default/dist
COPY --from=builder-classic /build/web/classic/dist ./web/classic/dist
RUN go build -ldflags "-s -w -X 'github.com/QuantumNous/new-api/common.Version=$(cat VERSION)'" -o new-api

FROM builder2 AS runtime-files

RUN set -eux; \
    mkdir -p \
      /runtime/bin \
      /runtime/usr/bin \
      /runtime/usr/lib \
      /runtime/lib \
      /runtime/etc/ssl/certs \
      /runtime/usr/local/go/lib/time \
      /runtime/tmp; \
    cp /bin/busybox /runtime/bin/busybox; \
    cp /usr/bin/ssl_client /runtime/usr/bin/ssl_client; \
    cp /usr/lib/libssl.so.3 /usr/lib/libcrypto.so.3 /runtime/usr/lib/; \
    cp /lib/ld-musl-*.so.1 /runtime/lib/; \
    cp /etc/ssl/certs/ca-certificates.crt /runtime/etc/ssl/certs/ca-certificates.crt; \
    cp /usr/local/go/lib/time/zoneinfo.zip /runtime/usr/local/go/lib/time/zoneinfo.zip; \
    ln -s /bin/busybox /runtime/bin/sh; \
    ln -s /bin/busybox /runtime/usr/bin/wget; \
    ln -s /bin/busybox /runtime/usr/bin/grep; \
    chmod 1777 /runtime/tmp

FROM scratch

ARG APP_VERSION
LABEL org.opencontainers.image.title="new-api" \
      org.opencontainers.image.version="${APP_VERSION}"

ENV PATH=/usr/bin:/bin \
    GOROOT=/usr/local/go \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

COPY --from=runtime-files /runtime/ /
COPY --from=builder2 /build/new-api /
COPY LICENSE NOTICE THIRD-PARTY-LICENSES.md /licenses/
EXPOSE 3000
WORKDIR /data
ENTRYPOINT ["/new-api"]
