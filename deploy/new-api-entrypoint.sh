#!/bin/sh
set -eu
for task_name in session_secret crypto_secret; do
    task_file="/run/secrets/$task_name"
    if [ ! -r "$task_file" ] || [ "$(wc -c < "$task_file")" -lt 64 ]; then
        printf 'Missing or invalid secret file: %s (run bash scripts/stack.sh init)\n' "$task_name" >&2
        exit 1
    fi
done
SESSION_SECRET=$(tr -d '\r\n' < /run/secrets/session_secret)
CRYPTO_SECRET=$(tr -d '\r\n' < /run/secrets/crypto_secret)
export SESSION_SECRET CRYPTO_SECRET
exec /new-api "$@"

