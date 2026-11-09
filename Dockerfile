# Base images are pinned by digest so a build can't silently pick up a
# different (or tampered) image. Update the tag and digest together.
FROM golang:1.24.7-alpine3.22@sha256:fc2cff6625f3c1c92e6c85938ac5bd09034ad0d4bc2dfb08278020b68540dbb5 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/lds ./cmd/lds

FROM alpine:3.22.2@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412
# dnsmasq-dnssec is dnsmasq built with DNSSEC validation; fail the build if not.
RUN apk add --no-cache dnsmasq-dnssec \
 && dnsmasq --version | grep -Eq '(^| )DNSSEC( |$)' \
 && mkdir -p /data /etc/dnsmasq.d \
 && chmod 0755 /data
COPY --from=build /out/lds /usr/local/bin/lds
ENV LDS_DATA_DIR=/data
VOLUME /data
EXPOSE 53/udp 53/tcp 5380/tcp
# Sends a real DNS query to dnsmasq through /healthz.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["lds", "healthcheck"]
# lds starts as root only so dnsmasq can bind port 53; dnsmasq then drops to
# its own unprivileged user. docker-compose.yml removes every other capability.
ENTRYPOINT ["lds"]
CMD ["serve"]
