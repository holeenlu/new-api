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
SOURCE_ARCHIVE=$(mktemp "${TMPDIR:-/tmp}/new-api-source.XXXXXX")
SOURCE_MANIFEST=$(mktemp "${TMPDIR:-/tmp}/new-api-manifest.XXXXXX")
REMOTE_ARCHIVE="/tmp/$(basename "$IMAGE_ARCHIVE")"
REMOTE_SOURCE_ARCHIVE="/tmp/$(basename "$SOURCE_ARCHIVE")"
REMOTE_SOURCE_MANIFEST="/tmp/$(basename "$SOURCE_MANIFEST")"
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
  rm -f "$IMAGE_ARCHIVE" "$SOURCE_ARCHIVE" "$SOURCE_MANIFEST" "$ASKPASS_SCRIPT"
  if [[ "$SSH_MASTER_ACTIVE" == true ]]; then
    ssh_remote "$DEPLOY_TARGET" \
      "rm -f '$REMOTE_ARCHIVE' '$REMOTE_SOURCE_ARCHIVE' '$REMOTE_SOURCE_MANIFEST'" >/dev/null 2>&1 || true
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
deploy_require_commands docker ssh scp tar gzip curl git date awk sort
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
  --exclude='./.new-api-deploy-manifest' \
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
  -czf "$SOURCE_ARCHIVE" -C "$ROOT_DIR" .
tar -tzf "$SOURCE_ARCHIVE" \
  | awk 'substr($0, length($0), 1) != "/" { sub(/^\.\//, ""); if (length($0) > 0) print }' \
  | LC_ALL=C sort -u >"$SOURCE_MANIFEST"
scp_remote "$SOURCE_ARCHIVE" "$DEPLOY_TARGET:$REMOTE_SOURCE_ARCHIVE"
scp_remote "$SOURCE_MANIFEST" "$DEPLOY_TARGET:$REMOTE_SOURCE_MANIFEST"
ssh_bash "$REMOTE_DIR" "$REMOTE_SOURCE_ARCHIVE" "$REMOTE_SOURCE_MANIFEST" <<'REMOTE_SYNC'
set -Eeuo pipefail

remote_dir=$1
source_archive=$2
next_manifest=$3
deployed_manifest="$remote_dir/.new-api-deploy-manifest"

mkdir -p "$remote_dir"
cd "$remote_dir"

validate_deploy_path() {
  local path=$1
  if [[ -z "$path" || "$path" == /* || "$path" == "." ]]; then
    echo "Unsafe deployment manifest path: $path" >&2
    return 1
  fi
  case "/$path/" in
    *"/../"*|*"/./"*)
      echo "Unsafe deployment manifest path: $path" >&2
      return 1
      ;;
  esac
}

is_persistent_deploy_path() {
  case "$1" in
    .env|data|data/*|logs|logs/*|backups|backups/*|caddy|caddy/*)
      return 0
      ;;
  esac
  return 1
}

while IFS= read -r path || [[ -n "$path" ]]; do
  validate_deploy_path "$path"
  if is_persistent_deploy_path "$path"; then
    echo "Persistent path unexpectedly present in deployment manifest: $path" >&2
    exit 1
  fi
done <"$next_manifest"

if [[ -f "$deployed_manifest" ]]; then
  while IFS= read -r path || [[ -n "$path" ]]; do
    validate_deploy_path "$path"
  done <"$deployed_manifest"
fi

stale_manifest=$(mktemp "$remote_dir/.new-api-stale.XXXXXX")
trap 'rm -f "$stale_manifest"' EXIT
if [[ -f "$deployed_manifest" ]]; then
  awk 'NR == FNR { current[$0] = 1; next } !($0 in current)' \
    "$next_manifest" "$deployed_manifest" >"$stale_manifest"
fi

declare -a stale_directories=()
while IFS= read -r path || [[ -n "$path" ]]; do
  if is_persistent_deploy_path "$path"; then
    continue
  fi
  if [[ ! -d "$path" || -L "$path" ]]; then
    rm -f -- "$path"
  fi
  parent=${path%/*}
  while [[ "$parent" != "$path" && -n "$parent" && "$parent" != "." ]]; do
    if ! is_persistent_deploy_path "$parent"; then
      stale_directories+=("$parent")
    fi
    path=$parent
    parent=${path%/*}
  done
done <"$stale_manifest"

# These files predate the managed manifest and are no longer valid deployment inputs.
for path in \
  bin/deploy-104.128.92.169.sh \
  deploy-server-104.128.92.169.sh \
  docker-compose.server-104.128.92.169.yml \
  Caddyfile.104.128.92.169 \
  docker-compose.override.yml; do
  rm -f -- "$path"
done

while IFS= read -r path || [[ -n "$path" ]]; do
  if [[ -d "$path" && ! -L "$path" ]]; then
    rmdir -- "$path" 2>/dev/null || {
      echo "Cannot replace non-empty directory with deployed file: $path" >&2
      exit 1
    }
  fi
done <"$next_manifest"

tar -xzf "$source_archive"
manifest_temp="$deployed_manifest.tmp.$$"
cp "$next_manifest" "$manifest_temp"
mv -f "$manifest_temp" "$deployed_manifest"
rm -f "$source_archive" "$next_manifest"

for ((pass = 0; pass < 64 && ${#stale_directories[@]} > 0; pass++)); do
  removed=false
  for path in "${stale_directories[@]}"; do
    if rmdir -- "$path" 2>/dev/null; then
      removed=true
    fi
  done
  [[ "$removed" == true ]] || break
done
REMOTE_SYNC

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

keep_caddy=false
if [[ -n "$extra_services" ]]; then
  read -r -a configured_services <<<"$extra_services"
  for service in "${configured_services[@]}"; do
    if [[ "$service" == "caddy" ]]; then
      keep_caddy=true
      break
    fi
  done
fi
if [[ "$keep_caddy" == false ]] && docker inspect new-api-caddy >/dev/null 2>&1; then
  caddy_service=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.service" }}' new-api-caddy)
  caddy_working_dir=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' new-api-caddy)
  canonical_remote_dir=$(pwd -P)
  if [[ "$caddy_service" == "caddy" && ( "$caddy_working_dir" == "$remote_dir" || "$caddy_working_dir" == "$canonical_remote_dir" ) ]]; then
    docker rm -f new-api-caddy
  else
    echo "Preserving new-api-caddy because its Compose labels do not match this deployment" >&2
  fi
fi

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
