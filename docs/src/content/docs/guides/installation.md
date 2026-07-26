---
title: Installation
description: Install Decypharr as a native Linux service or with Docker.
---

## Native Linux (Recommended)

Download the archive for your architecture from this fork's
[GitHub Releases](https://github.com/Trifocals3537/decypharr/releases).
Linux hosts need FUSE support and a compatible FUSE runtime (commonly
`libfuse2` on Ubuntu and Debian). Install rclone only if you plan to use the
rclone mount backend.

```bash
# Verify the downloaded archive against SHA256SUMS, then extract it.
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf decypharr_Linux_x86_64.tar.gz

install -Dm755 decypharr ~/.local/bin/decypharr
mkdir -p ~/.decypharr

# First launch: creates config.json and starts the setup wizard.
~/.local/bin/decypharr --config ~/.decypharr
```

Native installs listen on `127.0.0.1:8282` by default so the unauthenticated
first-run setup wizard is not exposed to the network. To configure a remote
seedbox, open a tunnel:

```bash
ssh -L 8282:127.0.0.1:8282 user@seedbox
```

Then visit `http://127.0.0.1:8282`. For permanent remote access, prefer a
trusted reverse proxy. Set `bind_address` explicitly only when you intend to
listen on another interface. Decypharr logs a security warning when
authentication is disabled on a non-loopback listener because the UI, APIs,
and provider configuration can otherwise be exposed over plain HTTP.

### Shared seedboxes

A native shared-seedbox installation needs only one assigned inbound
application port. The Web UI, qBittorrent-compatible API, SABnzbd-compatible
API, and WebDAV routes share Decypharr's HTTP listener. Real-Debrid and TorBox
use outbound HTTPS connections; Usenet providers, including an NNTP endpoint
supplied by TorBox, use outbound NNTP or NNTPS connections. Those outbound
connections do not require additional assigned application ports.

Prefer the DFS mount backend when the host supplies `/dev/fuse` and allows
user mounts. DFS does not start rclone's remote-control listener. Select the
rclone backend only when rclone is installed and its extra local control
listener is acceptable on the host.

Before replacing an existing seedbox binary:

1. Run `decypharr --config PATH --check-config`.
2. Confirm `bind_address`, `port`, and `use_auth` in the effective
   configuration.
3. Confirm the download, mount, cache, and Usenet buffer paths are owned and
   writable by the service account.
4. Confirm `/dev/fuse` and `fusermount3` are available when using DFS.
5. Stop the old process cleanly and verify its FUSE mount is gone before
   starting the replacement.

`--check-config` is for an existing configuration and never creates or changes
files:

```bash
~/.local/bin/decypharr --config ~/.decypharr --check-config
```

### Run as a user service

Linux release archives include a systemd user-service template. This keeps the
service under your account and does not require Docker or root access after the
host's FUSE prerequisites have been installed.

```bash
mkdir -p ~/.config/systemd/user
install -m 0644 decypharr.service ~/.config/systemd/user/decypharr.service

systemctl --user daemon-reload
systemctl --user enable --now decypharr
systemctl --user status decypharr
```

The supplied unit uses `~/.decypharr`, matching the application's native
default. Before relying on a user service after logout, check whether lingering
is enabled:

```bash
loginctl show-user "$USER" -p Linger
```

If it reports `Linger=no`, ask the host administrator or seedbox provider to
enable lingering for your account. Some managed seedboxes do not expose a user
systemd manager at all; in that case, use the provider's service manager,
supervisord, s6, or another process supervisor. As a basic non-restarting
fallback:

```bash
nohup ~/.local/bin/decypharr --config ~/.decypharr \
  >> ~/.decypharr/decypharr.log 2>&1 &
```

Logs for the systemd service are available with:

```bash
journalctl --user -u decypharr -f
```

## Docker (Alternative)

### Docker Compose

Create a `docker-compose.yml`:

```yaml
services:
  decypharr:
    image: ghcr.io/trifocals3537/decypharr:beta
    container_name: decypharr
    ports:
      - "8282:8282"
    volumes:
      - /mnt/:/mnt:rshared
      - ./configs/:/app # config.json must be in this directory
    restart: unless-stopped
    devices:
      - /dev/fuse:/dev/fuse:rwm
    cap_add:
      - SYS_ADMIN
    security_opt:
      - apparmor:unconfined
```

Run:

```bash
docker compose up -d
```

Access at `http://localhost:8282`

### Docker Run

```bash
docker run -d \
  --name=decypharr \
  -p 8282:8282 \
  -v ./config:/app \
  -v ./downloads:/downloads \
  -v ./cache:/cache \
  -e PUID=1000 \
  -e PGID=1000 \
    --restart unless-stopped \
    --device /dev/fuse:/dev/fuse:rwm \
    --cap-add SYS_ADMIN \
    --security-opt apparmor:unconfined \
  ghcr.io/trifocals3537/decypharr:beta
```

The container image explicitly listens on `0.0.0.0`; publish only the ports you
intend to expose.

## Managed (ElfHosted)

Prefer not to self-host? A managed Decypharr instance is available
via [ElfHosted](https://store.elfhosted.com/product/decypharr/?utm_source=github&utm_medium=docs&utm_campaign=decypharr-docs),
preconfigured alongside Sonarr/Radarr and connected to your debrid provider. Includes a 7-day trial.

## Next Steps

After installation, access the web UI. You'll be redirected to the [Setup Wizard](./quick-start/) for first-run
configuration.
