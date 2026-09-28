# Changelog

Notable user-facing changes to this fork are documented here.

## 2.6.0 - Unreleased

This is the first stable promotion of the fork's storage-reliability work. It
retains existing provider mappings and configuration while making failures
bounded, observable, and recoverable.

### Playback and storage reliability

- Added resumable range streaming, strict response validation, and bounded
  provider retry behavior.
- Added seekable stream sessions, overlapping-read coalescing, probe
  prioritization, and per-file circuit breakers so one unhealthy item does not
  stall unrelated playback.
- Added event-driven repair, durable recovery state, and mount-aware runtime
  readiness.
- Added provider-aware traffic controls and bounded memory/disk buffering.

### Imports and providers

- Added sampled import-readiness checks before media is exposed to Arr
  applications.
- Added deterministic cached-provider preference with controlled uncached
  fallback and faster terminal/seedless-transfer detection.
- Improved recovery and lifecycle handling across Real-Debrid, TorBox,
  AllDebrid, Debrid-Link, and Premiumize.
- Preserved stable torrent output paths and durable torrent sources across
  restarts and provider recovery.

### Usenet

- Added strict NZB segment geometry, yEnc framing, CRC, archive completeness,
  and sampled-content validation.
- Added provider failover, fair multi-client prefetch, bounded connection
  lifecycle, and fast failure for missing articles.
- Added safe portable filename sanitization with collision and traversal
  protection.
- Improved staged-import ownership and cleanup so concurrent work cannot remove
  active metadata.

### Security and operations

- Added safer authentication bootstrap, login throttling, secret-aware
  configuration responses, strict path containment, and safer generated-link
  request handling.
- Added transactional configuration updates and runtime logging controls.
- Added native Linux release artifacts with a GLIBC 2.17 baseline, plus Windows,
  macOS, and multi-architecture container builds.
- Added cross-platform tests, race detection, static analysis, vulnerability
  checks, dependency auditing, CodeQL, Dependabot, and protected release
  branches.

### Upgrade notes

- Existing healthy links, provider mappings, and configuration remain valid.
- Stable container deployments should use
  `ghcr.io/trifocals3537/decypharr:latest`; `beta` remains the preview channel.
- Back up the configuration directory before upgrading and verify the mount and
  media-server reads before retiring the previous binary or image.
