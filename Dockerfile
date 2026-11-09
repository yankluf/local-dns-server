# Base images are pinned by digest so a build can't silently pick up a
# different (or tampered) image. Dependabot proposes updates (tag and digest
# together) and CI builds and tests them before they are merged.
FROM golang:1.27.3-alpine3.24@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/lds ./cmd/lds

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
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
# its own unprivileged user. docker-compose.yml removes every capability it
# doesn't need.
ENTRYPOINT ["lds"]
CMD ["serve"]
