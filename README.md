# LDS: Local DNS Server

Give the apps on your network friendly names like `grafana.home.lan`,
without fiddling with config files.

LDS is a small DNS server for your network. It does the same job as
Pi-hole's "Local DNS" page, but it's built for a setup where Traefik sends
traffic to your Docker containers, and it's hardened so it can be trusted
with important traffic.

## What it does

- **Answers DNS for your own names**, like `nas.home.lan` → `192.168.1.10`.
- **Passes every other lookup to the internet** (Cloudflare and Quad9 by
  default) and checks DNSSEC signatures on the way, so it can be the only DNS
  server on your network.
- **Adds records with one short command**: `lds add nas.home.lan 192.168.1.10`.
- **Shows all your records on a web page**, where you can also add and remove
  them.
- **Picks up your Docker apps automatically.** If a container has a Traefik
  rule like ``Host(`grafana.home.lan`)``, that name starts working as soon as
  the container starts.
- **Imports your old Pi-hole records**, so switching over takes a minute.
- **Can run on two servers**, so DNS keeps working if one goes down.

Under the hood it uses [dnsmasq](https://thekelleys.org.uk/dnsmasq/doc.html),
the same DNS engine Pi-hole uses.

---

## Contents

- [Getting started](#getting-started)
- [Basic configuration](#basic-configuration)
- [Adding records](#adding-records)
- [The web page](#the-web-page)
- [Automatic records from Docker](#automatic-records-from-docker)
- [Moving from Pi-hole](#moving-from-pi-hole)
- [Backing up and moving records](#backing-up-and-moving-records)
- [A second server](#a-second-server)
- [Security](#security)
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

Not sure whether something is using the port? This shows it:

```sh
sudo ss -lunp 'sport = :53'
```

No output means the port is free.

### 2. Start it

```sh
docker compose up -d --build
```

That's it: no settings are needed to start. This runs two containers: `lds`
itself, and a small helper called `lds-docker-proxy` that lets LDS see your
containers without giving it control over Docker.

### 3. Sign in

LDS made an access token for you on first start. Show it with:

```sh
docker exec lds cat /data/admin-token
```

Keep it somewhere safe, like your password manager. Open
`http://<your-server-ip>:5380` and sign in with it.

### 4. Set your domain and Traefik IP

Open the **Settings** tab and fill in:

- **Domains**: the domain your names live under, like `home.lan`.
- **Traefik IP**: the IP of the machine running Traefik, like `192.168.1.5`.

Click **Save changes**. They work right away.

LDS only ever answers for names inside your domains. That's a safety rule:
nobody can use it to redirect `google.com` or your bank.

### 5. Add your first record

This one line sends every name under `home.lan` to Traefik:

```sh
docker exec lds lds add '*.home.lan' 192.168.1.5
```

(Or use the form on the **Records** tab.)

Typing `docker exec lds` every time gets old fast, so add this shortcut to
your `~/.bashrc` or `~/.zshrc`:

```sh
alias lds='docker exec lds lds'
```

From now on, `lds add ...` is all you need. (Inside the container, `lds`
already knows the token.)

### 6. Point your network at it

Make your devices use LDS as their DNS server. The easiest way is your
router's settings: look for **DHCP** → **DNS server**, and enter your
server's IP address. Devices pick up the change the next time they reconnect.

Test it from any computer:

```sh
nslookup grafana.home.lan <your-server-ip>
```

---

## Basic configuration

LDS starts with no configuration at all, and some things work straight away.
Others need a setting first. This section lists everything that **doesn't**
work out of the box, why, and how to turn it on.

### What works with zero configuration

- **DNS forwarding.** Every lookup is sent to Cloudflare and Quad9, with
  DNSSEC checks. Your devices can use LDS as a normal DNS server.
- **The web page**, on port 5380 of every address of the machine.
- **An access token**, generated on first start and saved in
  `./data/admin-token`.
- **The `lds` command** inside the container, which already knows the token.
- **Safety features**: all the protections in [Security](#security), the
  automatic fallback if something goes wrong, and the health check.

### What needs a setting

| Doesn't work yet | Why | How to turn it on |
| --- | --- | --- |
| **Adding records** | LDS only answers for domains you choose, so nobody can use it to redirect outside sites. With no domains set, every record is refused. | **Settings → Domains**, or `LDS_ZONES: home.lan` |
| **Automatic records from Docker** | LDS has to know which IP your Docker app names should point to. | **Settings → Traefik IP**, or `LDS_DOCKER_TARGET: 192.168.1.5` |
| **Your devices using LDS** | Devices keep using their old DNS server until told otherwise. | Your router's DHCP settings → DNS server = this machine's IP |
| **Port 53** | If Pi-hole or systemd-resolved already uses it, dnsmasq can't start. The web page then says "dnsmasq stopped". | Free it up, see [step 1](#1-free-up-port-53) |
| **HTTPS** | The page and API use plain HTTP, so the token crosses your network unencrypted. | `LDS_TLS_CERT` and `LDS_TLS_KEY`, or Traefik, see [Use HTTPS](#use-https-for-the-web-page) |
| **Opening the page under a name** like `dns.example.org` | To block DNS rebinding attacks, the page only opens under an IP, `localhost`, or a name inside your domains. Other names get "unknown Host". | `LDS_ALLOWED_HOSTS: dns.example.org` |
| **Devices on a VPN or another subnet** | With no listen address set, LDS only answers networks it's directly connected to, so it can't become an open DNS server on the internet. | `LDS_DNS_LISTEN: 192.168.1.5` (the IPs to answer on) |
| **Company sites that resolve to private IPs** (split DNS) | Rebinding protection drops outside answers that point at private addresses. | **Settings → Block rebinding** off, or a `rebind-domain-ok=/corp.example.com/` line in `./dnsmasq.d/` |
| **Containers choosing their own IP** with `lds.target` | Labels can come from any downloaded image, so other IPs must be approved. | **Settings → Other allowed IPs**, or `LDS_DOCKER_ALLOWED_TARGETS` |
| **A second server** | It has to know where the first one is, and both need the same token. A generated token isn't shared between machines. | `LDS_REPLICA_OF` plus `LDS_TOKEN_FILE` on both, see [A second server](#a-second-server) |
| **Your own token** (to share, rotate or keep in a password manager) | Without one, LDS uses the generated token. | `LDS_TOKEN_FILE` (see the comments in `docker-compose.yml`) |
| **Using the CLI from another computer** | The CLI needs to know where the server is and what the token is. | `LDS_URL` and `LDS_TOKEN_FILE`, see [the CLI](#using-the-cli-from-another-computer) |
| **More than 5000 records** | A limit stops anyone flooding LDS. | **Settings → Record limit**, or `LDS_MAX_RECORDS` |
| **Ad-blocking** | LDS doesn't block ads. | **Settings → Upstream DNS servers**: use a filtering service like AdGuard DNS (`94.140.14.14, 94.140.15.15`) |
| **Clients remembering answers** | Local answers have a lifetime of 0, so a changed record is seen right away. | **Settings → Answer lifetime**, or `LDS_LOCAL_TTL: 300` |
| **DHCP or other dnsmasq extras** | Not part of LDS. | Add `.conf` files to `./dnsmasq.d/`; for DHCP also add `NET_ADMIN` and `NET_RAW` to `cap_add` |

### Also worth knowing

- **DNSSEC needs the right time.** If the server's clock is far off, most
  lookups fail. Fix the clock, or turn off **Settings → Check DNSSEC
  signatures**.
- **Wildcards are never created automatically.** Add `*.home.lan` yourself,
  as in [step 5](#5-add-your-first-record).
- **Settings changed on the dashboard win** over `docker-compose.yml`. Each
  setting shows where its value comes from, and **Use docker-compose.yml
  value** undoes a dashboard change.

---

## Adding records

There's only one command, `add`. You give it a name and what the name should
point to, and LDS works out the kind of record for you:

| You type                                 | You get     | What it means                                    |
| ---------------------------------------- | ----------- | ------------------------------------------------ |
| `lds add '*.home.lan' 192.168.1.5`       | Wildcard    | `home.lan` and **every** name under it → that IP |
| `lds add nas.home.lan 192.168.1.10`      | A record    | This one name → this IP                          |
| `lds add nas.home.lan fd00::10`          | AAAA record | Same, for an IPv6 address                        |
| `lds add files.home.lan nas.home.lan`    | CNAME       | This name is a nickname for another name         |

In short: an IP gives you a normal record, another name gives you a CNAME,
and a name starting with `*.` gives you a wildcard.

Put the wildcard in quotes (`'*.home.lan'`). Otherwise your shell may try to
treat the `*` as a file pattern.

### Add a note

Use `-c` to remember what a record is for:

```sh
lds add nas.home.lan 192.168.1.10 -c "Synology in the closet"
```

### IPv4 and IPv6 for the same name

A name can have one IPv4 address and one IPv6 address. Just add both:

```sh
lds add nas.home.lan 192.168.1.10
lds add nas.home.lan fd00::10
```

### Remove a record

Use its name or its ID (shown by `lds list`):

```sh
lds rm nas.home.lan
```

Removing by name removes everything with that name (both addresses, if it
has two). Removing by ID removes just that one. LDS prints what it removed.

### Check a name

Not sure why a name goes where it goes? Ask:

```sh
lds check grafana.home.lan
```

It shows what LDS answers for that name and which record decides it (its own
record, a CNAME, or a wildcard). It only works for names inside your domains.

### See your records

```sh
lds list
```

```
NAME              TYPE      VALUE         SOURCE          ID                       NOTE
*.home.lan        WILDCARD  192.168.1.5   manual          3c6633be5a1f09d2         Traefik
grafana.home.lan  A         192.168.1.5   docker:grafana  docker:a:grafana.home.lan
nas.home.lan      A         192.168.1.10  manual          8ed5116e02b7c4a1         Synology in the closet
```

Add `--source docker` or `--source manual` to see only one kind, or `--json`
to use the list in a script.

### The rules

- **Names must be inside your zones.** With `LDS_ZONES=home.lan`, you can add
  `nas.home.lan` or `*.lab.home.lan`, but not `nas.lan` or `google.com`.
- **A specific name beats a wildcard.** With `*.home.lan → 192.168.1.5` and
  `nas.home.lan → 192.168.1.10`, the NAS gets its own IP and everything else
  goes to Traefik.
- **A record you add yourself beats one from Docker** with the same name.
- **Names must be unique**, except that a name can have one IPv4 and one IPv6
  address. To change a record, remove it and add it again.
- **Every change is tested before it's saved.** If dnsmasq wouldn't accept
  it, you get an error and nothing changes.

### A note about CNAMEs

A CNAME only works when the name it points to **is also a record in LDS**.
That's a rule in dnsmasq, and Pi-hole has the same limit.

- ✅ `files.home.lan` → `nas.home.lan` works, because `nas.home.lan` is a record here.
- ❌ `files.home.lan` → `example.com` is refused, because it's outside your zones.
- ❌ CNAMEs that point at each other in a circle (`a` → `b` → `a`) are refused.

The CLI and the web page warn you when a CNAME's target doesn't exist yet.
For apps behind Traefik you rarely need CNAMEs anyway, because the wildcard
or Docker already handles them.

---

## The web page

Open `http://<your-server-ip>:5380` and sign in with your token.

- **Your token stays in that browser tab** and is forgotten when you close it,
  unless you tick "Remember on this browser". Use **Sign out** on shared
  computers.
- **Records are grouped by domain**, with the start of each name in bold.
- **Colors show the kind of record:** amber for wildcards, green for A/AAAA,
  purple for CNAMEs.
- **Click a name** to open that site in a new tab.
- **Add a record** with the form at the top. It works the same way as the
  `add` command.
- **Remove a record** with the button on its row. Records from Docker show
  "auto" instead, because they come from your containers.
- **Filter** by name, IP or container, or show only manual or Docker records.
- **Check a name** with the box under the list. It shows what LDS answers
  for that name and which record decides it.
- **The top line shows whether things are healthy**: whether DNS is running,
  when Docker was last checked, where other lookups are sent, and on a second
  server, when it last copied the first one. Problems show up here too.
- **The Settings tab** shows every setting, its current value and where it
  comes from (default, `docker-compose.yml`, or changed here). You can change
  the day-to-day ones right there. Before saving, LDS tests the change and
  asks you to confirm anything with side effects, like records that would
  stop being served. See [Settings](#settings).

The page refreshes itself every 10 seconds and follows your system's light or
dark mode.

---

## Automatic records from Docker

LDS watches your containers. When a container with a Traefik rule starts,
its names start working. When it stops, they go away.

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

### Safety rules for Docker records

Container labels can come from any image you download, so LDS doesn't trust
them blindly:

- **Only names inside your zones** become records. A label claiming
  `github.com` is ignored.
- **Only plain names**, never wildcards. Add wildcards yourself.
- **They point at your Traefik IP.** A container can only pick a different IP
  with `lds.target` if that IP is listed in `LDS_DOCKER_ALLOWED_TARGETS`.
- **Your own records always win** over Docker ones with the same name.

Anything skipped is noted in the log: `docker logs lds`.

### Labels you can add

| Label                         | What it does                                                         |
| ----------------------------- | -------------------------------------------------------------------- |
| `lds.enable=false`            | Ignore this container.                                               |
| `lds.target=192.168.1.7`      | Point this container's names at another IP (must be allowed, see above). |
| `lds.hosts=a.home.lan,b.home.lan` | Add extra names, even for containers that don't use Traefik.     |

Containers with `traefik.enable=false` are ignored too.

### How LDS sees your containers

LDS doesn't get the Docker socket directly, because that would give it full
control over Docker, and so over the whole machine. (A `:ro` on a socket
mount doesn't change that.) Instead, `lds-docker-proxy` only lets it **read
the container list and events**. The proxy has no network at all: LDS talks
to it through a socket file that only the two of them share, so nothing else
on the machine or the network can reach it. It's all set up in
`docker-compose.yml`.

### Things to know

- Docker records can't be removed with `rm` or from the web page. Stop the
  container or change its labels instead.
- If two containers use the same name, the first one alphabetically wins.
- Docker records count toward the record limit. If they don't fit, none are
  changed and the top of the web page says so.
- Changes show up about a second after a container starts or stops, and LDS
  double-checks every minute.
- Docker changes never restart dnsmasq, so a container stuck in a restart
  loop can't disturb DNS.

---

## Moving from Pi-hole

Copy your records out of Pi-hole, then import them into LDS. Only names
inside your domains are imported, so add them under **Settings → Domains**
first (for example `home.lan, lan.example.com`).

**Pi-hole v6** keeps everything in one file:

```sh
docker cp pihole:/etc/pihole/pihole.toml ./data/
lds import /data/pihole.toml
```

**Pi-hole v5** uses two files:

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

## Backing up and moving records

`lds export` prints your records as a plain text file, notes included:

```sh
lds export > records-backup.txt
```

`lds import` reads that file back, on this server or another one:

```sh
cp records-backup.txt data/
lds import /data/records-backup.txt
```

Records from Docker are left out, because your containers recreate them. Add
`--all` to include them.

---

## A second server

One DNS server is a single point of failure: if that machine is down, nothing
on your network can look up names. You can run a second LDS that keeps a
copy of the first one's records.

Both servers need the same token, so use your own instead of the generated
one. On the first machine, either create `lds_token.txt` as described in
`docker-compose.yml`, or reuse the generated one:

```sh
sudo cp data/admin-token lds_token.txt && sudo chown $USER lds_token.txt && chmod 600 lds_token.txt
```

Turn on `LDS_TOKEN_FILE` and `secrets` in `docker-compose.yml` on **both**
machines, copy `lds_token.txt` to the second one, and add this on the second
machine:

```yaml
LDS_REPLICA_OF: http://192.168.1.5:5380   # the first server
```

The second server:

- copies the first server's domains and all its records every 30 seconds
  (`LDS_REPLICA_INTERVAL` changes that), so you don't set `LDS_ZONES` on it,
- tells you on its web page when it last copied, and if any record couldn't
  be copied,
- saves the copy, so it keeps answering even if the first server is down when
  it restarts,
- refuses changes. Make them on the first server.

Then give your devices **both** servers: in your router's DHCP settings, set
the first server as DNS 1 and the second as DNS 2.

---

## Security

LDS controls where every device on your network goes, so it's locked down by
default:

| Protection                     | What it stops                                                         |
| ------------------------------ | --------------------------------------------------------------------- |
| Token required                 | Anyone on the network changing or reading your records.               |
| Allowed domains (`LDS_ZONES`)  | Redirecting outside sites like `google.com`, even with the token.      |
| Strict checks on every record  | Sneaking extra settings into dnsmasq through a record.                |
| Changes need real JSON from the same site | Web pages making your browser change records (CSRF).        |
| Only answers to known addresses | Websites reaching LDS through a fake domain (DNS rebinding).         |
| Wrong-token slowdown           | Guessing the token: after 10 wrong tries, that address gets one try per second for 5 minutes. The right token still works, so nobody can lock you out. |
| Rate limits and a record limit | Flooding LDS with changes.                                            |
| Changes tested before saving   | A bad record breaking DNS.                                            |
| Last working config kept       | DNS going down after a restart, whatever happened to the records.     |
| Docker socket proxy            | A bug in LDS turning into control over Docker. The proxy has no network and allows two read-only requests. |
| Container hardening            | Read-only files, no extra privileges, only the rights dnsmasq needs.  |
| Change log                     | Not knowing who changed what: every change is written to `docker logs lds`. |
| Pinned images, automatic checks | Surprise updates. Images are pinned to exact versions, Dependabot proposes updates, and every change to LDS is tested on GitHub against a real Docker setup. |
| Local networks only            | Your server becoming an open DNS server on the internet.              |
| DNSSEC checks                  | Forged answers for signed domains.                                    |
| Rebinding protection           | Outside domains pointing your devices at internal addresses.          |
| Strict web page policy         | Scripts from anywhere else running in the LDS page.                   |

### Use HTTPS for the web page

The token is sent with every change. Over plain HTTP, someone listening on
the network could see it. Two easy fixes:

- **Built in:** set `LDS_TLS_CERT` and `LDS_TLS_KEY` to a certificate and key
  file (mount them into the container). The CLI trusts a private CA if you set
  `LDS_CA_FILE`.
- **Through Traefik** (when Traefik runs on the same machine): set
  `LDS_HTTP_LISTEN` to the machine's LAN IP, like `192.168.1.5:5380`, and add
  a Traefik route (with the file provider) from an HTTPS name like
  `dns.home.lan` to `http://192.168.1.5:5380`. The hop between Traefik and LDS
  then never leaves the machine. If the name is outside your zones, add it to
  `LDS_ALLOWED_HOSTS`.

### Good habits

- Use a long random token. The generated one is; for your own, use
  `openssl rand -hex 32`. The slowdown above only helps against guessing
  when the token can't be guessed in the first place.
- Keep the token private, and change it if it leaks. For the generated one,
  delete `./data/admin-token` and run `docker compose restart lds` to get a
  new one. For your own, write a new `lds_token.txt` and run
  `docker compose up -d`.
- Set `LDS_HTTP_LISTEN` to your LAN IP (like `192.168.1.5:5380`) so the page
  isn't reachable on other networks.
- Back up the `./data` folder, or run `lds export` now and then.
- To see who changed what, run `docker logs lds 2>&1 | grep audit:`. Each
  line has the address that made the change and what it added, removed or
  set.
- Keep the `cap_add` list in `docker-compose.yml` as it is. Without `KILL`,
  LDS can't restart dnsmasq, so wildcard, CNAME and settings changes wait
  until the container restarts. (The web page warns you if that happens.)

---

## Command reference

```
lds add <name> <value> [-c note]       Add a record
lds rm <name or id> [...]              Remove records (a name removes all with that name)
lds list [--json] [--source manual|docker|replica]
lds check <name>                       Show what a name resolves to, and why
lds import [--dry-run] <file>          Import from Pi-hole, a hosts file or an export
lds export [--all]                     Print your records as a file "lds import" can read
lds healthcheck                        Exit 0 if DNS is answering (used by Docker)
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

Then tell it where the server is and how to find the token:

```sh
export LDS_URL=http://192.168.1.5:5380
export LDS_TOKEN_FILE=~/.config/lds/token   # a file containing the token
lds list
```

You can also set `LDS_TOKEN` directly, or pass `--server` and `--token-file`
with each command. There's no `--token` option on purpose: commands are
visible to other users on the machine and saved in your shell history.

---

## Settings

There are two ways to change settings:

- **The dashboard's Settings tab** for the day-to-day ones: domains, upstream
  DNS servers, DNSSEC, rebinding protection, cache size, answer lifetime,
  query logging, the record limit, and the Docker IPs. Changes are checked and tested, then work
  right away. They're saved in `./data/settings.json` and win over
  `docker-compose.yml`. **Use docker-compose.yml value** undoes one.
- **`docker-compose.yml`** for everything, including the settings that
  protect the server itself (token, addresses, ports, HTTPS, data folder,
  Docker connection). The Settings tab shows these too, greyed out. After
  editing, run `docker compose up -d`.

LDS checks every setting when it starts and stops with a clear message if one
is wrong. None are required; see [Basic configuration](#basic-configuration)
for what each one unlocks.

**Common** (✏️ = can also be changed on the dashboard)

| Setting                      | Default                       | What it does                                                 |
| ---------------------------- | ----------------------------- | ------------------------------------------------------------ |
| `LDS_ZONES` ✏️               | *(empty: records refused)*    | Domains LDS answers for, comma-separated. Example: `home.lan`. |
| `LDS_TOKEN_FILE` or `LDS_TOKEN` | *(generated)*              | The access token (a file is safer). At least 16 characters.  |
| `LDS_DOCKER_TARGET` ✏️       | *(empty: Docker off)*         | Traefik's IP. Docker names point here.                       |
| `LDS_DOCKER_ALLOWED_TARGETS` ✏️ | *(empty)*                  | Other IPs containers may choose with `lds.target`.           |
| `DOCKER_HOST`                | `unix:///var/run/docker.sock` | How to reach Docker. The compose file uses the proxy.        |
| `LDS_UPSTREAMS` ✏️           | `1.1.1.1,1.0.0.1,9.9.9.9`     | Where other lookups go. `IP` or `IP#port`.                   |
| `LDS_HTTP_LISTEN`            | `:5380`                       | Address and port for the web page and API.                   |
| `LDS_DNS_LISTEN`             | *(all, local networks only)*  | Only answer on these IPs, comma-separated.                   |
| `LDS_TLS_CERT`, `LDS_TLS_KEY` | *(empty)*                    | Serve the web page and API over HTTPS.                       |
| `LDS_REPLICA_OF`             | *(empty)*                     | Make this a read-only copy of another LDS.                   |

**Fine-tuning**

| Setting                      | Default          | What it does                                                          |
| ---------------------------- | ---------------- | --------------------------------------------------------------------- |
| `LDS_ALLOWED_HOSTS`          | *(empty)*        | Extra names the web page may be opened under (besides IPs and your zones). |
| `LDS_PUBLIC_READ`            | `false`          | Let anyone read records without the token.                            |
| `LDS_MAX_RECORDS` ✏️          | `5000`           | Most records you can add.                                             |
| `LDS_DNSSEC` ✏️               | `true`           | Check DNSSEC signatures on upstream answers.                          |
| `LDS_STOP_DNS_REBIND` ✏️      | `true`           | Block upstream answers that point at private IPs (your zones are exempt). |
| `LDS_CACHE_SIZE` ✏️           | `10000`          | How many answers to remember.                                         |
| `LDS_LOCAL_TTL` ✏️            | `0`              | Seconds devices may remember your records (up to 86400). `0` means changes are seen right away. |
| `LDS_LOG_QUERIES` ✏️          | `false`          | Log every lookup. Handy for debugging, noisy otherwise.               |
| `LDS_RESTART_SPACING_MS`     | `2000`           | Least time between dnsmasq restarts (wildcard and CNAME changes).     |
| `LDS_REPLICA_INTERVAL`       | `30`             | Seconds between copies on a replica.                                  |
| `LDS_DNS_PORT`               | `53`             | Port for DNS.                                                         |
| `LDS_DATA_DIR`               | `/data`          | Where records are saved.                                              |
| `LDS_DNSMASQ_EXTRA_DIR`      | `/etc/dnsmasq.d` | Folder for your own dnsmasq settings (see below).                     |
| `LDS_DOCKER_ALLOW_INSECURE`  | `false`          | Allow unencrypted `tcp://` to Docker on another machine.              |
| `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH` | *(empty)* | Use TLS for a remote Docker, like the docker CLI.                     |
| `LDS_INSECURE_NO_AUTH`       | `false`          | Run without a token. Only for throwaway test setups.                  |
| `LDS_ALLOW_TLD_ZONES`        | `false`          | Allow a whole top-level domain like `lan` in `LDS_ZONES`.             |

### Your own dnsmasq settings

Need something extra from dnsmasq? Put `.conf` files in the `./dnsmasq.d`
folder next to `docker-compose.yml`, then run `docker compose restart`.
These files are trusted as-is, so only you should be able to edit them. If
you add DHCP, also add `NET_ADMIN` and `NET_RAW` to `cap_add`.

### Your data

Records you add are saved in `./data/records.json`, with the previous version
kept as `records.json.bak`. Dashboard settings are in `./data/settings.json`,
and the generated token in `./data/admin-token`. Back up the folder and you've backed up
everything. Docker records aren't saved, because they're rebuilt from your
containers.

---

## Good to know

- **Most changes are instant and seamless.** Plain records (A/AAAA), including
  all Docker records, are reloaded live without interrupting DNS.
- **Wildcard and CNAME changes restart dnsmasq.** The restart takes a few
  milliseconds, and LDS waits at least 2 seconds between restarts.
- **Mistakes can't break DNS.** Every change is tested first. If something
  still goes wrong at startup, LDS falls back to the last config that worked,
  or to no local records, so lookups keep working. The web page then shows
  what happened.
- **It looks after itself.** If dnsmasq crashes, it's restarted within a
  couple of seconds. The health check sends a real DNS query, and you can see
  the result in `docker ps`.
- **Names in your zones never leave your network.** Unknown names like
  `typo.home.lan` get "no such name" right away instead of being sent to the
  internet.
- **No ad-blocking.** LDS only does local names and forwarding. If you want
  ad-blocking, set `LDS_UPSTREAMS` to a filtering DNS service such as AdGuard
  DNS (`94.140.14.14,94.140.15.15`).

---

## Troubleshooting

**LDS won't start and prints "bad settings".**
The message says which setting is wrong and how to fix it. A common one is
`LDS_TOKEN_FILE` pointing at a file that doesn't exist.

**I can't sign in / I lost the token.**
Show the generated token with `docker exec lds cat /data/admin-token`. If you
set your own with `LDS_TOKEN_FILE`, it's in that file.

**The Settings tab says my dashboard settings were ignored.**
`./data/settings.json` couldn't be read or has a value that's no longer
valid (for example after an upgrade). LDS is running on the
`docker-compose.yml` values. Save the settings again to fix the file.

**The page says dnsmasq stopped, or the logs say "Address in use".**
Something else is using port 53. Run `sudo ss -lunp 'sport = :53'` to see
what it is, then go back to [step 1](#1-free-up-port-53).

**The page says "unknown Host".**
You opened it under a name LDS doesn't know. Use the server's IP, a name
inside your zones, or add the name to `LDS_ALLOWED_HOSTS`.

**A record is refused as "outside the domains this server manages".**
Add its domain under **Settings → Domains** (or to `LDS_ZONES`).

**A name doesn't work.**

1. Ask LDS directly, so other DNS servers aren't involved:
   `nslookup nas.home.lan <your-server-ip>`.
2. If that works, your device isn't using LDS yet. Check your router's DHCP
   settings, then reconnect the device.
3. If it doesn't work, run `lds check nas.home.lan`. It shows which record
   decides the answer, or that there is none.

**Devices on a VPN or another subnet get no answers.**
When `LDS_DNS_LISTEN` is empty, LDS only answers networks it's directly
connected to. Set `LDS_DNS_LISTEN` to the IPs it should answer on.

**Some outside sites stopped working.**
- If they're company sites that resolve to private IPs (split DNS), the
  rebinding protection is blocking them. Add
  `rebind-domain-ok=/corp.example.com/` in a file in `./dnsmasq.d`.
- If many sites fail at once, check the server's clock. DNSSEC needs the
  correct time.

**The page says "LDS isn't allowed to stop dnsmasq".**
The container is missing the `KILL` capability, probably because
`docker-compose.yml` is from an older version. Make sure it has
`cap_add: [NET_BIND_SERVICE, SETUID, SETGID, KILL]`, then run
`docker compose up -d`.

**I get "too many wrong tokens".**
That address sent 10 wrong tokens, so it's limited to one try per second for
a few minutes. Wait a second and try again with the right token.

**A Docker app doesn't get a name.**

- Is the name inside your domains (**Settings → Domains**)?
- Is the Traefik IP set (**Settings → Traefik IP**)? The top of the web page
  should say "Docker synced".
- Is `lds-docker-proxy` running? (`docker ps`)
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

The web page and the CLI both use this small JSON API.

| Method   | Path                        | Token  | What it does                                     |
| -------- | --------------------------- | ------ | ------------------------------------------------ |
| `GET`    | `/api/records`              | yes    | List all records                                 |
| `POST`   | `/api/records`              | yes    | Add a record: `{"name": "...", "value": "...", "comment": "..."}` |
| `DELETE` | `/api/records/{name or id}` | yes    | Remove a record (a name removes all with that name); returns what was removed |
| `GET`    | `/api/check?name=...`       | yes    | What a name resolves to, and which record decides it |
| `GET`    | `/api/status`               | yes    | Health of dnsmasq, Docker and replication        |
| `GET`    | `/api/settings`             | yes    | Every setting, its value and where it comes from |
| `PUT`    | `/api/settings`             | yes    | Change settings: `{"set": {"zones": ["home.lan"]}, "reset": ["upstreams"], "dry_run": false}` |
| `GET`    | `/api/info`                 | no     | Whether a token is needed                        |
| `GET`    | `/healthz`                  | no     | `ok` if DNS answers a test query                 |

- Send the token as `Authorization: Bearer <token>`.
- Send bodies with `Content-Type: application/json`.
- With `"dry_run": true`, a settings change is only checked: you get any
  warnings (like records that would stop being served) and nothing changes.

```sh
curl -X POST http://192.168.1.5:5380/api/records \
  -H "Authorization: Bearer $(sudo cat data/admin-token)" \
  -H 'Content-Type: application/json' \
  -d '{"name": "nas.home.lan", "value": "192.168.1.10"}'
```

---

## For developers

LDS is written in Go and uses only the standard library.

```
cmd/lds/            the CLI and the serve command, plus the Pi-hole importer
internal/names/     strict checks shared by everything that writes config
internal/config/    reads and checks settings
internal/store/     records: checking, saving, working out types
internal/dnsmasq/   writes dnsmasq's files and runs dnsmasq
internal/docker/    reads Traefik labels and follows container events
internal/replica/   keeps a read-only copy of another server
internal/settings/  settings changed from the dashboard
internal/server/    the API and the web page (ui/)
```

Run the tests (with dnsmasq installed, they also run a real dnsmasq):

```sh
go test ./...
```

Every push also runs `.github/workflows/ci.yml` on GitHub: the tests, a
vulnerability scan, and the real two-container setup from
`docker-compose.yml` with DNS queries against port 53.

Run it locally on spare ports:

```sh
mkdir -p /tmp/lds && chmod 755 /tmp/lds
LDS_DATA_DIR=/tmp/lds \
LDS_DNS_PORT=5354 LDS_DNS_LISTEN=127.0.0.1 go run ./cmd/lds serve

dig @127.0.0.1 -p 5354 nas.home.lan
```

The data folder must be readable by dnsmasq's own user, hence the `chmod`.
