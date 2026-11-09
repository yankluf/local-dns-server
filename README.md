# LDS: Local DNS Server

Give the apps in your home lab friendly names like `grafana.home.lan`,
without fiddling with config files.

LDS is a small DNS server for your home network. It does the same job as
Pi-hole's "Local DNS" page, but it's built for a setup where Traefik sends
traffic to your Docker containers.

## What it does

- **Answers DNS for your own names**, like `nas.home.lan` → `192.168.1.10`.
- **Passes every other lookup to the internet** (Cloudflare and Quad9 by
  default), so it can be the only DNS server on your network.
- **Adds records with one short command**: `lds add nas.home.lan 192.168.1.10`.
- **Shows all your records on a web page**, where you can also add and remove
  them.
- **Picks up your Docker apps automatically.** If a container has a Traefik
  rule like ``Host(`grafana.home.lan`)``, that name starts working as soon as
  the container starts, with nothing to type.
- **Imports your old Pi-hole records**, so switching over takes a minute.

Under the hood it uses [dnsmasq](https://thekelleys.org.uk/dnsmasq/doc.html),
the same DNS engine Pi-hole uses.

---

## Contents

- [Getting started](#getting-started)
- [Adding records](#adding-records)
- [The web page](#the-web-page)
- [Automatic records from Docker](#automatic-records-from-docker)
- [Moving from Pi-hole](#moving-from-pi-hole)
- [Command reference](#command-reference)
- [Settings](#settings)
- [Good to know](#good-to-know)
- [Troubleshooting](#troubleshooting)
- [API](#api)
- [For developers](#for-developers)

---

## Getting started

You need a Linux machine with Docker and Docker Compose. Usually that's the
same machine that runs Traefik.

### 1. Free up port 53

DNS uses port 53, and only one program can use it at a time.

- **If Pi-hole is running on this machine, stop it**:
  `docker stop pihole`. Copy its records first if you want to keep them (see
  [Moving from Pi-hole](#moving-from-pi-hole)).
- **On Ubuntu and Debian**, a built-in service called systemd-resolved also
  sits on port 53. Turn that part of it off:

  ```sh
  sudo sed -i 's/^#\?DNSStubListener=.*/DNSStubListener=no/' /etc/systemd/resolved.conf
  sudo systemctl restart systemd-resolved
  ```

Not sure whether something is using the port? This command shows it:

```sh
sudo ss -lunp 'sport = :53'
```

No output means the port is free.

### 2. Set your Traefik IP

Open `docker-compose.yml` and change this line to the IP address of the
machine running Traefik:

```yaml
LDS_DOCKER_TARGET: 192.168.1.5
```

### 3. Start it

```sh
docker compose up -d --build
```

Then open `http://<your-server-ip>:5380` in a browser. You should see an empty
list of records.

### 4. Add your first record

This one line sends every name under `home.lan` to Traefik:

```sh
docker exec lds lds add '*.home.lan' 192.168.1.5
```

Typing `docker exec lds` every time gets old fast, so add this shortcut
to your `~/.bashrc` or `~/.zshrc`:

```sh
alias lds='docker exec lds lds'
```

From now on, `lds add ...` is all you need.

### 5. Point your network at it

Make your devices use LDS as their DNS server. The easiest way is your
router's settings: look for **DHCP** → **DNS server**, and enter your server's
IP address. Devices pick up the change the next time they reconnect.

Test it from any computer:

```sh
nslookup grafana.home.lan <your-server-ip>
```

---

## Adding records

There's only one command, `add`. You give it a name and what the name should
point to, and LDS works out the kind of record for you:

| You type                                      | You get    | What it means                                   |
| --------------------------------------------- | ---------- | ----------------------------------------------- |
| `lds add '*.home.lan' 192.168.1.5`       | Wildcard   | `home.lan` and **every** name under it → that IP |
| `lds add nas.home.lan 192.168.1.10`      | A record   | This one name → this IP                          |
| `lds add nas.home.lan fd00::10`          | AAAA record | Same, for an IPv6 address                       |
| `lds add files.home.lan nas.home.lan`    | CNAME      | This name is a nickname for another name        |

In short: an IP gives you a normal record, another name gives you a CNAME,
and a name starting with `*.` gives you a wildcard.

Put the wildcard in quotes (`'*.home.lan'`). Otherwise your shell may try to
treat the `*` as a file pattern.

### Add a note

Use `-c` to remember what a record is for. The note shows up in the list and
on the web page.

```sh
lds add nas.home.lan 192.168.1.10 -c "Synology in the closet"
```

### Remove a record

Use its name or its ID (the ID is shown by `lds list`):

```sh
lds rm nas.home.lan
```

### See your records

```sh
lds list
```

```
NAME              TYPE      VALUE         SOURCE          ID                       NOTE
*.home.lan        WILDCARD  192.168.1.5   manual          3c6633be                 Traefik
grafana.home.lan  A         192.168.1.5   docker:grafana  docker:grafana.home.lan
nas.home.lan      A         192.168.1.10  manual          8ed5116e                 Synology in the closet
```

Add `--source docker` or `--source manual` to see only one kind, or `--json`
to use the list in a script.

### Which record wins?

- **A specific name beats a wildcard.** With `*.home.lan → 192.168.1.5` and
  `nas.home.lan → 192.168.1.10`, the NAS gets its own IP and everything else
  goes to Traefik.
- **A record you add yourself beats one from Docker** with the same name.
- **Names must be unique.** To change a record, remove it and add it again.

### A note about CNAMEs

A CNAME only works when the name it points to **is also a record in
LDS**. That's a rule in dnsmasq, and Pi-hole has the same limit.

- ✅ `files.home.lan` → `nas.home.lan` works, because `nas.home.lan` is a record here.
- ❌ `files.home.lan` → `example.com` won't resolve.
- ❌ A CNAME pointing at a name that's only covered by a wildcard won't
  resolve either.

You don't have to remember this: the CLI and the web page warn you when a
CNAME won't work. For apps behind Traefik you rarely need CNAMEs anyway,
because the wildcard or Docker already handles them.

---

## The web page

Open `http://<your-server-ip>:5380`.

- **Records are grouped by domain**, with the start of each name in bold, so
  long lists stay easy to scan.
- **Colors show the kind of record:** amber for wildcards, green for A/AAAA,
  purple for CNAMEs.
- **Click a name** to open that site in a new tab.
- **Add a record** with the form at the top. It works the same way as the
  `add` command.
- **Remove a record** with the button on its row. Records from Docker show
  "auto" instead, because they come from your containers.
- **Filter** by name, IP or container, or show only manual or Docker records.
- **The top line shows whether things are healthy**: whether DNS is running,
  when Docker was last checked, and where other lookups are sent.

The page refreshes itself every 10 seconds and follows your system's light or
dark mode.

---

## Automatic records from Docker

LDS watches your containers. When a container with a Traefik rule
starts, its names start working. When it stops, they go away.

For example, this container:

```yaml
services:
  grafana:
    image: grafana/grafana
    labels:
      - traefik.http.routers.grafana.rule=Host(`grafana.home.lan`)
```

…gets `grafana.home.lan` → your Traefik IP, with no command needed.

It understands the usual Traefik rules:

- `` Host(`a.home.lan`) ``
- `` Host(`a.home.lan`) || Host(`b.home.lan`) ``
- `` Host(`a.home.lan`, `b.home.lan`) `` (Traefik v2 style)
- `` Host(`a.home.lan`) && PathPrefix(`/api`) `` (the path part is ignored)
- `` HostSNI(`db.home.lan`) `` for TCP routers

`HostRegexp` rules are skipped, because there's no single name to create.
Cover those with a wildcard.

### Turning it on

Discovery is on when both of these are in `docker-compose.yml` (they are by
default):

- `LDS_DOCKER_TARGET` is set to your Traefik IP.
- The Docker socket is mounted: `/var/run/docker.sock:/var/run/docker.sock:ro`.
  The `:ro` means read-only: LDS looks at your containers and never
  changes them.

To turn it off, remove `LDS_DOCKER_TARGET`.

### Labels you can add

Put these on a container to change how it's handled:

| Label                         | What it does                                                        |
| ----------------------------- | ------------------------------------------------------------------- |
| `lds.enable=false`       | Ignore this container.                                              |
| `lds.target=192.168.1.7` | Point this container's names at a different IP.                      |
| `lds.hosts=a.lan,b.lan`  | Add extra names, even for containers that don't use Traefik.         |

Containers with `traefik.enable=false` are ignored too.

### Things to know

- Docker records can't be removed with `rm` or from the web page. Stop the
  container or change its labels instead.
- If you add a record by hand with the same name, yours wins. The Docker one
  stays in the list, crossed out.
- If two containers use the same name, the first one alphabetically wins.
- Changes show up about a second after a container starts or stops.
  LDS also double-checks every minute.

---

## Moving from Pi-hole

Copy your records out of Pi-hole, then import them into lds.

**Pi-hole v6** keeps everything in one file:

```sh
docker cp pihole:/etc/pihole/pihole.toml ./data/
lds import /data/pihole.toml
```

**Pi-hole v5** uses two files, one for normal records and one for CNAMEs:

```sh
docker cp pihole:/etc/pihole/custom.list ./data/
docker cp pihole:/etc/dnsmasq.d/05-pihole-custom-cname.conf ./data/
lds import /data/custom.list
lds import /data/05-pihole-custom-cname.conf
```

Why `./data/` and then `/data/`? The `import` command runs inside the
container, and the `./data` folder on your machine is `/data` inside it.

Tips:

- **Preview first** with `--dry-run`, which shows what would be added without
  changing anything.
- **Running it twice is safe.** Names that already exist are skipped.
- **Imported records get the note "imported"**, so you can spot them later.
- `import` also reads plain hosts files (`192.168.1.10 nas.lan`) and dnsmasq
  files with `address=`, `host-record=` and `cname=` lines.

---

## Command reference

```
lds add <name> <value> [-c note]       Add a record
lds rm <name or id> [...]              Remove one or more records
lds list [--json] [--source manual|docker]
lds import [--dry-run] <file>          Import from Pi-hole or a hosts file
lds serve                              Run the server (the container does this)
lds version
lds help
```

### Using the CLI from another computer

The CLI talks to the server over the network, so you can use it from your
laptop too. Build it once (you need [Go](https://go.dev/dl/)):

```sh
go build -o lds ./cmd/lds
```

Then tell it where the server is:

```sh
export LDS_URL=http://192.168.1.5:5380
lds list
```

You can also pass `--server http://192.168.1.5:5380` with each command.

---

## Settings

Set these in the `environment:` section of `docker-compose.yml`, then run
`docker compose up -d` to apply them.

| Setting                      | Default                       | What it does                                                   |
| ---------------------------- | ----------------------------- | -------------------------------------------------------------- |
| `LDS_DOCKER_TARGET`     | *(empty: Docker discovery off)* | Traefik's IP. Docker names point here.                       |
| `LDS_UPSTREAMS`         | `1.1.1.1,1.0.0.1,9.9.9.9`     | Where all other lookups go.                                    |
| `LDS_HTTP_LISTEN`       | `:5380`                       | Address and port for the web page and API.                     |
| `LDS_TOKEN`             | *(empty)*                     | A password needed to add or remove records. See below.         |
| `LDS_DNS_PORT`          | `53`                          | Port for DNS.                                                  |
| `LDS_DNS_LISTEN`        | *(all addresses)*             | Only answer on these IPs, comma-separated.                     |
| `LDS_CACHE_SIZE`        | `10000`                       | How many answers to remember.                                  |
| `LDS_LOG_QUERIES`       | `false`                       | Log every lookup. Handy for debugging, noisy otherwise.        |
| `LDS_DATA_DIR`          | `/data`                       | Where records are saved.                                       |
| `LDS_DNSMASQ_EXTRA_DIR` | `/etc/dnsmasq.d`              | Folder for your own dnsmasq settings. See below.               |
| `LDS_DNSMASQ_ENABLED`   | `true`                        | Set to `false` to only write the config without running dnsmasq. |
| `DOCKER_HOST`                | `unix:///var/run/docker.sock` | How to reach Docker (`unix://` or `tcp://`).                   |

### Protecting changes with a token

Anyone on your network can open the web page. To stop others from changing
records, set a token:

```yaml
LDS_TOKEN: pick-something-long
```

- Viewing records still works for everyone.
- The web page asks for the token the first time you add or remove something,
  then remembers it in that browser.
- The CLI reads the token from `LDS_TOKEN` or `--token`.
  `docker exec` runs inside the container, which already has the setting, so
  it works without extra steps.

### Your own dnsmasq settings

Need something extra from dnsmasq, like DHCP or a special forwarding rule? Put
`.conf` files in the `./dnsmasq.d` folder next to `docker-compose.yml`, then
restart: `docker compose restart`.

### Your data

Records you add are saved in `./data/records.json`. Back up that file and
you've backed up everything. Docker records aren't saved, because they're
rebuilt from your containers each time.

---

## Good to know

- **Changes are instant.** Each change rewrites dnsmasq's settings and
  restarts it, which takes a few milliseconds.
- **Mistakes can't break DNS.** Every change is tested before it's used, and a
  bad one is rejected while the old settings keep working.
- **It looks after itself.** If dnsmasq crashes, it's restarted within a
  couple of seconds. The container also has a health check that you can see
  in `docker ps`.
- **No ad-blocking.** LDS only does local names and forwarding. If you
  miss ad-blocking, set `LDS_UPSTREAMS` to a filtering DNS service such
  as AdGuard DNS (`94.140.14.14,94.140.15.15`).
- **Each change clears the cache.** Restarting dnsmasq empties its memory of
  past answers, so the next few lookups are a little slower. You won't notice
  it at home-lab sizes.

---

## Troubleshooting

**The web page says dnsmasq stopped, or the logs say "Address in use".**
Something else is using port 53. Run `sudo ss -lunp 'sport = :53'` to see
what it is, then go back to [step 1](#1-free-up-port-53).

**A name doesn't work.**

1. Ask LDS directly, so other DNS servers aren't involved:
   `nslookup nas.home.lan <your-server-ip>`.
2. If that works, your device isn't using LDS yet. Check your router's
   DHCP settings, then reconnect the device.
3. If it doesn't work, check `lds list`. Is the record there? Does it
   have a warning?

**A Docker app doesn't get a name.**

- Is `LDS_DOCKER_TARGET` set? The top of the web page should say "Docker
  synced".
- Is the Docker socket mounted in `docker-compose.yml`?
- Does the container have a `traefik.…rule` label with `Host(...)` in it?
- Look at the logs: `docker logs lds`.

**I changed a record, but my computer still gets the old answer.**
Your computer has its own DNS memory. Wait a minute, or clear it:

- Windows: `ipconfig /flushdns`
- macOS: `sudo dscacheutil -flushcache; sudo killall -HUP mDNSResponder`
- Linux: `resolvectl flush-caches`

**I want to see every lookup.**
Set `LDS_LOG_QUERIES: "true"`, run `docker compose up -d`, then watch
`docker logs -f lds`.

---

## API

The web page and the CLI both use this small JSON API, so you can use it from
scripts as well.

| Method   | Path                          | What it does                                    |
| -------- | ----------------------------- | ----------------------------------------------- |
| `GET`    | `/api/records`                | List all records                                |
| `POST`   | `/api/records`                | Add a record: `{"name": "...", "value": "...", "comment": "..."}` |
| `DELETE` | `/api/records/{name or id}`   | Remove a record                                 |
| `GET`    | `/api/status`                 | Health of dnsmasq and Docker discovery          |
| `GET`    | `/healthz`                    | `ok`, or an error if DNS is down                |

If you set a token, send it with changes as `Authorization: Bearer <token>`.

```sh
curl -X POST http://192.168.1.5:5380/api/records \
  -H 'Content-Type: application/json' \
  -d '{"name": "nas.home.lan", "value": "192.168.1.10"}'
```

---

## For developers

LDS is written in Go and uses only the standard library.

```
cmd/lds/        the CLI and the serve command, plus the Pi-hole importer
internal/store/      records: checking, saving, working out types
internal/dnsmasq/    writes dnsmasq's config and runs dnsmasq
internal/docker/     reads Traefik labels and follows container events
internal/server/     the API and the web page (ui/index.html)
```

Run the tests:

```sh
go test ./...
```

When dnsmasq is installed, the tests also check that the generated config is
valid.

Run it locally on a spare port, without needing root:

```sh
LDS_DATA_DIR=./data LDS_DNS_PORT=5354 LDS_DNS_LISTEN=127.0.0.1 \
  go run ./cmd/lds serve

dig @127.0.0.1 -p 5354 nas.home.lan
```
