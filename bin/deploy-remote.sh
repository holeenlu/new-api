#!/usr/bin/env bash

set -Eeuo pipefail

if (($# != 12)); then
  echo 'usage: deploy-remote.sh REMOTE_DIR COMPOSE_FILE PROXY_SERVICE ARCHIVE BUILD_IMAGE TARGET_IMAGE ROLLBACK_IMAGE SHA256 VERSION STATE_FILE BACKUP_ENABLED DEPLOY_NAME' >&2
  exit 2
fi

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
deploy_name=${12}

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
  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)
    [[ "$health" == healthy ]] && return 0
    [[ "$health" != unhealthy && "$health" != exited && "$health" != dead ]] || return 1
    sleep 2
  done
  return 1
}

wait_status() {
  local state
  local attempt
  for ((attempt = 1; attempt <= 45; attempt++)); do
    docker exec new-api wget -q -O - http://localhost:3000/api/status >/dev/null 2>&1 && return 0
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
  docker tag "$rollback_image" "$target_image"
  "${compose[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans new-api
  wait_status
  reload_proxy
}

finish() {
  local status=$?
  trap - EXIT
  rm -f "$archive"
  if ((status != 0)) && [[ "$switched" == true ]]; then
    rollback || echo "[deploy] Warning: $deploy_name rollback failed" >&2
  fi
  exit "$status"
}
trap finish EXIT

[[ "$(file_sha256 "$archive")" == "$expected_sha" ]] || {
  echo 'Transferred image checksum mismatch' >&2
  exit 1
}
"${compose[@]}" config -q
for dependency in redis postgres; do
  "${compose[@]}" up -d --no-build "$dependency"
  wait_healthy "$dependency" 30
done

if [[ "$backup_enabled" == true || "$backup_enabled" == 1 ]]; then
  mkdir -p backups
  backup="backups/predeploy-$(date -u +%Y%m%dT%H%M%SZ).sql.gz"
  docker exec postgres sh -c 'pg_dump -U "$POSTGRES_USER" "$POSTGRES_DB"' | gzip >"$backup"
  chmod 600 "$backup"
  shopt -s nullglob
  backups=(backups/predeploy-*.sql.gz)
  while ((${#backups[@]} > 3)); do
    rm -f "${backups[0]}"
    backups=("${backups[@]:1}")
  done
  shopt -u nullglob
fi

if [[ -n "$("${compose[@]}" ps -q "$proxy_service")" ]]; then
  "${compose[@]}" exec -T "$proxy_service" caddy validate --config /etc/caddy/Caddyfile </dev/null >/dev/null
else
  "${compose[@]}" run --rm --no-deps -T --entrypoint caddy "$proxy_service" validate --config /etc/caddy/Caddyfile </dev/null >/dev/null
fi

if docker inspect new-api >/dev/null 2>&1; then
  previous_image=$(docker inspect -f '{{.Image}}' new-api)
  if docker image inspect "$previous_image" >/dev/null 2>&1; then
    docker tag "$previous_image" "$rollback_image"
    rollback_available=true
  fi
fi

gunzip -c "$archive" | docker load >/dev/null
loaded_version=$(docker image inspect "$build_image" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')
[[ "$loaded_version" == "$expected_version" ]] || {
  echo 'Loaded image version mismatch' >&2
  exit 1
}
docker tag "$build_image" "$target_image"
switched=true
"${compose[@]}" up -d --no-build --no-deps --force-recreate --remove-orphans new-api
wait_healthy new-api || {
  docker logs --tail 120 new-api >&2 || true
  exit 1
}
reload_proxy

expected_image=$(docker image inspect "$target_image" --format '{{.Id}}')
running_image=$(docker inspect -f '{{.Image}}' new-api)
[[ "$expected_image" == "$running_image" ]] || {
  echo 'Running image mismatch' >&2
  exit 1
}

status_json=$(docker exec new-api wget -q -O - http://localhost:3000/api/status)
start_time=$(printf '%s' "$status_json" | sed -n 's/.*"start_time":\([0-9][0-9]*\).*/\1/p')
version=$(printf '%s' "$status_json" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
[[ "$version" == "$expected_version" && "$start_time" =~ ^[0-9]+$ ]] || {
  echo 'Running process identity mismatch' >&2
  exit 1
}
printf 'ARCHIVE_SHA256=%s\nAPP_VERSION=%s\nSTART_TIME=%s\n' "$expected_sha" "$version" "$start_time" >"$state_file"
chmod 600 "$state_file"
switched=false
