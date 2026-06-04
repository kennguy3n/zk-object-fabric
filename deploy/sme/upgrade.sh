#!/usr/bin/env bash
# Zero-downtime rolling upgrade of the gateway replicas.
#
#   ./upgrade.sh              # build/pull the new image, roll replicas
#   ZKOF_IMAGE=...:v2 ./upgrade.sh
#
# Strategy: with two replicas behind Traefik, take them out one at a
# time. For each old container we POST /internal/drain (the gateway then
# fails readiness and Traefik's health check pulls it from rotation
# while in-flight requests finish), wait the drain grace period, remove
# it, and let compose start a fresh replica on the new image. The
# surviving replica serves traffic throughout, so there is no downtime.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.production.yml}"
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/.env}"
DRAIN_GRACE="${DRAIN_GRACE:-30}"
READY_TIMEOUT="${READY_TIMEOUT:-120}"
HEALTH_PORT="${HEALTH_PORT:-8082}"

compose() { docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" "$@"; }

# Number of gateway replicas to maintain throughout the roll.
replicas="$(compose ps -q gateway | wc -l | tr -d ' ')"
if [ "$replicas" -lt 1 ]; then
    echo "upgrade: no running gateway replicas; run 'up -d' first" >&2
    exit 1
fi
echo "upgrade: maintaining $replicas gateway replica(s)"

echo "upgrade: building/pulling the new gateway image ..."
if [ -n "${ZKOF_IMAGE:-}" ]; then
    compose pull gateway || true
fi
compose build gateway

wait_ready() {
    # $1 = container id. Polls /internal/ready on the dedicated health
    # listener from inside the container until it returns 200 or we hit
    # READY_TIMEOUT.
    local cid="$1" waited=0
    while [ "$waited" -lt "$READY_TIMEOUT" ]; do
        if docker exec "$cid" wget -q -O - \
            "http://127.0.0.1:$HEALTH_PORT/internal/ready" >/dev/null 2>&1; then
            return 0
        fi
        sleep 3
        waited=$((waited + 3))
    done
    return 1
}

# Snapshot the replicas we must replace. New containers created during
# the roll get fresh IDs, so this fixed list is our work queue.
mapfile -t old_ids < <(compose ps -q gateway)

for cid in "${old_ids[@]}"; do
    short="$(echo "$cid" | cut -c1-12)"
    echo "upgrade: draining replica $short ..."
    # POST with an empty body; drain flips the node to the draining
    # state so readiness starts failing and Traefik stops routing to it.
    docker exec "$cid" wget -q -O - --post-data='' \
        "http://127.0.0.1:$HEALTH_PORT/internal/drain" >/dev/null 2>&1 || true

    echo "upgrade: waiting ${DRAIN_GRACE}s for in-flight requests ..."
    sleep "$DRAIN_GRACE"

    echo "upgrade: removing $short and starting a replacement ..."
    docker rm -f "$cid" >/dev/null

    # Recreate the missing replica on the new image. The surviving
    # replica(s) keep serving while this one comes up.
    compose up -d --no-deps --no-recreate --scale gateway="$replicas" gateway

    # Identify the freshly created replica (a current ID not in old set).
    new_cid=""
    for c in $(compose ps -q gateway); do
        is_old=0
        for o in "${old_ids[@]}"; do
            [ "$c" = "$o" ] && is_old=1 && break
        done
        [ "$is_old" -eq 0 ] && new_cid="$c"
    done

    if [ -z "$new_cid" ]; then
        echo "upgrade: could not locate replacement replica" >&2
        exit 1
    fi
    new_short="$(echo "$new_cid" | cut -c1-12)"
    echo "upgrade: waiting for $new_short to become ready ..."
    if ! wait_ready "$new_cid"; then
        echo "upgrade: replacement $new_short never became ready; aborting" >&2
        exit 1
    fi
    echo "upgrade: replica $new_short ready"

    # Fold the new container into the "known" set so the next iteration
    # does not mistake it for a replacement.
    old_ids+=("$new_cid")
done

echo "upgrade: complete — all replicas on the new image."
