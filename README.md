# local-dns-server

`localdns` replaces Pi-hole's "Local DNS" feature with something built for a
Traefik + Docker home lab:

- **dnsmasq** answers DNS for your local names and forwards everything else
  to upstream resolvers.
- **One CLI command** adds a record: `localdns add nas.home.lan 192.168.1.10`.
- **A web UI** on port 5380 lists every record, grouped by domain.
- **Docker discovery** reads the `Host(...)` rules in your containers' Traefik
  labels and creates the matching records automatically as containers start
  and stop.

It's a single Go binary (no dependencies beyond the standard library) running
next to dnsmasq in a small Alpine image.

## Quick start

1. Free up port 53. Stop Pi-hole, and on Ubuntu/Debian hosts that use it,
   disable systemd-resolved's stub listener:

   ```sh
   sudo sed -i 's/^#\?DNSStubListener=.*/DNSStubListener=no/' /etc/systemd/resolved.conf
   sudo systemctl restart systemd-resolved
   ```

2. Edit `docker-compose.yml`: set `LOCALDNS_DOCKER_TARGET` to the IP of the
   machine running Traefik, and adjust `LOCALDNS_UPSTREAMS` if you like.

3. Start it:

   ```sh
   docker compose up -d --build
   ```

4. Add a wildcard so every name under your domain reaches Traefik:

   ```sh
   docker exec localdns localdns add '*.home.lan' 192.168.1.5 -c Traefik
   ```

5. Open `http://<host>:5380`, then point your router's DHCP DNS setting (or
   individual devices) at the host's IP.

For a shorter command on the Docker host:

```sh
alias localdns='docker exec localdns localdns'
```

## Records

The record type comes from what you type, so there's just one `add` command:

| Command                                          | Type     | dnsmasq line                              |
| ------------------------------------------------ | -------- | ----------------------------------------- |
| `localdns add '*.home.lan' 192.168.1.5`          | wildcard | `address=/home.lan/192.168.1.5`           |
| `localdns add nas.home.lan 192.168.1.10`         | A        | `host-record=nas.home.lan,192.168.1.10`   |
| `localdns add nas.home.lan fd00::10`             | AAAA     | `host-record=nas.home.lan,fd00::10`       |
| `localdns add files.home.lan nas.home.lan`       | CNAME    | `cname=files.home.lan,nas.home.lan`       |

How they interact:

- A wildcard covers the domain itself and every name under it.
- A specific A/AAAA record always wins over a wildcard, so
  `nas.home.lan` can point somewhere else than `*.home.lan`.
- **A CNAME only works when its target is another record defined here** (A,
  AAAA, CNAME, or a Docker record). That's a dnsmasq rule, and Pi-hole has the
  same limitation. Names only covered by a wildcard, and outside names like
  `example.com`, don't count. The CLI and web UI warn you when an alias won't
  resolve. For Traefik services you usually don't need CNAMEs at all: the
  wildcard or Docker discovery already sends them to Traefik.

## CLI

```
localdns add <name> <value> [-c comment]   add a record
localdns rm <name|id>...                   remove records
localdns list [--json] [--source manual|docker]
localdns import [--dry-run] <file>         import from Pi-hole or a hosts file
```

The client commands call the server's API at `http://127.0.0.1:5380` by
default. Use `--server http://dns-host:5380` or `LOCALDNS_URL` to manage it from
another machine, and `--token` or `LOCALDNS_TOKEN` if the server requires one.
The same binary runs on the host if you build it with `go build ./cmd/localdns`.

`list --json` makes the records easy to use in scripts:

```sh
localdns list --json | jq -r '.[] | select(.source=="docker") | .name'
```

## Docker discovery

With `LOCALDNS_DOCKER_TARGET` set and the Docker socket mounted, localdns
watches container start/stop events (and re-checks every minute). For every
running container it reads all `traefik.*.rule` labels and creates an A record
for each hostname in `Host(...)` or `HostSNI(...)` matchers:

```yaml
labels:
  - traefik.http.routers.grafana.rule=Host(`grafana.home.lan`)
  # -> grafana.home.lan -> LOCALDNS_DOCKER_TARGET
```

Per-container labels:

| Label             | Effect                                                                 |
| ----------------- | ---------------------------------------------------------------------- |
| `localdns.enable=false` | Skip this container (`traefik.enable=false` is skipped too).     |
| `localdns.target=IP`    | Use this IP instead of `LOCALDNS_DOCKER_TARGET`.                 |
| `localdns.hosts=a.lan,b.lan` | Extra hostnames, e.g. for containers Traefik doesn't route. |

Docker records show up in the CLI and web UI marked as Docker records. They
can't be removed by hand: stop or relabel the container instead. If you add a
manual record with the same name, the manual one wins and the Docker record is
shown as overridden. `HostRegexp` rules are ignored, since there's no single
name to create; cover those with a wildcard.

## Migrating from Pi-hole

Copy Pi-hole's local DNS files out of its container or volume, then import
them while localdns is running. Use `--dry-run` first to see what would be added.

```sh
# Pi-hole v6: records live in pihole.toml (dns.hosts and dns.cnameRecords)
docker cp pihole:/etc/pihole/pihole.toml .
localdns import pihole.toml

# Pi-hole v5: A records in custom.list, CNAMEs in a dnsmasq file
docker cp pihole:/etc/pihole/custom.list .
docker cp pihole:/etc/dnsmasq.d/05-pihole-custom-cname.conf .
localdns import custom.list
localdns import 05-pihole-custom-cname.conf
```

`import` also reads plain hosts files and dnsmasq `host-record=`, `cname=` and
`address=/domain/ip` lines. Records whose name already exists are skipped.
When you run `docker cp` and `localdns import` inside the container, copy the
files into the container's `/data` folder (the `./data` folder on the host).

## Configuration

All settings are environment variables on the server:

| Variable                     | Default                         | Meaning                                                     |
| ---------------------------- | ------------------------------- | ----------------------------------------------------------- |
| `LOCALDNS_DOCKER_TARGET`     | (unset: discovery off)          | IP that Docker/Traefik hostnames resolve to                 |
| `DOCKER_HOST`                | `unix:///var/run/docker.sock`   | Docker API (`unix://` or `tcp://`)                          |
| `LOCALDNS_UPSTREAMS`         | `1.1.1.1,1.0.0.1,9.9.9.9`       | Upstream DNS servers                                        |
| `LOCALDNS_HTTP_LISTEN`       | `:5380`                         | Web UI and API address                                      |
| `LOCALDNS_TOKEN`             | (unset)                         | If set, adding/removing records requires this bearer token  |
| `LOCALDNS_DATA_DIR`          | `/data`                         | Where `records.json` and the generated `dnsmasq.conf` live  |
| `LOCALDNS_DNS_PORT`          | `53`                            | DNS port                                                    |
| `LOCALDNS_DNS_LISTEN`        | (all interfaces)                | Comma-separated listen addresses                            |
| `LOCALDNS_CACHE_SIZE`        | `10000`                         | dnsmasq cache entries                                       |
| `LOCALDNS_LOG_QUERIES`       | `false`                         | Log every query to the container log                        |
| `LOCALDNS_DNSMASQ_EXTRA_DIR` | `/etc/dnsmasq.d`                | Extra `*.conf` files included in dnsmasq's config           |
| `LOCALDNS_DNSMASQ_ENABLED`   | `true`                          | `false` only writes the config (useful for development)     |

Manual records are stored in `/data/records.json`, so keep that volume (and
back it up). Docker records aren't stored; they're rebuilt from your containers.

## How it works

- Each change regenerates `dnsmasq.conf`, checks it with `dnsmasq --test`, and
  restarts dnsmasq. A config that fails the check is never applied. The
  restart takes a few milliseconds and empties dnsmasq's cache. dnsmasq has to
  restart because it doesn't reload `address=`, `host-record=` or `cname=` on
  SIGHUP.
- If dnsmasq dies, it's restarted within a couple of seconds. `/healthz`
  returns 503 while it's down, which the image's health check uses.
- The web UI refreshes every 10 seconds.

### API

| Method | Path                        | Body / result                                            |
| ------ | --------------------------- | -------------------------------------------------------- |
| GET    | `/api/records`              | `{"records": [...]}`                                     |
| POST   | `/api/records`              | `{"name", "value", "comment"}` → the new record          |
| DELETE | `/api/records/{name or id}` | the removed record                                       |
| GET    | `/api/status`               | dnsmasq and Docker state, upstreams                      |
| GET    | `/healthz`                  | `ok`, or 503 if dnsmasq isn't running                    |

## Development

```sh
go test ./...
LOCALDNS_DATA_DIR=./data LOCALDNS_DNS_PORT=5354 LOCALDNS_DNS_LISTEN=127.0.0.1 \
  go run ./cmd/localdns serve
dig @127.0.0.1 -p 5354 nas.home.lan
```

The tests check generated configs with `dnsmasq --test` when dnsmasq is
installed.
