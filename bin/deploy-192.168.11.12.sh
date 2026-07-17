#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT_DIR/bin/deploy-common.sh"

DEPLOY_TARGET=kdan@192.168.11.12
DEPLOY_SSH_KEY=${DEPLOY_SSH_KEY:-$HOME/.ssh/new-api-192-deploy}
REMOTE_DIR=/home/kdan/newapi-proxy
COMPOSE_FILE=docker-compose.server-192.168.11.12.yml
CADDY_FILE=Caddyfile.192.168.11.12
PROXY_SERVICE=gateway
TARGET_IMAGE=new-api:release-192
ROLLBACK_IMAGE=new-api:rollback-192
HEALTH_URL=http://192.168.11.12:3000/api/status

BUILD_IMAGE=new-api:build-192-amd64
NO_CACHE=${NO_CACHE:-false}
DEPLOY_BUILD_ATTEMPTS=${DEPLOY_BUILD_ATTEMPTS:-2}
GOPROXY=${GOPROXY:-https://goproxy.cn,direct}
GOPROXY_FALLBACK=${GOPROXY_FALLBACK:-https://proxy.golang.org,direct}
LOCAL_ARCHIVE=$(mktemp "${TMPDIR:-/tmp}/new-api-release-192.XXXXXX")
REMOTE_ARCHIVE=/tmp/new-api-release-192-$$.tar.gz
REMOTE_LOCK=$REMOTE_DIR/.deploy-lock-192
REMOTE_STATE=$REMOTE_DIR/.deploy-state-192.env
CONTROL_PATH=/tmp/new-api-192-ssh-${UID}-$$
SSH_MASTER_ACTIVE=false
[[ -r "$DEPLOY_SSH_KEY" ]] || deploy_die "Missing SSH deployment key: $DEPLOY_SSH_KEY; run bin/setup-ssh-key-192.168.11.12.sh once"
SSH_OPTIONS=(
  -i "$DEPLOY_SSH_KEY"
  -o IdentitiesOnly=yes
  -o BatchMode=yes
  -o StrictHostKeyChecking=accept-new
  -o ConnectTimeout=15
  -o ControlMaster=auto
  -o ControlPersist=5m
  -o ControlPath="$CONTROL_PATH"
)

ssh_remote() {
  command ssh "${SSH_OPTIONS[@]}" "$@"
}

scp_remote() {
  command scp "${SSH_OPTIONS[@]}" "$@"
}

cleanup() {
  if [[ "$SSH_MASTER_ACTIVE" == true ]]; then
    ssh_remote "$DEPLOY_TARGET" "rm -f '$REMOTE_ARCHIVE'; rmdir '$REMOTE_LOCK' 2>/dev/null || true" >/dev/null 2>&1 || true
    ssh_remote -O exit "$DEPLOY_TARGET" >/dev/null 2>&1 || true
  fi
  rm -f "$CONTROL_PATH" "$LOCAL_ARCHIVE"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

deploy_ensure_docker_cli
deploy_require_commands docker ssh scp gzip curl git sed awk tail
docker info >/dev/null 2>&1 || deploy_die "Local Docker daemon is unavailable"
docker buildx version >/dev/null 2>&1 || deploy_die "Local docker buildx is unavailable"
for file in "$ROOT_DIR/$COMPOSE_FILE" "$ROOT_DIR/$CADDY_FILE"; do
  [[ -f "$file" ]] || deploy_die "Missing deployment input: $file"
done

APP_VERSION=$(deploy_build_version "$ROOT_DIR")
deploy_build_image "$ROOT_DIR" "$BUILD_IMAGE" linux/amd64 "$APP_VERSION" "$GOPROXY" "$GOPROXY_FALLBACK" "$NO_CACHE" \
  || deploy_die "192 local source build failed"
deploy_assert_image_platform "$BUILD_IMAGE" linux/amd64
deploy_assert_image_runs "$BUILD_IMAGE" "$APP_VERSION" linux/amd64
docker save "$BUILD_IMAGE" | gzip >"$LOCAL_ARCHIVE"
[[ -s "$LOCAL_ARCHIVE" ]] || deploy_die "192 image archive is empty"
ARCHIVE_SHA256=$(deploy_file_sha256 "$LOCAL_ARCHIVE")

deploy_log "Connecting to 192.168.11.12"
ssh_remote -MNf "$DEPLOY_TARGET"
SSH_MASTER_ACTIVE=true
ssh_remote "$DEPLOY_TARGET" "docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1" \
  || deploy_die "Remote Docker is unavailable"
ssh_remote "$DEPLOY_TARGET" "mkdir -p '$REMOTE_DIR' && test -f '$REMOTE_DIR/.env' && { mkdir '$REMOTE_LOCK' 2>/dev/null || { find '$REMOTE_LOCK' -maxdepth 0 -mmin +60 -print -quit | grep -q . && rmdir '$REMOTE_LOCK' && mkdir '$REMOTE_LOCK'; }; }" \
  || deploy_die "Remote .env is missing or another 192 deployment is active"

deploy_log "Uploading 192 configuration and image"
scp_remote "$ROOT_DIR/$COMPOSE_FILE" "$DEPLOY_TARGET:$REMOTE_DIR/$COMPOSE_FILE"
scp_remote "$ROOT_DIR/$CADDY_FILE" "$DEPLOY_TARGET:$REMOTE_DIR/$CADDY_FILE"
scp_remote "$LOCAL_ARCHIVE" "$DEPLOY_TARGET:$REMOTE_ARCHIVE"

deploy_log "Deploying 192 application"
ssh_remote "$DEPLOY_TARGET" "bash -s -- '$REMOTE_DIR' '$COMPOSE_FILE' '$PROXY_SERVICE' '$REMOTE_ARCHIVE' '$BUILD_IMAGE' '$TARGET_IMAGE' '$ROLLBACK_IMAGE' '$ARCHIVE_SHA256' '$APP_VERSION' '$REMOTE_STATE' '${DEPLOY_DATABASE_BACKUP_ENABLED:-true}'" <<'REMOTE_DEPLOY'
set -Eeuo pipefail

remote_dir=$1
compose_file=$2
proxy_service=$3
archive=$4
build_image=$5
target_image=$6
rollback_image=$7
expected_sha=$8
expected_version=$9
state_file=${10}
backup_enabled=${11}

cd "$remote_dir"
compose=(docker compose --env-file .env -f "$compose_file")
switched=false
rollback_available=false

file_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

wait_healthy() {
  local container=$1
  local attempts=${2:-45}
  local health
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)
    [[ "$health" == healthy ]] && return 0
    [[ "$health" != unhealthy && "$health" != exited && "$health" != dead ]] || return 1
    sleep 2
  done
  return 1
}

wait_status() {
  for ((attempt = 1; attempt <= 45; attempt++)); do
    if docker exec new-api wget -q -O - http://localhost:3000/api/status >/dev/null 2>&1; then
      return 0
    fi
    state=$(docker inspect -f '{{.State.Status}}' new-api 2>/dev/null || true)
    [[ "$state" != exited && "$state" != dead ]] || return 1
    sleep 2
  done
  return 1
}

reload_proxy() {
  "${compose[@]}" up -d --no-build --no-deps "$proxy_service" >/dev/null
  "${compose[@]}" exec -T "$proxy_service" caddy reload --config /etc/caddy/Caddyfile </dev/null >/dev/null
}

rollback() {
  [[ "$rollback_available" == true ]] || return 1
  echo "[deploy] Restoring previous 192 image" >&2
  docker tag "$rollback_image" "$target_image"
  "${compose[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans new-api
  wait_status
  reload_proxy
}

finish() {
  status=$?
  trap - EXIT
  rm -f "$archive"
  if ((status != 0)) && [[ "$switched" == true ]]; then
    rollback || echo "[deploy] Warning: 192 rollback failed" >&2
  fi
  exit "$status"
}
trap finish EXIT

[[ "$(file_sha256 "$archive")" == "$expected_sha" ]] || { echo "Transferred image checksum mismatch" >&2; exit 1; }
"${compose[@]}" config -q
rm -f docker-compose.yml docker-compose.deploy.yml bin/deploy-common.sh bin/deploy-remote.sh
rmdir bin 2>/dev/null || true
for dependency in redis postgres; do
  if ! docker inspect "$dependency" >/dev/null 2>&1; then
    "${compose[@]}" up -d --no-build "$dependency"
  fi
  wait_healthy "$dependency" 30
done

if [[ "$backup_enabled" == true || "$backup_enabled" == 1 ]]; then
  mkdir -p backups
  backup="backups/predeploy-$(date -u +%Y%m%dT%H%M%SZ).sql.gz"
  docker exec postgres sh -c 'pg_dump -U "$POSTGRES_USER" "$POSTGRES_DB"' | gzip >"$backup"
  chmod 600 "$backup"
  mapfile -t backups < <(find backups -maxdepth 1 -name 'predeploy-*.sql.gz' -type f | sort)
  while ((${#backups[@]} > 3)); do
    rm -f "${backups[0]}"
    backups=("${backups[@]:1}")
  done
  echo "[deploy] Database backup created: $remote_dir/$backup"
fi

if [[ -n "$("${compose[@]}" ps -q "$proxy_service")" ]]; then
  "${compose[@]}" exec -T "$proxy_service" caddy validate --config /etc/caddy/Caddyfile </dev/null >/dev/null
else
  "${compose[@]}" run --rm --no-deps -T --entrypoint caddy "$proxy_service" \
    validate --config /etc/caddy/Caddyfile </dev/null >/dev/null
fi

if docker inspect new-api >/dev/null 2>&1; then
  docker tag "$(docker inspect -f '{{.Image}}' new-api)" "$rollback_image"
  rollback_available=true
fi

gunzip -c "$archive" | docker load >/dev/null
loaded_version=$(docker image inspect "$build_image" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')
[[ "$loaded_version" == "$expected_version" ]] || { echo "Loaded image version mismatch" >&2; exit 1; }
docker tag "$build_image" "$target_image"
switched=true
"${compose[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans new-api
wait_healthy new-api || { docker logs --tail 120 new-api >&2 || true; exit 1; }
reload_proxy

expected_image=$(docker image inspect "$target_image" --format '{{.Id}}')
running_image=$(docker inspect -f '{{.Image}}' new-api)
[[ "$expected_image" == "$running_image" ]] || { echo "Running image mismatch" >&2; exit 1; }
status_json=$(docker exec new-api wget -q -O - http://localhost:3000/api/status)
start_time=$(printf '%s' "$status_json" | sed -n 's/.*"start_time":\([0-9][0-9]*\).*/\1/p')
version=$(printf '%s' "$status_json" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
[[ "$version" == "$expected_version" && "$start_time" =~ ^[0-9]+$ ]] || { echo "Running process identity mismatch" >&2; exit 1; }
printf 'ARCHIVE_SHA256=%s\nAPP_VERSION=%s\nSTART_TIME=%s\n' "$expected_sha" "$version" "$start_time" >"$state_file"
chmod 600 "$state_file"
switched=false
echo "[deploy] 192 container ready: version=$version start_time=$start_time"
REMOTE_DEPLOY
REMOTE_START_TIME=$(ssh_remote "$DEPLOY_TARGET" "sed -n 's/^START_TIME=//p' '$REMOTE_STATE'")
deploy_log "Verifying 192 public endpoint"
version=""
start_time=""
for ((attempt = 1; attempt <= 30; attempt++)); do
  if status_json=$(curl --noproxy '*' --fail --silent --show-error --max-time 5 \
    --header 'Cache-Control: no-cache' "$HEALTH_URL?deploy_check=$REMOTE_START_TIME" 2>/dev/null); then
    version=$(printf '%s' "$status_json" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
    start_time=$(printf '%s' "$status_json" | sed -n 's/.*"start_time":\([0-9][0-9]*\).*/\1/p')
    [[ "$version" == "$APP_VERSION" && "$start_time" == "$REMOTE_START_TIME" ]] && break
  fi
  sleep 2
done
if [[ "$version" != "$APP_VERSION" || "$start_time" != "$REMOTE_START_TIME" ]] || \
  ! deploy_verify_relay_routes "${HEALTH_URL%/api/status}"; then
  deploy_log "192 public verification failed; restoring rollback image"
  ssh_remote "$DEPLOY_TARGET" "bash -s -- '$REMOTE_DIR' '$COMPOSE_FILE' '$PROXY_SERVICE' '$TARGET_IMAGE' '$ROLLBACK_IMAGE'" <<'REMOTE_ROLLBACK'
set -Eeuo pipefail
cd "$1"
compose=(docker compose --env-file .env -f "$2")
docker image inspect "$5" >/dev/null 2>&1 || { echo "Rollback image is unavailable: $5" >&2; exit 1; }
docker tag "$5" "$4"
"${compose[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans new-api
restored=false
for ((attempt = 1; attempt <= 45; attempt++)); do
  if docker exec new-api wget -q -O - http://localhost:3000/api/status >/dev/null 2>&1; then
    restored=true
    break
  fi
  sleep 2
done
[[ "$restored" == true ]] || { docker logs --tail 120 new-api >&2 || true; exit 1; }
"${compose[@]}" up -d --no-build --no-deps "$3" >/dev/null
"${compose[@]}" exec -T "$3" caddy reload --config /etc/caddy/Caddyfile </dev/null >/dev/null
REMOTE_ROLLBACK
  deploy_die "192 deployment verification failed"
fi

deploy_log "192 deployment completed: version=$APP_VERSION start_time=$REMOTE_START_TIME"
