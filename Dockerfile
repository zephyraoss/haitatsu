FROM golang:1.26-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/haitatsu ./cmd/haitatsu

FROM debian:bookworm-slim

ARG TARGETARCH
ARG PKL_VERSION=0.31.1

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates wget \
    && rm -rf /var/lib/apt/lists/* \
    && case "${TARGETARCH}" in amd64) PKL_ARCH=amd64 ;; arm64) PKL_ARCH=aarch64 ;; *) echo "unsupported arch ${TARGETARCH}" >&2; exit 1 ;; esac \
    && wget -q -O /usr/local/bin/pkl "https://github.com/apple/pkl/releases/download/${PKL_VERSION}/pkl-linux-${PKL_ARCH}" \
    && chmod +x /usr/local/bin/pkl \
    && useradd --system --uid 10001 --home-dir /app --shell /usr/sbin/nologin haitatsu
WORKDIR /app

COPY --from=build /out/haitatsu /usr/local/bin/haitatsu
COPY haitatsu.compose.pkl /app/haitatsu.pkl

USER haitatsu
EXPOSE 8080 25 143 465 587

ENTRYPOINT ["/usr/local/bin/haitatsu"]
CMD ["-config", "/app/haitatsu.pkl"]
