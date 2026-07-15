#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=deploy-common.sh
source "$ROOT_DIR/bin/deploy-common.sh"

ENV_FILE=${ENV_FILE:-$ROOT_DIR/.env}
IMAGE=${IMAGE:-new-api:oauth-local}
PLATFORM=${PLATFORM:-}
APP_VERSION=${APP_VERSION:-${RELEASE_VERSION:-$(deploy_build_version "$ROOT_DIR")}}
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:3000/api/status}
NO_CACHE=${NO_CACHE:-false}
GOPROXY=${GOPROXY:-$DEPLOY_DEFAULT_GOPROXY}
GOPROXY_FALLBACK=${GOPROXY_FALLBACK:-$DEPLOY_DEFAULT_GOPROXY_FALLBACK}
export NEW_API_IMAGE=$IMAGE

deploy_ensure_docker_cli
deploy_require_commands docker curl git date od sed grep tail tr awk
docker info >/dev/null 2>&1 || deploy_die "Docker daemon is unavailable"
docker buildx version >/dev/null 2>&1 || deploy_die "docker buildx is unavailable"
for file in docker-compose.yml docker-compose.deploy.yml docker-compose.local.yml; do
  [[ -f "$ROOT_DIR/$file" ]] || deploy_die "Missing deployment file: $file"
done

deploy_prepare_env_file "$ENV_FILE"
COMPOSE=(
  docker compose
  --env-file "$ENV_FILE"
  -f "$ROOT_DIR/docker-compose.yml"
  -f "$ROOT_DIR/docker-compose.deploy.yml"
  -f "$ROOT_DIR/docker-compose.local.yml"
)

deploy_log "Validating local Compose configuration"
"${COMPOSE[@]}" config -q

deploy_build_image "$ROOT_DIR" "$IMAGE" "$PLATFORM" "$APP_VERSION" "$GOPROXY" "$GOPROXY_FALLBACK" "$NO_CACHE"
deploy_assert_image_platform "$IMAGE" "$PLATFORM"
EXPECTED_IMAGE_ID=$(deploy_image_id "$IMAGE")
deploy_log "Built image: ${EXPECTED_IMAGE_ID#sha256:}"

deploy_log "Starting PostgreSQL and Redis"
"${COMPOSE[@]}" up -d --no-build redis postgres
deploy_log "Recreating new-api"
"${COMPOSE[@]}" up -d --no-build --force-recreate new-api

RUNNING_IMAGE_ID=$(docker inspect -f '{{.Image}}' new-api)
[[ "$RUNNING_IMAGE_ID" == "$EXPECTED_IMAGE_ID" ]] || \
  deploy_die "new-api container image mismatch: expected=$EXPECTED_IMAGE_ID actual=$RUNNING_IMAGE_ID"
deploy_prune_project_images

deploy_log "Waiting for endpoint: $HEALTH_URL"
for ((attempt = 1; attempt <= 45; attempt++)); do
  if status_json=$(curl --fail --silent --show-error --max-time 3 "$HEALTH_URL" 2>/dev/null); then
    version=$(printf '%s' "$status_json" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
    [[ "$version" == "$APP_VERSION" ]] || deploy_die "Running version mismatch: expected=$APP_VERSION actual=${version:-unavailable}"
    deploy_log "Deployment completed: version=$version image=${RUNNING_IMAGE_ID#sha256:} url=http://127.0.0.1:3000"
    exit 0
  fi
  sleep 2
done

"${COMPOSE[@]}" logs --tail=100 new-api >&2 || true
deploy_die "Service failed health check: $HEALTH_URL"
