#!/usr/bin/env bash
set -euo pipefail

# Legacy identifiers are allowed only where compatibility, migration, or
# attribution requires their exact spelling. New product-facing use is a bug.
git rev-parse --is-inside-work-tree >/dev/null

failed=0

allowed_legacy_reference() {
    local path="$1"
    local content="$2"
    case "$path" in
        docs/src/content/docs/guides/migration-from-decypharr.md|\
        internal/migration/legacy.go|internal/migration/legacy_test.go|\
        migration_command.go|migration_command_test.go|\
        pkg/manager/legacy_artifact_names.go|pkg/manager/legacy_usenet_adoption.go|\
        pkg/server/legacy_protocol_names.go|pkg/usenet/fs/reader/legacy_cache_names.go|\
        scripts/check-brand.sh)
            return 0
            ;;
        README.md)
            [[ "$content" == *'[Decypharr](https://github.com/sirrobot01/decypharr)'* ||
               "$content" == *'migration-from-decypharr.md'* ]]
            return
            ;;
        docs/astro.config.mjs)
            [[ "$content" == *"label: 'Migrate from Decypharr', link: '/guides/migration-from-decypharr'"* ]]
            return
            ;;
        main.go)
            [[ "$content" == *'os.Args[1] == "migrate-from-decypharr"'* ]]
            return
            ;;
        pkg/manager/strm.go)
            [[ "$content" == *'decypharr-strm-root'* ]]
            return
            ;;
        pkg/manager/usenet_watcher_identity.go)
            [[ "$content" == *'decypharr/watched-nzb/v1'* ]]
            return
            ;;
        pkg/strm/strm.go)
            [[ "$content" == *'decypharr-strm'* ]]
            return
            ;;
        pkg/usenet/fs/reader/cache.go)
            [[ "$content" == *'decypharr stream cache v1'* ||
               "$content" == *'decypharr segment cache v1'* ]]
            return
            ;;
    esac
    return 1
}

while IFS=: read -r path line content; do
    if ! allowed_legacy_reference "$path" "$content"; then
        printf 'unexpected legacy brand reference: %s:%s:%s\n' "$path" "$line" "$content" >&2
        failed=1
    fi
done < <(git grep -Iin 'decypharr' -- . || true)

while IFS= read -r path; do
    if [[ "$path" != "docs/src/content/docs/guides/migration-from-decypharr.md" ]]; then
        printf 'unexpected legacy brand in tracked path: %s\n' "$path" >&2
        failed=1
    fi
done < <(git ls-files | grep -i 'decypharr' || true)

if (( failed )); then
    exit 1
fi

printf 'Tessarr brand boundary verified.\n'
