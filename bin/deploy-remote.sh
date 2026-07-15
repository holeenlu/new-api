#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=deploy-common.sh
source "$ROOT_DIR/bin/deploy-common.sh"

: "${DEPLOY_TARGET:?DEPLOY_TARGET is required}"
: "${REMOTE_DIR:?REMOTE_DIR is required}"
: "${TARGET_COMPOSE:?TARGET_COMPOSE is required}"
: "${HEALTH_URL:?HEALTH_URL is required}"

PLATFORM=${PLATFORM:-linux/amd64}
BUILD_IMAGE=${BUILD_IMAGE:-new-api:deploy-amd64}
REMOTE_IMAGE=${REMOTE_IMAGE:-new-api:oauth-local}
APP_VERSION=${APP_VERSION:-${RELEASE_VERSION:-$(deploy_build_version "$ROOT_DIR")}}
NO_CACHE=${NO_CACHE:-false}
GOPROXY=${GOPROXY:-$DEPLOY_DEFAULT_GOPROXY}
GOPROXY_FALLBACK=${GOPROXY_FALLBACK:-$DEPLOY_DEFAULT_GOPROXY_FALLBACK}
EXTRA_SERVICES=${EXTRA_SERVICES:-}

CONTROL_PATH="/tmp/new-api-deploy-ssh-${UID}-$$"
IMAGE_ARCHIVE=$(mktemp "${TMPDIR:-/tmp}/new-api-image.XXXXXX")
REMOTE_ARCHIVE="/tmp/$(basename "$IMAGE_ARCHIVE")"
ASKPASS_SCRIPT=""
SSH_MASTER_ACTIVE=false
SSH_OPTIONS=(
  -o StrictHostKeyChecking=accept-new
  -o ConnectTimeout=15
  -o ControlMaster=auto
  -o ControlPersist=10m
  -o ControlPath="$CONTROL_PATH"
)

cleanup() {
  rm -f "$IMAGE_ARCHIVE" "$ASKPASS_SCRIPT"
  if [[ "$SSH_MASTER_ACTIVE" == true ]]; then
    ssh_remote "$DEPLOY_TARGET" "rm -f '$REMOTE_ARCHIVE'" >/dev/null 2>&1 || true
    ssh_remote -O exit "$DEPLOY_TARGET" >/dev/null 2>&1 || true
  fi
  rm -f "$CONTROL_PATH"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if [[ -n "${SSHPASS:-}" ]]; then
  export SSH_DEPLOY_PASSWORD=$SSHPASS
  unset SSHPASS
  ASKPASS_SCRIPT=$(mktemp "${TMPDIR:-/tmp}/new-api-ssh-askpass.XXXXXX")
  chmod 700 "$ASKPASS_SCRIPT"
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "$SSH_DEPLOY_PASSWORD"' >"$ASKPASS_SCRIPT"
fi

ssh_remote() {
  if [[ -n "$ASKPASS_SCRIPT" ]]; then
    DISPLAY="${DISPLAY:-new-api-deploy}" \
      SSH_ASKPASS="$ASKPASS_SCRIPT" \
      SSH_ASKPASS_REQUIRE=force \
      command ssh "${SSH_OPTIONS[@]}" "$@"
    return
  fi
  command ssh "${SSH_OPTIONS[@]}" "$@"
}

scp_remote() {
  if [[ -n "$ASKPASS_SCRIPT" ]]; then
    DISPLAY="${DISPLAY:-new-api-deploy}" \
      SSH_ASKPASS="$ASKPASS_SCRIPT" \
      SSH_ASKPASS_REQUIRE=force \
      command scp "${SSH_OPTIONS[@]}" "$@"
    return
  fi
  command scp "${SSH_OPTIONS[@]}" "$@"
}

ssh_bash() {
  local remote_command="bash -s --"
  local arg
  for arg in "$@"; do
    printf -v remote_command '%s %q' "$remote_command" "$arg"
  done
  ssh_remote "$DEPLOY_TARGET" "$remote_command"
}

deploy_ensure_docker_cli
deploy_require_commands docker ssh scp tar gzip curl git date awk
docker info >/dev/null 2>&1 || deploy_die "Docker daemon is unavailable"
docker buildx version >/dev/null 2>&1 || deploy_die "docker buildx is unavailable"
for file in docker-compose.yml docker-compose.deploy.yml "$TARGET_COMPOSE"; do
  [[ -f "$ROOT_DIR/$file" ]] || deploy_die "Missing deployment file: $file"
done

deploy_log "Opening SSH connection to $DEPLOY_TARGET"
ssh_remote -MNf "$DEPLOY_TARGET"
SSH_MASTER_ACTIVE=true
ssh_remote "$DEPLOY_TARGET" "docker info >/dev/null 2>&1" || deploy_die "Remote user cannot access Docker"
ssh_remote "$DEPLOY_TARGET" "docker compose version >/dev/null 2>&1" || deploy_die "docker compose is unavailable remotely"

deploy_build_image "$ROOT_DIR" "$BUILD_IMAGE" "$PLATFORM" "$APP_VERSION" "$GOPROXY" "$GOPROXY_FALLBACK" "$NO_CACHE"
deploy_assert_image_platform "$BUILD_IMAGE" "$PLATFORM"
LOCAL_IMAGE_ID=$(deploy_image_id "$BUILD_IMAGE")
LOCAL_IMAGE_VERSION=$(docker image inspect "$BUILD_IMAGE" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')
[[ "$LOCAL_IMAGE_VERSION" == "$APP_VERSION" ]] || \
  deploy_die "Built image version mismatch: expected=$APP_VERSION actual=${LOCAL_IMAGE_VERSION:-unavailable}"
deploy_log "Built image: ${LOCAL_IMAGE_ID#sha256:}"

deploy_log "Synchronizing deployment files to $REMOTE_DIR"
COPYFILE_DISABLE=1 tar \
  --no-xattrs \
  --exclude='./.git' \
  --exclude='./.DS_Store' \
  --exclude='._*' \
  --exclude='./.env' \
  --exclude='./data' \
  --exclude='./logs' \
  --exclude='./backups' \
  --exclude='./caddy' \
  --exclude='./web/node_modules' \
  --exclude='./web/default/node_modules' \
  --exclude='./web/default/dist' \
  --exclude='./web/classic/node_modules' \
  --exclude='./web/classic/dist' \
  -czf - -C "$ROOT_DIR" . \
  | ssh_remote "$DEPLOY_TARGET" "mkdir -p '$REMOTE_DIR' && cd '$REMOTE_DIR' && tar -xzf - && rm -f docker-compose.override.yml"

deploy_log "Preparing remote environment"
ssh_bash "$REMOTE_DIR" <<'REMOTE_ENV'
set -Eeuo pipefail

remote_dir=$1
# shellcheck source=/dev/null
source "$remote_dir/bin/deploy-common.sh"
deploy_require_commands od sed grep tail tr awk
deploy_prepare_env_file "$remote_dir/.env"
REMOTE_ENV

deploy_log "Validating remote Compose configuration"
ssh_bash "$REMOTE_DIR" "$TARGET_COMPOSE" "$REMOTE_IMAGE" <<'REMOTE_VALIDATE'
set -Eeuo pipefail
cd "$1"
export NEW_API_IMAGE=$3
docker compose --env-file .env -f docker-compose.yml -f docker-compose.deploy.yml -f "$2" config -q
REMOTE_VALIDATE

deploy_log "Transferring image"
docker save "$BUILD_IMAGE" | gzip >"$IMAGE_ARCHIVE"
ARCHIVE_SHA256=$(deploy_file_sha256 "$IMAGE_ARCHIVE")
deploy_log "Image archive SHA-256: $ARCHIVE_SHA256"
scp_remote "$IMAGE_ARCHIVE" "$DEPLOY_TARGET:$REMOTE_ARCHIVE"

deploy_log "Loading image and recreating services"
ssh_bash \
  "$REMOTE_DIR" "$REMOTE_ARCHIVE" "$BUILD_IMAGE" "$REMOTE_IMAGE" "$TARGET_COMPOSE" "$EXTRA_SERVICES" "$ARCHIVE_SHA256" "$APP_VERSION" <<'REMOTE_DEPLOY'
set -Eeuo pipefail

remote_dir=$1
remote_archive=$2
build_image=$3
remote_image=$4
target_compose=$5
extra_services=$6
expected_archive_sha256=$7
expected_version=$8

# shellcheck source=/dev/null
source "$remote_dir/bin/deploy-common.sh"
archive_sha256=$(deploy_file_sha256 "$remote_archive")
if [[ "$archive_sha256" != "$expected_archive_sha256" ]]; then
  echo "Transferred archive mismatch: expected=$expected_archive_sha256 actual=$archive_sha256" >&2
  exit 1
fi
echo "Verified image archive: $archive_sha256"

gunzip -c "$remote_archive" | docker load
docker tag "$build_image" "$remote_image"
rm -f "$remote_archive"
loaded_image_id=$(docker image inspect "$remote_image" --format '{{.Id}}')
loaded_version=$(docker image inspect "$remote_image" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')
if [[ "$loaded_version" != "$expected_version" ]]; then
  echo "Loaded image version mismatch: expected=$expected_version actual=${loaded_version:-unavailable}" >&2
  exit 1
fi
echo "Loaded image: ${loaded_image_id#sha256:} version=$loaded_version"

cd "$remote_dir"
export NEW_API_IMAGE=$remote_image
compose=(docker compose --env-file .env -f docker-compose.yml -f docker-compose.deploy.yml -f "$target_compose")
"${compose[@]}" up -d --no-build redis postgres
"${compose[@]}" up -d --no-build --force-recreate new-api
if [[ -n "$extra_services" ]]; then
  read -r -a services <<<"$extra_services"
  "${compose[@]}" up -d --no-build --force-recreate "${services[@]}"
fi

container_image_id=$(docker inspect -f '{{.Image}}' new-api)
if [[ "$container_image_id" != "$loaded_image_id" ]]; then
  echo "new-api container image mismatch: expected=$loaded_image_id actual=$container_image_id" >&2
  exit 1
fi
echo "Deployed image: ${container_image_id#sha256:}"
deploy_prune_project_images
REMOTE_DEPLOY

deploy_log "Waiting for container health"
ssh_bash <<'REMOTE_HEALTH'
set -Eeuo pipefail
for ((attempt = 1; attempt <= 45; attempt++)); do
  health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' new-api 2>/dev/null || true)
  if [[ "$health" == "healthy" ]]; then
    docker ps --filter name='^/new-api$' --format 'new-api: {{.Status}} ({{.Image}})'
    exit 0
  fi
  if [[ "$health" == "unhealthy" || "$health" == "exited" || "$health" == "dead" ]]; then
    docker logs --tail 100 new-api >&2
    exit 1
  fi
  sleep 2
done
docker logs --tail 100 new-api >&2
echo "Timed out waiting for new-api to become healthy" >&2
exit 1
REMOTE_HEALTH

deploy_log "Verifying endpoint: $HEALTH_URL"
version=""
for ((attempt = 1; attempt <= 30; attempt++)); do
  if status_json=$(curl --fail --silent --show-error --max-time 5 "$HEALTH_URL" 2>/dev/null); then
    version=$(printf '%s' "$status_json" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
    [[ "$version" == "$APP_VERSION" ]] && break
  fi
  sleep 2
done
[[ "$version" == "$APP_VERSION" ]] || deploy_die "Running version mismatch: expected=$APP_VERSION actual=${version:-unavailable}"
deploy_log "Deployment completed: version=${version:-unavailable} image=${LOCAL_IMAGE_ID#sha256:}"
