#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=deploy-common.sh
source "$ROOT_DIR/bin/deploy-common.sh"

ENV_FILE=${ENV_FILE:-$ROOT_DIR/.env}
IMAGE=new-api:deploy-linux-amd64
PLATFORM=linux/amd64
APP_VERSION=$(deploy_build_version "$ROOT_DIR")
HEALTH_URL=http://127.0.0.1:3000/api/status
NO_CACHE=${NO_CACHE:-false}
GOPROXY=${GOPROXY:-$DEPLOY_DEFAULT_GOPROXY}
GOPROXY_FALLBACK=${GOPROXY_FALLBACK:-$DEPLOY_DEFAULT_GOPROXY_FALLBACK}
DEPLOY_DATABASE_BACKUP_ENABLED=${DEPLOY_DATABASE_BACKUP_ENABLED:-true}
LOCK_DIR="$ROOT_DIR/.deploy-local.lock"
ROLLBACK_IMAGE="${IMAGE}-rollback"

deploy_ensure_docker_cli
deploy_require_commands docker curl git date od sed grep tail tr awk tee gzip
docker info >/dev/null 2>&1 || deploy_die "Docker daemon is unavailable"
docker buildx version >/dev/null 2>&1 || deploy_die "docker buildx is unavailable"
[[ -f "$ROOT_DIR/docker-compose.local.yml" ]] || deploy_die "Missing deployment file: docker-compose.local.yml"

if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  if find "$LOCK_DIR" -maxdepth 0 -mmin +360 -print -quit 2>/dev/null | grep -q .; then
    rmdir "$LOCK_DIR" 2>/dev/null || true
  fi
  if ! mkdir "$LOCK_DIR" 2>/dev/null; then
    deploy_die "Another local build or deployment is active"
  fi
fi
cleanup() {
  rmdir "$LOCK_DIR" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

deploy_prepare_env_file "$ENV_FILE"
COMPOSE=(
  docker compose
  --env-file "$ENV_FILE"
  -f "$ROOT_DIR/docker-compose.local.yml"
)

deploy_log "Validating local test environment configuration"
"${COMPOSE[@]}" config -q
deploy_build_image "$ROOT_DIR" "$IMAGE" "$PLATFORM" "$APP_VERSION" "$GOPROXY" "$GOPROXY_FALLBACK" "$NO_CACHE"
deploy_assert_image_platform "$IMAGE" "$PLATFORM"
deploy_assert_image_runs "$IMAGE" "$APP_VERSION" "$PLATFORM"
IMAGE_ID=$(deploy_image_id "$IMAGE")

deploy_log "Starting local PostgreSQL and Redis"
for container in postgres redis; do
  if ! docker inspect "$container" >/dev/null 2>&1; then
    "${COMPOSE[@]}" up -d --no-build "$container"
  fi
  for ((attempt = 1; attempt <= 30; attempt++)); do
    health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)
    [[ "$health" == "healthy" ]] && break
    [[ "$health" == "unhealthy" || "$health" == "exited" || "$health" == "dead" ]] && deploy_die "$container failed health check"
    sleep 2
  done
  [[ "$health" == "healthy" ]] || deploy_die "$container health check timed out"
done

if [[ "$DEPLOY_DATABASE_BACKUP_ENABLED" == "true" || "$DEPLOY_DATABASE_BACKUP_ENABLED" == "1" ]]; then
  deploy_backup_postgres "$ROOT_DIR/backups/local-predeploy"
fi

ROLLBACK_AVAILABLE=false
if docker inspect new-api >/dev/null 2>&1; then
  PREVIOUS_IMAGE_ID=$(docker inspect -f '{{.Image}}' new-api)
  docker tag "$PREVIOUS_IMAGE_ID" "$ROLLBACK_IMAGE"
  ROLLBACK_AVAILABLE=true
fi

rollback_local() {
  [[ "$ROLLBACK_AVAILABLE" == true ]] || return 1
  deploy_log "Restoring previous local test image"
  docker tag "$ROLLBACK_IMAGE" "$IMAGE"
  "${COMPOSE[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans new-api
}

DEPLOY_STARTED_AT=$(date +%s)
deploy_log "Updating persistent local test environment"
if ! "${COMPOSE[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans new-api; then
  rollback_local || true
  deploy_die "Failed to recreate the local test service"
fi

RUNNING_IMAGE_ID=$(docker inspect -f '{{.Image}}' new-api)
if [[ "$RUNNING_IMAGE_ID" != "$IMAGE_ID" ]]; then
  rollback_local || true
  deploy_die "Local image mismatch: expected=$IMAGE_ID actual=$RUNNING_IMAGE_ID"
fi

version=""
start_time=""
for ((attempt = 1; attempt <= 60; attempt++)); do
  if status_json=$(curl --noproxy '*' --fail --silent --show-error --max-time 3 \
    --header 'Cache-Control: no-cache' "${HEALTH_URL}?local_deploy=${DEPLOY_STARTED_AT}" 2>/dev/null); then
    version=$(printf '%s' "$status_json" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
    start_time=$(printf '%s' "$status_json" | sed -n 's/.*"start_time":\([0-9][0-9]*\).*/\1/p')
    if [[ "$version" == "$APP_VERSION" && "$start_time" =~ ^[0-9]+$ && "$start_time" -ge "$DEPLOY_STARTED_AT" ]]; then
      break
    fi
  fi
  sleep 2
done
if [[ "$version" != "$APP_VERSION" || ! "$start_time" =~ ^[0-9]+$ || "$start_time" -lt "$DEPLOY_STARTED_AT" ]]; then
  "${COMPOSE[@]}" logs --tail=200 new-api >&2 || true
  rollback_local || true
  deploy_die "Local test environment verification failed"
fi
if ! deploy_verify_relay_routes "${HEALTH_URL%/api/status}"; then
  rollback_local || true
  deploy_die "Local relay route verification failed"
fi

deploy_log "Local test environment ready: url=http://127.0.0.1:3000 image=${IMAGE_ID#sha256:}"
