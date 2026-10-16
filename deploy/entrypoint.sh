#!/bin/sh
# Starts Calendar Dynamics. With a backup bucket configured (BUCKET_NAME is
# set automatically by `fly storage create`), it:
#   1. restores the database from the bucket if this volume is empty
#      (new machine, or a lost volume), then
#   2. runs the app under `litestream replicate`, which streams every change
#      to the bucket within about a second.
set -e
export DB_PATH="${DB_PATH:-/data/app.db}"
mkdir -p "$(dirname "$DB_PATH")"

if [ -n "$BUCKET_NAME" ]; then
  litestream restore -config /etc/litestream.yml -if-db-not-exists -if-replica-exists "$DB_PATH"
  exec litestream replicate -config /etc/litestream.yml -exec "/app/server serve"
fi

echo "WARNING: BUCKET_NAME is not set — running WITHOUT continuous backups." >&2
exec /app/server serve
