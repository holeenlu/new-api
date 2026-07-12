#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DEPLOY_TARGET=${DEPLOY_TARGET:-root@104.128.92.169}
REMOTE_DIR=${REMOTE_DIR:-/opt/newapi-proxy}
SERVER_COMPOSE=${SERVER_COMPOSE:-docker-compose.server-104.128.92.169.yml}
BUILD_IMAGE=${BUILD_IMAGE:-new-api:deploy-amd64}
REMOTE_IMAGE=${REMOTE_IMAGE:-new-api:oauth-local}
HEALTH_URL=${HEALTH_URL:-http://104.128.92.169/api/status}
RELEASE_VERSION=${RELEASE_VERSION:-$(git -C "$ROOT_DIR" describe --tags --abbrev=0 2>/dev/null || sed -n '1p' "$ROOT_DIR/VERSION")}

if [[ -z "$RELEASE_VERSION" ]]; then
  echo "Unable to determine the latest Git release version" >&2
  exit 1
fi

CONTROL_PATH="${TMPDIR:-/tmp}/new-api-deploy-ssh-${UID}-$$"
IMAGE_ARCHIVE=$(mktemp "${TMPDIR:-/tmp}/new-api-amd64.XXXXXX")
REMOTE_ARCHIVE="/tmp/$(basename "$IMAGE_ARCHIVE")"
SSH_OPTIONS=(
  -o StrictHostKeyChecking=accept-new
  -o ControlMaster=auto
  -o ControlPersist=10m
  -o ControlPath="$CONTROL_PATH"
)

cleanup() {
  rm -f "$IMAGE_ARCHIVE"
  ssh "${SSH_OPTIONS[@]}" -O exit "$DEPLOY_TARGET" >/dev/null 2>&1 || true
  rm -f "$CONTROL_PATH"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for command in docker ssh scp tar gzip curl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Missing required command: $command" >&2
    exit 1
  fi
done

if [[ ! -f "$ROOT_DIR/docker-compose.yml" || ! -f "$ROOT_DIR/$SERVER_COMPOSE" ]]; then
  echo "Compose files are missing under $ROOT_DIR" >&2
  exit 1
fi

echo "[1/7] Opening SSH connection to $DEPLOY_TARGET"
ssh "${SSH_OPTIONS[@]}" -MNf "$DEPLOY_TARGET"

echo "[2/7] Building linux/amd64 image: $BUILD_IMAGE"
docker buildx build \
  --progress=plain \
  --platform linux/amd64 \
  --load \
  --build-arg "APP_VERSION=$RELEASE_VERSION" \
  --tag "$BUILD_IMAGE" \
  "$ROOT_DIR"

architecture=$(docker image inspect "$BUILD_IMAGE" --format '{{.Architecture}}')
operating_system=$(docker image inspect "$BUILD_IMAGE" --format '{{.Os}}')
if [[ "$architecture" != "amd64" || "$operating_system" != "linux" ]]; then
  echo "Unexpected image platform: $operating_system/$architecture" >&2
  exit 1
fi

echo "[3/7] Synchronizing source without server data or secrets"
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
  | ssh "${SSH_OPTIONS[@]}" "$DEPLOY_TARGET" \
      "mkdir -p '$REMOTE_DIR' && cd '$REMOTE_DIR' && tar -xzf - && rm -f docker-compose.override.yml"

echo "[4/7] Saving and transferring image"
docker save "$BUILD_IMAGE" | gzip > "$IMAGE_ARCHIVE"
scp "${SSH_OPTIONS[@]}" "$IMAGE_ARCHIVE" "$DEPLOY_TARGET:$REMOTE_ARCHIVE"

echo "[5/7] Loading image and recreating application container"
ssh "${SSH_OPTIONS[@]}" "$DEPLOY_TARGET" bash -s -- \
  "$REMOTE_DIR" "$REMOTE_ARCHIVE" "$BUILD_IMAGE" "$REMOTE_IMAGE" "$SERVER_COMPOSE" <<'REMOTE_SCRIPT'
set -Eeuo pipefail

remote_dir=$1
remote_archive=$2
build_image=$3
remote_image=$4
server_compose=$5

gunzip -c "$remote_archive" | docker load
docker tag "$build_image" "$remote_image"
rm -f "$remote_archive"

cd "$remote_dir"
docker compose \
  -f docker-compose.yml \
  -f "$server_compose" \
  up -d --no-build --force-recreate new-api
REMOTE_SCRIPT

echo "[6/7] Waiting for container health"
ssh "${SSH_OPTIONS[@]}" "$DEPLOY_TARGET" bash -s <<'REMOTE_SCRIPT'
set -Eeuo pipefail

for _ in $(seq 1 45); do
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
REMOTE_SCRIPT

echo "[7/7] Verifying public endpoint: $HEALTH_URL"
curl --fail --silent --show-error --max-time 20 "$HEALTH_URL" >/dev/null

version=$(curl --fail --silent --show-error --max-time 20 "$HEALTH_URL" \
  | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
echo "Deployment completed: ${version:-version unavailable}"
