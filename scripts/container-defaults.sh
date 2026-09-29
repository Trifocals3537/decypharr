#!/bin/sh
set -e

# Keep security defaults identical for the main process and Docker healthcheck.
# Test variable presence rather than truthiness so an explicit value is retained.
if [ "${TESSARR_ENABLE_WEBDAV_AUTH+x}" != "x" ]; then
    TESSARR_ENABLE_WEBDAV_AUTH=true
fi
export TESSARR_ENABLE_WEBDAV_AUTH

exec "$@"
