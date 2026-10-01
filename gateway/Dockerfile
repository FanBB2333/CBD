ARG GO_IMAGE=golang:1.25-bookworm
ARG RUNTIME_IMAGE=debian:bookworm-slim
FROM ${GO_IMAGE} AS build

WORKDIR /src
COPY gateway/go.mod gateway/go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
RUN GOPROXY=${GOPROXY} go mod download
COPY gateway/ ./
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/cellbridge-gateway ./cmd/cellbridge-gateway
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/cellbridge-admin ./cmd/cellbridge-admin
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/cellbridge-probe ./cmd/cellbridge-probe

FROM ${RUNTIME_IMAGE}

ARG DEBIAN_MIRROR=http://deb.debian.org
RUN sed -i "s|http://deb.debian.org|${DEBIAN_MIRROR}|g" /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::http::Pipeline-Depth=0 update \
    && apt-get -o Acquire::http::Pipeline-Depth=0 install -y --no-install-recommends adb alsa-utils ffmpeg ca-certificates wget usbutils python3 python3-usb libusb-1.0-0 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system cellbridge && useradd --system --gid cellbridge --home-dir /var/lib/cellbridge cellbridge
COPY --from=build /out/cellbridge-gateway /usr/local/bin/cellbridge-gateway
COPY --from=build /out/cellbridge-admin /usr/local/bin/cellbridge-admin
COPY --from=build /out/cellbridge-probe /usr/local/bin/cellbridge-probe
COPY gateway/tools/usb_at.py /usr/local/lib/cellbridge/usb_at.py
COPY infra/config.example.yaml /etc/cellbridge/config.example.yaml
RUN mkdir -p /var/lib/cellbridge && chown -R cellbridge:cellbridge /var/lib/cellbridge

USER cellbridge
VOLUME ["/var/lib/cellbridge"]
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["wget", "-qO-", "http://127.0.0.1:8787/api/v1/health"]
ENTRYPOINT ["/usr/local/bin/cellbridge-gateway"]
