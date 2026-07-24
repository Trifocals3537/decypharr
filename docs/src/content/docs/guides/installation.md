---
title: Installation
description: Install Decypharr via Docker or binary.
---

## Docker (Recommended)

### Docker Compose

Create a `docker-compose.yml`:

```yaml
services:
  decypharr:
    image: cy01/blackhole:latest
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
  sirrobot01/decypharr:latest
```

## Binary

Download the latest release from [GitHub Releases](https://github.com/sirrobot01/decypharr/releases).

```bash
# Verify the downloaded archive against SHA256SUMS, then extract it
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf decypharr_Linux_x86_64.tar.gz

# Run directly
./decypharr --config /path/to/
```

### Run as a user service

Linux release archives include a systemd user-service template. This keeps the
service under your account and does not require Docker or root access after the
host's FUSE prerequisites have been installed.

```bash
mkdir -p ~/.local/bin ~/.config/decypharr ~/.config/systemd/user
install -m 0755 decypharr ~/.local/bin/decypharr
install -m 0644 decypharr.service ~/.config/systemd/user/decypharr.service

systemctl --user daemon-reload
systemctl --user enable --now decypharr
systemctl --user status decypharr
```

The supplied unit starts Decypharr with
`--config ~/.config/decypharr`. To use an existing configuration elsewhere,
edit the installed unit's `ExecStart` path before enabling it.

Logs are available without Docker:

```bash
journalctl --user -u decypharr -f
```

## Managed (ElfHosted)

Prefer not to self-host? A managed Decypharr instance is available
via [ElfHosted](https://store.elfhosted.com/product/decypharr/?utm_source=github&utm_medium=docs&utm_campaign=decypharr-docs),
preconfigured alongside Sonarr/Radarr and connected to your debrid provider. Includes a 7-day trial.

## Next Steps

After installation, access the web UI. You'll be redirected to the [Setup Wizard](./quick-start/) for first-run
configuration.
