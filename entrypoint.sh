#!/bin/sh
# API Key Rotator entrypoint.
# - Without R2_ACCESS_KEY_ID/R2_SECRET_ACCESS_KEY: runs the app directly
#   (local Docker / plain behavior, unchanged).
# - With them set: runs the app under Litestream, which continuously
#   replicates SQLite to the R2 bucket and restores from it on fresh
#   starts (Render free plan has no persistent disk).
set -e

DB_PATH="${DATABASE_PATH:-/app/data/api_key_rotator.db}"
mkdir -p "$(dirname "$DB_PATH")"

if [ -n "$R2_ACCESS_KEY_ID" ] && [ -n "$R2_SECRET_ACCESS_KEY" ]; then
	R2_BUCKET="${R2_BUCKET:-old-union-entire}"
	R2_ENDPOINT="${R2_ENDPOINT:-https://575e29bc16ae64a8ce64c6cf4d5c2d52.r2.cloudflarestorage.com}"
	echo "R2 backup enabled (bucket: $R2_BUCKET), starting under Litestream..."
	# R2 S3 credentials are hex, safe as YAML plain scalars.
	cat > /etc/litestream.yml <<EOF
dbs:
  - path: ${DB_PATH}
    restore-if-db-not-exists: true
    replica:
      url: s3://${R2_BUCKET}/rotator?endpoint=${R2_ENDPOINT}
      region: auto
      access-key-id: ${R2_ACCESS_KEY_ID}
      secret-access-key: ${R2_SECRET_ACCESS_KEY}
      sync-interval: 10s
EOF
	chmod 600 /etc/litestream.yml
	exec litestream replicate -config /etc/litestream.yml -exec "/app/api-key-rotator"
else
	echo "R2 vars not set, running without Litestream backup"
	if [ -f "$DB_PATH" ]; then
		chmod 664 "$DB_PATH" || true
		echo "Database file permissions updated"
	else
		echo "Database file not found, will be created automatically"
	fi
	exec /app/api-key-rotator "$@"
fi
