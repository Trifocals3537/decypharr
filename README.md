# Decypharr

![ui](docs/src/assets/images/index.png)

**Decypharr** is a **Media Gateway** for Debrid services and Usenet written in Go.

## What is Decypharr?

Decypharr provides a unified interface for Sonarr, Radarr, and other *Arr applications to access Debrid providers and
Usenet streaming.

## Features

- Mock Qbittorent and Sabnzbd API that supports the Arrs (Sonarr, Radarr, Lidarr etc)
- Multiple Debrid and usenet providers support with a single interface
- Direct Usenet streaming via NNTP (no separate download client required)
- Optional signed STRM library for mountless Plex, Jellyfin, and Emby playback

## Supported Debrid Providers

- [Real Debrid](https://real-debrid.com)
- [Torbox](https://torbox.app)
- [Debrid Link](https://debrid-link.com)
- [All Debrid](https://alldebrid.com)
- [Premiumize](https://www.premiumize.me)

## Quick Start

### Native Linux

Native binaries are the primary deployment path for seedboxes and other
user-managed Linux hosts. Download the archive for your architecture from this
fork's [GitHub Releases](https://github.com/Trifocals3537/decypharr/releases),
verify it with the published `SHA256SUMS`, and install it:

```bash
sha256sum --check --ignore-missing SHA256SUMS
tar -xzf decypharr_Linux_x86_64.tar.gz
install -Dm755 decypharr ~/.local/bin/decypharr
mkdir -p ~/.decypharr
~/.local/bin/decypharr --config ~/.decypharr
```

The first launch creates the configuration and listens on
`127.0.0.1:8282`. From another computer, use an SSH tunnel such as
`ssh -L 8282:127.0.0.1:8282 user@seedbox`, then open
`http://127.0.0.1:8282`. Put a trusted reverse proxy in front of Decypharr or
explicitly change `bind_address` only when remote access is intended.

On Ubuntu or Debian, the host needs FUSE support and the compatible FUSE
runtime (commonly `libfuse2`). Rclone is needed only when using the rclone
mount backend. Existing installations can validate their configuration without
starting services:

```bash
~/.local/bin/decypharr --config ~/.decypharr --check-config
```

For a shared seedbox that must listen beyond loopback, create credentials
interactively on the host before exposing the new process:

```bash
~/.local/bin/decypharr --config ~/.decypharr --set-auth admin
```

Use the host's TLS reverse proxy for the single assigned Decypharr port.
Remote first-time registration is intentionally rejected on non-loopback
listeners.

On a shared host, bind Decypharr directly to a provider-assigned private
address instead of `0.0.0.0` when one is available. The native installation
guide includes a rootless private HTTPS setup that keeps Decypharr
username/password authentication as a second layer and uses no additional
public port.

The release archive includes a user-level systemd unit. See the
[native installation guide](docs/src/content/docs/guides/installation.md) for
service and non-systemd options.

### Docker

Docker remains supported, but it is not required:

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

If Plex, Jellyfin, Emby, or another consumer runs in a separate Linux
container, do not use Docker's default `rprivate` bind for the Decypharr mount.
Mount it read-only with `rslave` so a Decypharr unmount/remount propagates from
the host into the already-running media container:

```yaml
services:
  jellyfin:
    volumes:
      - type: bind
        source: /mnt/decypharr
        target: /mnt/decypharr
        read_only: true
        bind:
          propagation: rslave
```

Without this, the host mount can be healthy while the media container keeps the
old FUSE mount and reports `Transport endpoint is not connected`. See the
[Docker installation notes](docs/src/content/docs/guides/installation.md#media-server-containers)
for verification and managed-host guidance.

> Prefer not to self-host? A managed Decypharr instance is available
> via [ElfHosted](https://store.elfhosted.com/product/decypharr/?utm_source=github&utm_medium=readme&utm_campaign=decypharr-readme),
> preconfigured alongside Sonarr/Radarr to route requests to your debrid provider (7-day trial).

## Documentation

See the [documentation source](docs/src/content/docs/) while the fork's
standalone documentation site is being prepared.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
