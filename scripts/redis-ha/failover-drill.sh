#!/usr/bin/env bash
# Redis failover drill (#231): runs the stores of the server against Redis
# Sentinel and against Redis Cluster, kills a primary in each, and checks
# that the server keeps working and that the security counters survive.
#
# It starts, on the host network:
#   - a primary with two replicas and three sentinels that monitor it, so
#     that a promoted replica still has a replica of its own and durable
#     writes (redis.durable_writes.replicas: 1) keep working;
#   - a cluster of three primaries with two replicas each, for the same
#     reason.
# Then it runs the TestHA tests of internal/redisutil, which find these
# nodes in REDIS_HA_* variables and stop a primary with SHUTDOWN NOSAVE. It
# exits with 0 only when every test passes, and removes its containers.
#
# Usage: scripts/redis-ha/failover-drill.sh   (from the repository root;
# needs Docker and Go). REDIS_IMAGE selects the Redis image, and
# SENTINEL_BASE and CLUSTER_BASE the first port of each setup.
set -euo pipefail

REDIS_IMAGE="${REDIS_IMAGE:-redis:7-alpine}"
NAME="redis-ha-drill-$$"
SENTINEL_BASE="${SENTINEL_BASE:-6390}"
CLUSTER_BASE="${CLUSTER_BASE:-7390}"
MASTER_NAME="crypto-exchange"

containers=()
log() { printf '\n== %s\n' "$*"; }

cleanup() {
	if [ "${#containers[@]}" -gt 0 ]; then
		docker rm -f "${containers[@]}" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

# run NAME ARGS... starts a container of REDIS_IMAGE on the host network.
run() {
	local name="$NAME-$1"
	shift
	containers+=("$name")
	docker run -d --rm --name "$name" --network host "$REDIS_IMAGE" "$@" >/dev/null
}

wait_ping() {
	local port="$1"
	for _ in $(seq 1 30); do
		if docker exec "${containers[0]}" redis-cli -p "$port" ping 2>/dev/null | grep -q PONG; then
			return 0
		fi
		sleep 0.5
	done
	echo "nothing answers on port $port" >&2
	return 1
}

docker pull -q "$REDIS_IMAGE" >/dev/null

log "Sentinel: a primary, two replicas, three sentinels"
primary=$SENTINEL_BASE
run primary redis-server --port "$primary" --save "" --appendonly no
wait_ping "$primary"
for i in 1 2; do
	run "replica-$i" redis-server --port $((primary + i)) --save "" --appendonly no --replicaof 127.0.0.1 "$primary"
	wait_ping $((primary + i))
done
sentinels=()
for i in 1 2 3; do
	port=$((primary + 20000 + i))
	# A sentinel rewrites its configuration file, so it gets one of its own.
	run "sentinel-$i" sh -c "printf '%s\n' \
		'port $port' \
		'sentinel monitor $MASTER_NAME 127.0.0.1 $primary 2' \
		'sentinel down-after-milliseconds $MASTER_NAME 1000' \
		'sentinel failover-timeout $MASTER_NAME 10000' \
		'sentinel parallel-syncs $MASTER_NAME 1' > /tmp/sentinel.conf && exec redis-sentinel /tmp/sentinel.conf"
	wait_ping "$port"
	sentinels+=("127.0.0.1:$port")
done

log "Cluster: three primaries with two replicas each"
nodes=()
for i in 0 1 2 3 4 5 6 7 8; do
	port=$((CLUSTER_BASE + i))
	run "node-$i" redis-server --port "$port" --save "" --appendonly no \
		--cluster-enabled yes --cluster-config-file "/tmp/nodes-$port.conf" --cluster-node-timeout 2000 \
		--cluster-announce-ip 127.0.0.1
	wait_ping "$port"
	nodes+=("127.0.0.1:$port")
done
docker exec "${containers[0]}" redis-cli --cluster create "${nodes[@]}" --cluster-replicas 2 --cluster-yes >/dev/null
for _ in $(seq 1 30); do
	if docker exec "${containers[0]}" redis-cli -p "$CLUSTER_BASE" cluster info | grep -q 'cluster_state:ok'; then
		break
	fi
	sleep 1
done

log "Failover tests"
export REDIS_HA_SENTINEL_MASTER="$MASTER_NAME"
REDIS_HA_SENTINEL_ADDRESSES="$(IFS=,; echo "${sentinels[*]}")"
REDIS_HA_CLUSTER_ADDRESSES="$(IFS=,; echo "${nodes[*]}")"
export REDIS_HA_SENTINEL_ADDRESSES REDIS_HA_CLUSTER_ADDRESSES
go test ./internal/redisutil/ -run '^TestHA' -count=1 -v

log "The Redis failover drill passed"
