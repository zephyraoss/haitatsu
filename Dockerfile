FROM golang:1.26-bookworm AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags "-s -w -X github.com/zephyraoss/haitatsu/internal/version.Version=${VERSION}" -o /out/haitatsu ./cmd/haitatsu

FROM debian:bookworm-slim

ARG TARGETARCH
ARG PKL_VERSION=0.31.1

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && case "${TARGETARCH}" in amd64) PKL_ARCH=amd64 PKL_SHA=618f13955d755cafbfe8c9cba1d27635848cd49dbc6abffd398d2751db1231bf ;; arm64) PKL_ARCH=aarch64 PKL_SHA=7ef10e743daa921fb94ae7bdb9ec6986f362bf250c55814b9ea2aeb13f2d083e ;; *) echo "unsupported arch ${TARGETARCH}" >&2; exit 1 ;; esac \
    && curl -fsSL -o /usr/local/bin/pkl "https://github.com/apple/pkl/releases/download/${PKL_VERSION}/pkl-linux-${PKL_ARCH}" \
    && echo "${PKL_SHA}  /usr/local/bin/pkl" | sha256sum -c - \
    && chmod +x /usr/local/bin/pkl \
    && useradd --system --uid 10001 --home-dir /var/lib/haitatsu --shell /usr/sbin/nologin haitatsu \
    && mkdir -p /var/lib/haitatsu/certmagic /var/lib/haitatsu/tmp /etc/haitatsu \
    && chown -R 10001:10001 /var/lib/haitatsu

COPY --from=build /out/haitatsu /usr/local/bin/haitatsu

USER haitatsu
WORKDIR /var/lib/haitatsu
ENV TMPDIR=/var/lib/haitatsu/tmp
VOLUME /var/lib/haitatsu
EXPOSE 8080 25 143 465 587

HEALTHCHECK --interval=15s --timeout=3s --start-period=30s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/ready >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/haitatsu"]
CMD ["-config", "/etc/haitatsu/haitatsu.pkl"]
