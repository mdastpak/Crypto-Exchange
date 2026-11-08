#!/usr/bin/env bash
# RabbitMQ failover drill (#232): runs the delivery of sign-in codes against
# a three-node RabbitMQ cluster, kills each node in turn, and checks that
# codes are still queued and delivered while it is down.
#
# It starts three clustered nodes on a Docker network, publishes their AMQP
# ports on the host, and runs the TestHA tests of internal/notify, which find
# them in RABBITMQ_HA_HOSTS and kill and restart them through Docker by the
# names in RABBITMQ_HA_CONTAINERS. It exits with 0 only when every test
# passes, and removes its containers and network.
#
# Usage: scripts/rabbitmq-ha/failover-drill.sh   (from the repository root;
# needs Docker and Go). RABBITMQ_IMAGE selects the image, and AMQP_BASE the
# first host port.
set -euo pipefail

RABBITMQ_IMAGE="${RABBITMQ_IMAGE:-rabbitmq:3.13}"
NAME="rmq-ha-drill-$$"
AMQP_BASE="${AMQP_BASE:-56721}"
COOKIE="drill-cookie-$$-$RANDOM"

nodes=("$NAME-1" "$NAME-2" "$NAME-3")
log() { printf '\n== %s\n' "$*"; }

cleanup() {
	docker rm -f "${nodes[@]}" >/dev/null 2>&1 || true
	docker network rm "$NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_running() {
	local container="$1"
	for _ in $(seq 1 90); do
		if docker exec "$container" rabbitmq-diagnostics -q check_running >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	docker logs "$container" | tail -30
	echo "$container did not start" >&2
	return 1
}

docker pull -q "$RABBITMQ_IMAGE" >/dev/null
docker network create "$NAME" >/dev/null

log "Three RabbitMQ nodes"
hosts=()
for i in 0 1 2; do
	container="${nodes[$i]}"
	port=$((AMQP_BASE + i))
	docker run -d --name "$container" --hostname "$container" --network "$NAME" \
		-e RABBITMQ_SERVER_ADDITIONAL_ERL_ARGS="-setcookie $COOKIE" -e RABBITMQ_CTL_ERL_ARGS="-setcookie $COOKIE" \
		-e RABBITMQ_DEFAULT_USER=drill -e RABBITMQ_DEFAULT_PASS=drill -p "127.0.0.1:$port:5672" "$RABBITMQ_IMAGE" >/dev/null
	hosts+=("127.0.0.1:$port")
done
for container in "${nodes[@]}"; do
	wait_running "$container"
done

log "Clustering"
for container in "${nodes[1]}" "${nodes[2]}"; do
	docker exec "$container" rabbitmqctl -q stop_app
	docker exec "$container" rabbitmqctl -q join_cluster "rabbit@${nodes[0]}"
	docker exec "$container" rabbitmqctl -q start_app
	wait_running "$container"
done
docker exec "${nodes[0]}" rabbitmqctl -q cluster_status | grep -A4 -i "running nodes" || true

log "Failover tests"
RABBITMQ_HA_HOSTS="$(IFS=,; echo "${hosts[*]}")"
RABBITMQ_HA_CONTAINERS="$(IFS=,; echo "${nodes[*]}")"
# The guest user may connect only from the broker's own host, so the nodes
# get a user of their own; the cluster keeps the one of the first node.
RABBITMQ_HA_USER=drill RABBITMQ_HA_PASSWORD=drill
export RABBITMQ_HA_HOSTS RABBITMQ_HA_CONTAINERS RABBITMQ_HA_USER RABBITMQ_HA_PASSWORD
go test ./internal/notify/ -run '^TestHA' -count=1 -v -timeout 15m

log "The RabbitMQ failover drill passed"
