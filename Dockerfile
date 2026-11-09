FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/localdns ./cmd/localdns

FROM alpine:3.20
RUN apk add --no-cache dnsmasq
COPY --from=build /out/localdns /usr/local/bin/localdns
ENV LOCALDNS_DATA_DIR=/data
VOLUME /data
EXPOSE 53/udp 53/tcp 5380/tcp
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:5380/healthz >/dev/null || exit 1
ENTRYPOINT ["localdns"]
CMD ["serve"]
