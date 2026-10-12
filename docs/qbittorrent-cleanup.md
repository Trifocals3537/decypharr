# qBittorrent queue cleanup and provider retention

Servarr uses `deleteFiles=true` both after successful import and when removing
failed downloads. It is not permission to delete a persistent library/provider
entry.

The qBittorrent delete endpoint retires the active-download queue row only:

- With `deleteFiles=true`, owned staging files are removed; persistent main
  entries and provider placements are preserved by default.
- With `deleteFiles=false` or an omitted flag, staging files and provider/main
  entries are preserved.
- An absent queue row is an idempotent success, never a fallback to main deletion.
- The explicit administrative deletion API remains destructive when requested.

## Failed uncached transfers

Provider cancellation is permitted only for a torrent with an uncached-download
failure (`stalledDL`, a recognized watchdog handoff reason, a submitting Arr
category/client endpoint, and a valid active provider placement). The owning Arr
must first validate its uniquely matched qBittorrent endpoint and exact queue
item. A before-delete callback durably records authorization **after validation
and before Arr DELETE**, since Arr can synchronously call the qBittorrent delete
endpoint during that request. No worker/deletion lease is held across this Arr
network call.

Authorization binds the queue incarnation, owner/endpoint, active provider/ID,
failure reason/state and progress baseline. A queue update that changes this
evidence durably revokes authorization; changing it back does not restore it.
Missing or stale authorization falls back to benign queue/staging cleanup.

After workers drain, the final incarnation is classified while preparing its
durable deletion intent. Client intents carry `queued_transfer_only` scope and,
when authorized, exactly one active placement snapshot. Retry/restart recovery
cannot add current main entries or inactive placements to this scope. Unknown
scopes and malformed placement snapshots fail closed. Legacy intents without a
scope retain their previous all-placement semantics.

A placement also present in main storage (even under another hash) is not safe
to cancel: cleanup returns a manual-review error instead of removing a possible
library target. Immediately before provider deletion, collision checking and a
synced placement fence share a short local mutex with every main publication
(including provider synchronization and rollback writes). The mutex is released
before network work: same-hash and cross-hash main writes for the reserved ID
fail closed rather than wait behind provider calls. Retry/recovery uses the same
boundary. Provider failures retain retryable intent; only typed provider-not-found
errors count as already absent.

The fence remains durable even after success or an ambiguous provider failure.
Stale provider listings must not republish a library row backed by a deleted ID.
A different provider ID is unaffected; reuse of a fenced ID requires manual
reconciliation. There is deliberately no automatic fence expiry or clearing API.

Explicit admin removal fulfills main-provider destruction through a separately
synced snapshot in the main deletion tombstone, even when resuming a benign
client intent with no provider callback. Main cleanup runs before queue files;
provider errors preserve the admin obligation for retry/restart. The existing
client intent is neither widened nor upgraded from preserving files.

## Protocol limitation

The qBittorrent delete protocol supplies only a hash, not a queue-incarnation
token. An arbitrarily delayed delete for an old submission cannot be distinguished
from a delete for a new submission of the same hash. Durable authorization is
never inherited by a new incarnation, so such a callback does not acquire old
provider-deletion permission, but it can still retire the new queue/staging row.
This change does not claim to solve that protocol limitation.
