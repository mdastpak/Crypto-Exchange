#!/usr/bin/env bash
# Restore drill (#236): a point-in-time recovery from end to end, on Docker.
#
# It starts a PostgreSQL primary that archives its WAL, migrates it with
# cmd/admin, checks archiving with "admin backup status", takes a base
# backup, records a good change and then a bad one, restores the base backup
# with the archived WAL to a point between the two, and checks the restored
# database with "admin restore verify". It exits with 0 only when every
# step passes, and removes its containers and volumes.
#
# This is the local form of the drill that docs/disaster-recovery.md asks
# to run on staging every month with the real backup tool (pgBackRest or
# WAL-G) and storage; there, only the backup and restore steps differ.
#
# Usage: scripts/dr/restore-drill.sh   (from the repository root; needs
# Docker and Go). PG_IMAGE selects the PostgreSQL image.
set -euo pipefail

PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
NAME="dr-drill-$$"
PRIMARY_PORT="${PRIMARY_PORT:-55432}"
RESTORE_PORT="${RESTORE_PORT:-55433}"
PASSWORD="drill-$$-$RANDOM"
WORK="$(mktemp -d)"

log() { printf '\n== %s\n' "$*"; }

cleanup() {
	docker rm -f "$NAME-primary" "$NAME-restore" >/dev/null 2>&1 || true
	docker volume rm "$NAME-archive" "$NAME-backup" >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

wait_ready() {
	local container="$1"
	for _ in $(seq 1 60); do
		if docker exec "$container" pg_isready -U postgres -d exchange -q 2>/dev/null; then
			# pg_isready answers before recovery has finished promoting.
			if [ "$(docker exec "$container" psql -U postgres -d exchange -Atc 'SELECT NOT pg_is_in_recovery()' 2>/dev/null)" = "t" ]; then
				return 0
			fi
		fi
		sleep 1
	done
	docker logs "$container" | tail -30
	echo "$container did not become ready" >&2
	return 1
}

psql_primary() { docker exec "$NAME-primary" psql -U postgres -d exchange -v ON_ERROR_STOP=1 -Atc "$1"; }

admin() {
	local port="$1"
	shift
	cat >"$WORK/config.yaml" <<YAML
postgres:
  host: "localhost"
  port: "$port"
  user: "postgres"
  password: "$PASSWORD"
  dbname: "exchange"
  sslmode: "disable"
  connect_retries: 5
  connect_retry_delay_ms: 500
YAML
	go run ./cmd/admin -config "$WORK/config.yaml" -operator restore-drill "$@"
}

docker volume create "$NAME-archive" >/dev/null
docker volume create "$NAME-backup" >/dev/null

log "Starting a primary that archives its WAL"
docker run -d --name "$NAME-primary" -p "127.0.0.1:$PRIMARY_PORT:5432" \
	-e POSTGRES_PASSWORD="$PASSWORD" -e POSTGRES_DB=exchange \
	-v "$NAME-archive:/archive" -v "$NAME-backup:/backup" \
	"$PG_IMAGE" -c wal_level=replica -c archive_mode=on -c archive_timeout=60 \
	-c 'archive_command=test ! -f /archive/%f && cp %p /archive/%f' >/dev/null
docker exec "$NAME-primary" sh -c 'until [ -d /archive ]; do sleep 0.2; done; chown postgres /archive /backup'
wait_ready "$NAME-primary"

log "Migrating the primary"
admin "$PRIMARY_PORT" platform status

log "Checking WAL archiving"
psql_primary "SELECT pg_switch_wal()" >/dev/null
sleep 2
admin "$PRIMARY_PORT" backup status -rpo 5m

log "Taking a base backup"
docker exec -u postgres "$NAME-primary" pg_basebackup -U postgres -D /backup/base -X stream -c fast

log "Recording a good change, then a bad one"
psql_primary "INSERT INTO audit_events (hk, actor_type, action, outcome) VALUES (gen_random_uuid(), 'system', 'drill.good_change', 'success')" >/dev/null
GOOD="$(psql_primary "SELECT to_char(MAX(occurred_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') FROM audit_events")"
sleep 2
TARGET_AT="$(psql_primary "SELECT now() AT TIME ZONE 'UTC'")"
TARGET="$(psql_primary "SELECT to_char(TIMESTAMP '$TARGET_AT', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")"
sleep 2
psql_primary "INSERT INTO audit_events (hk, actor_type, action, outcome) VALUES (gen_random_uuid(), 'system', 'drill.bad_change', 'success')" >/dev/null
psql_primary "SELECT pg_switch_wal()" >/dev/null
sleep 2
echo "good change at $GOOD; restoring to $TARGET"

log "Restoring the base backup with the archived WAL to $TARGET"
docker run -d --name "$NAME-restore" -p "127.0.0.1:$RESTORE_PORT:5432" \
	-v "$NAME-archive:/archive:ro" -v "$NAME-backup:/backup:ro" \
	--entrypoint sh "$PG_IMAGE" -c "
		set -e
		mkdir -p /restore && cp -a /backup/base /restore/data
		chown -R postgres /restore && chmod 700 /restore/data
		touch /restore/data/recovery.signal
		cat >> /restore/data/postgresql.auto.conf <<CONF
restore_command = 'cp /archive/%f %p'
recovery_target_time = '$TARGET_AT+00'
recovery_target_action = 'promote'
archive_mode = 'off'
CONF
		exec su-exec postgres postgres -D /restore/data
	" >/dev/null
wait_ready "$NAME-restore"

log "Verifying the restored database"
admin "$RESTORE_PORT" restore verify -not-before "$GOOD" -not-after "$TARGET"

log "The restore drill passed"
