# go-wol-ssh

A tiny TCP reverse proxy that wakes sleeping machines via Wake-on-LAN **before**
forwarding the connection - so `ssh user@ssh.yourdomain.com` just works, even
when the target machine is powered off.

Inspired by [go-wol-proxy](https://github.com/darksworm/go-wol-proxy), which
does the same thing for HTTP. `go-wol-ssh` works at the TCP layer, so it can
wake machines on any port-based protocol (SSH, RDP, VNC, etc.) - no client-side
`ProxyCommand` or special app support required.

## How it works

```
ssh user@ssh.domain.com -p 2222
        │
        ▼
┌───────────────────────────────┐
│ go-wol-ssh (Docker container) │
│                               │
│ 1. Dial target:22 (2s)        │
│    └─ already up? proxy now   │
│ 2. Send WOL magic packet      │
│ 3. Poll target:22 until up    │
│ 4. io.Copy both ways          │
└───────────────────────────────┘
        │
        ▼
  target machine:22
```

Each machine gets its own listening port. Connect to `ssh.domain.com:2222` for
machine A, `ssh.domain.com:2223` for machine B, and so on.

## Configuration

Create `config.yaml` (see [`config.example.yaml`](./config.example.yaml)):

```yaml
listen_host: "0.0.0.0"
wake_timeout: 120               # seconds to wait for the machine to wake
poll_interval: 3                # seconds between reachability checks
keepalive_packets_interval: 30  # seconds between WOL keepalive packets

machines:
  - label: "Gaming PC"
    port: 2222                      # the port clients connect to
    ip: "192.168.100.2"             # target machine IP
    mac: "aa:bb:cc:dd:ee:ff"        # target MAC for the magic packet
    broadcast: "192.168.100.255"    # subnet broadcast address
    ssh_port: 22                    # port to proxy/probe on the target
    wol_port: 9                     # WOL UDP port (7 or 9)
    keepalive_packets: true         # send periodic WOL packets while connected
    on_disconnect: "systemctl suspend"  # optional: run this command after each session ends

  - label: "Workstation"
    port: 2223
    ip: "192.168.100.3"
    mac: "11:22:33:44:55:66"
    broadcast: "192.168.100.255"
    ssh_port: 22
    wol_port: 9
    keepalive_packets: false
```

### Field reference

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `listen_host` | no | `0.0.0.0` | Interface to bind all listeners on |
| `wake_timeout` | no | `120` | Seconds to wait before giving up on a wake |
| `poll_interval` | no | `3` | Seconds between reachability checks |
| `keepalive_packets_interval` | no | `30` | Seconds between WOL keepalive packets when `keepalive_packets` is enabled |
| `machines[].label` | yes | - | Human-readable label (used in logs) |
| `machines[].port` | yes | - | Port clients connect to; must be unique per machine |
| `machines[].ip` | yes | - | Target machine's LAN IP |
| `machines[].mac` | yes | - | Target machine's MAC (colon or dash separated) |
| `machines[].broadcast` | yes | - | Subnet broadcast address for WOL |
| `machines[].ssh_port` | no | `22` | Port to probe and proxy on the target |
| `machines[].wol_port` | no | `9` | UDP port for WOL magic packet |
| `machines[].keepalive_packets` | no | `false` | Send a WOL packet every `keepalive_packets_interval` seconds while at least one connection is active. If the machine suspends mid-session, this re-wakes it within one interval so the SSH connection can resume without a manual reconnect. |
| `machines[].on_disconnect` | no | - | Shell command (`sh -c`) to run on the proxy host after a session ends. Only fires when a connection was successfully proxied; skipped if the machine never woke up. Runs in the background so it does not block new connections. |

## Deployment (Docker)

`network_mode: host` is required so UDP broadcasts reach the LAN. This works
fine inside an unprivileged Proxmox LXC container with `features: nesting=1`
(same setup as go-wol-proxy).

`docker-compose.yml`:

```yaml
services:
  ssh-wol:
    image: ghcr.io/karamanliev/go-wol-ssh:latest
    network_mode: host
    restart: unless-stopped
    volumes:
      - ./config.yaml:/etc/ssh-wol/config.yaml:ro
```

Run it:

```bash
docker compose up -d
docker compose logs -f
```

Update to the latest image:

```bash
docker compose pull && docker compose up -d
```

## DNS setup

Point `ssh.yourdomain.com` at the host (LXC container, VM, etc.) running
`go-wol-ssh`. Pick whichever local DNS resolver you use.

### Pi-hole

1. Open the Pi-hole admin UI.
2. Go to **Local DNS → DNS Records**.
3. Add a record:
   - **Domain:** `ssh.yourdomain.com`
   - **IP Address:** `<IP of the machine/LXC running go-wol-ssh>`
4. Click **Add**.

That's it - Pi-hole picks it up immediately. Verify with:

```bash
dig @<pihole-ip> ssh.yourdomain.com +short
```

### AdGuard Home

1. Open the AdGuard Home admin UI.
2. Go to **Filters → DNS rewrites**.
3. Click **Add DNS rewrite**:
   - **Domain:** `ssh.yourdomain.com`
   - **Answer:** `<IP of the machine/LXC running go-wol-ssh>`
4. Click **Save**.

Verify with:

```bash
dig @<adguard-ip> ssh.yourdomain.com +short
```

## Usage

From any SSH client - terminal, Termius on iOS, VS Code Remote, etc.:

```bash
ssh -i ~/.ssh/mykey -p 2222 user@ssh.yourdomain.com   # Gaming PC
ssh -i ~/.ssh/mykey -p 2223 user@ssh.yourdomain.com   # Workstation
```

First connection after a cold boot takes ~10–30s (the time for the machine to
POST and `sshd` to start). Follow-up connections are immediate.

Optional `~/.ssh/config` convenience (not required, just ergonomic):

```
Host pc
  HostName ssh.yourdomain.com
  Port 2222
  User myuser
  IdentityFile ~/.ssh/mykey
  ServerAliveInterval 30

Host workstation
  HostName ssh.yourdomain.com
  Port 2223
  User myuser
  IdentityFile ~/.ssh/mykey
  ServerAliveInterval 30
```

Then just `ssh pc` or `ssh workstation`.

## Building locally

```bash
go build -o ssh-wol .
./ssh-wol ./config.yaml
```

Or build the Docker image:

```bash
docker build -t go-wol-ssh .
docker run --rm --network host -v "$PWD/config.yaml:/etc/ssh-wol/config.yaml:ro" go-wol-ssh
```

## License

MIT
