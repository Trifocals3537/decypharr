#!/usr/bin/env bash
set -euo pipefail

# Legacy identifiers are allowed only where compatibility, migration, or
# attribution requires their exact spelling. New product-facing use is a bug.
git rev-parse --is-inside-work-tree >/dev/null

allowed='^(README\.md|docs/astro\.config\.mjs|docs/src/content/docs/guides/migration-from-decypharr\.md|internal/migration/legacy(_test)?\.go|main\.go|migration_command(_test)?\.go|pkg/manager/legacy_artifact_names\.go|pkg/manager/legacy_usenet_adoption\.go|pkg/manager/strm\.go|pkg/manager/usenet_watcher_identity\.go|pkg/strm/strm\.go|pkg/usenet/fs/reader/cache\.go|scripts/check-brand\.sh)$'
failed=0

while IFS=: read -r path line content; do
    if [[ ! "$path" =~ $allowed ]]; then
        printf 'unexpected legacy brand reference: %s:%s:%s\n' "$path" "$line" "$content" >&2
        failed=1
    fi
done < <(git grep -Iin 'decypharr' -- . || true)

while IFS= read -r path; do
    if [[ ! "$path" =~ $allowed ]]; then
        printf 'unexpected legacy brand in tracked path: %s\n' "$path" >&2
        failed=1
    fi
done < <(git ls-files | grep -i 'decypharr' || true)

if (( failed )); then
    exit 1
fi

printf 'Tessarr brand boundary verified.\n'
