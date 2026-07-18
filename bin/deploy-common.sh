#!/usr/bin/env bash

if [[ -n "${NEW_API_DEPLOY_COMMON_LOADED:-}" ]]; then
  return 0
fi
readonly NEW_API_DEPLOY_COMMON_LOADED=1
readonly DEPLOY_DEFAULT_GOPROXY=https://goproxy.cn,direct
readonly DEPLOY_DEFAULT_GOPROXY_FALLBACK=https://proxy.golang.org,direct

deploy_log() {
  printf '[deploy] %s\n' "$*"
}

deploy_die() {
  printf '[error] %s\n' "$*" >&2
  exit 1
}

deploy_flag_enabled() {
  local name=$1
  local default_value=${2:-false}
  local value=${!name-}
  [[ -n "$value" ]] || value=$default_value
  case "$value" in
    true | 1) return 0 ;;
    false | 0) return 1 ;;
    *) deploy_die "$name must be true, false, 1, or 0" ;;
  esac
}

deploy_ensure_docker_cli() {
  if command -v docker >/dev/null 2>&1; then
    return
  fi
  local docker_desktop_bin=/Applications/Docker.app/Contents/Resources/bin
  if [[ -x "$docker_desktop_bin/docker" ]]; then
    export PATH="$docker_desktop_bin:$PATH"
  fi
}

deploy_require_commands() {
  local command
  for command in "$@"; do
    command -v "$command" >/dev/null 2>&1 || deploy_die "Missing required command: $command"
  done
}

deploy_build_version() {
  local root_dir=$1
  local release_version
  release_version=$(git -C "$root_dir" describe --tags --abbrev=0 2>/dev/null || sed -n '1p' "$root_dir/VERSION")
  [[ -n "$release_version" ]] || deploy_die "Unable to determine the release version"
  printf '%s\n' "$release_version"
}

deploy_file_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
    return
  fi
  shasum -a 256 "$1" | awk '{print $1}'
}

deploy_build_image() {
  local root_dir=$1
  local image=$2
  local platform=$3
  local app_version=$4
  local goproxy=$5
  local goproxy_fallback=$6
  local no_cache=$7
  local args=(
    docker buildx build
    --progress=plain
    --load
    --build-arg "APP_VERSION=$app_version"
    --build-arg "GOPROXY=$goproxy"
    --build-arg "GOPROXY_FALLBACK=$goproxy_fallback"
    --tag "$image"
  )
  if [[ -n "$platform" ]]; then
    args+=(--platform "$platform")
  fi
  if [[ "$no_cache" == "true" || "$no_cache" == "1" ]]; then
    args+=(--no-cache)
  fi

  local max_attempts=${DEPLOY_BUILD_ATTEMPTS:-2}
  [[ "$max_attempts" =~ ^[1-9][0-9]*$ ]] || deploy_die "DEPLOY_BUILD_ATTEMPTS must be a positive integer"
  local build_log
  build_log=$(mktemp "${TMPDIR:-/tmp}/new-api-build.XXXXXX")

  local attempt
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    : >"$build_log"
    deploy_log "Building image=$image platform=${platform:-native} version=$app_version attempt=$attempt/$max_attempts"
    if "${args[@]}" "$root_dir" 2>&1 | tee "$build_log"; then
      rm -f "$build_log"
      deploy_prune_build_cache
      return 0
    fi

    if ((attempt >= max_attempts)) || ! grep -Eiq \
      'failed to resolve source metadata|failed to do request|unexpected EOF|(^|[[:space:]:])EOF$|TLS handshake timeout|connection reset by peer|i/o timeout|temporary failure|429 Too Many Requests|503 Service Unavailable' \
      "$build_log"; then
      rm -f "$build_log"
      return 1
    fi

    local retry_delay=$((attempt * 5))
    deploy_log "Transient registry or network failure detected; retrying in ${retry_delay}s"
    sleep "$retry_delay"
  done

  rm -f "$build_log"
  return 1
}

deploy_prune_build_cache() {
  local mode=${DEPLOY_PRUNE_BUILD_CACHE:-true}
  if [[ "$mode" == "false" || "$mode" == "0" ]]; then
    return 0
  fi

  if [[ "$mode" == "all" ]]; then
    deploy_log "Pruning all Buildx cache"
    docker buildx prune --all --force || deploy_log "Warning: Buildx cache cleanup failed"
    return 0
  fi
  [[ "$mode" == "true" || "$mode" == "1" ]] || deploy_die "DEPLOY_PRUNE_BUILD_CACHE must be false, true, or all"
  local keep_storage=${DEPLOY_BUILD_CACHE_KEEP_STORAGE:-20GB}
  local unused_for=${DEPLOY_BUILD_CACHE_UNUSED_FOR:-24h}
  deploy_log "Pruning Buildx cache unused for ${unused_for}; keeping at most ${keep_storage}"
  if ! docker buildx prune --force --filter "until=${unused_for}" --keep-storage "$keep_storage"; then
    deploy_log "Warning: Buildx cache cleanup failed; deployment will continue"
  fi
}

deploy_assert_image_platform() {
  local image=$1
  local platform=$2
  [[ -n "$platform" ]] || return 0

  local expected_os=${platform%%/*}
  local expected_arch=${platform#*/}
  expected_arch=${expected_arch%%/*}
  local actual_os
  local actual_arch
  actual_os=$(docker image inspect "$image" --format '{{.Os}}')
  actual_arch=$(docker image inspect "$image" --format '{{.Architecture}}')
  [[ "$actual_os" == "$expected_os" && "$actual_arch" == "$expected_arch" ]] || \
    deploy_die "Unexpected image platform: expected=$expected_os/$expected_arch actual=$actual_os/$actual_arch"
}

deploy_image_id() {
  docker image inspect "$1" --format '{{.Id}}'
}

deploy_random_hex() {
  od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
}

deploy_assert_image_runs() {
  local image=$1
  local expected_version=$2
  local platform=${3:-}
  local actual_version
  local args=(docker run --rm)
  if [[ -n "$platform" ]]; then
    args+=(--platform "$platform")
  fi
  actual_version=$("${args[@]}" "$image" --version)
  [[ "$actual_version" == "$expected_version" ]] || \
    deploy_die "Image smoke test failed: expected_version=$expected_version actual_version=${actual_version:-unavailable}"
}

deploy_verify_relay_routes() {
  local base_url=${1%/}
  local path
  local status
  for path in /v1/responses /v1/chat/completions /v1/messages; do
    status=$(curl --noproxy '*' --silent --output /dev/null --write-out '%{http_code}' \
      --max-time 10 --header 'Content-Type: application/json' \
      --request POST --data '{}' "$base_url$path" || true)
    if [[ "$status" != "401" ]]; then
      printf '[error] Relay route probe failed: path=%s expected_status=401 actual_status=%s\n' \
        "$path" "${status:-unavailable}" >&2
      return 1
    fi
  done
}

deploy_backup_postgres() {
  local backup_dir=$1
  local backup_file="$backup_dir/predeploy-$(date -u +%Y%m%dT%H%M%SZ).sql.gz"
  local temp_file="${backup_file}.tmp.$$"
  mkdir -p "$backup_dir"
  chmod 700 "$backup_dir"
  if ! docker exec postgres sh -c 'pg_dump -U "$POSTGRES_USER" "$POSTGRES_DB"' | gzip >"$temp_file"; then
    rm -f "$temp_file"
    return 1
  fi
  [[ -s "$temp_file" ]] || {
    rm -f "$temp_file"
    return 1
  }
  chmod 600 "$temp_file"
  mv "$temp_file" "$backup_file"

  local backups=("$backup_dir"/predeploy-*.sql.gz)
  local remove_count=$((${#backups[@]} - 3))
  local index
  for ((index = 0; index < remove_count; index++)); do
    rm -f "${backups[$index]}"
  done
  deploy_log "Database backup created: $backup_file"
}

deploy_env_get() {
  local env_file=$1
  local key=$2
  sed -n "s/^[[:space:]]*${key}=//p" "$env_file" | tail -n 1
}

deploy_env_ensure() {
  local env_file=$1
  local key=$2
  local value=$3
  local existing
  existing=$(deploy_env_get "$env_file" "$key")
  if grep -Eq "^[[:space:]]*${key}=" "$env_file"; then
    [[ -n "$existing" ]] || deploy_die "$key is empty in $env_file"
    return 0
  fi
  printf '%s=%s\n' "$key" "$value" >>"$env_file"
}

deploy_env_migrate_default() {
  local env_file=$1
  local key=$2
  local old_default=$3
  local new_default=$4
  local existing
  existing=$(deploy_env_get "$env_file" "$key")

  if ! grep -Eq "^[[:space:]]*${key}=" "$env_file"; then
    printf '%s=%s\n' "$key" "$new_default" >>"$env_file"
    return
  fi
  [[ -n "$existing" ]] || deploy_die "$key is empty in $env_file"
  if [[ "$existing" != "$old_default" ]]; then
    return
  fi

  local migrated_file
  migrated_file=$(mktemp "${env_file}.XXXXXX")
  awk -v key="$key" -v value="$new_default" '
    $0 ~ "^[[:space:]]*" key "=" {
      if (!found) print key "=" value
      found = 1
      next
    }
    { print }
  ' "$env_file" >"$migrated_file"
  chmod 600 "$migrated_file"
  mv "$migrated_file" "$env_file"
  deploy_log "Migrated $key from $old_default to $new_default"
}

deploy_prepare_env_file() {
  local env_file=$1
  if [[ ! -f "$env_file" ]]; then
    umask 077
    : >"$env_file"
  fi
  chmod 600 "$env_file"

  deploy_env_ensure "$env_file" POSTGRES_USER root
  deploy_env_ensure "$env_file" POSTGRES_PASSWORD "$(deploy_random_hex)"
  deploy_env_ensure "$env_file" POSTGRES_DB new-api
  deploy_env_ensure "$env_file" REDIS_PASSWORD "$(deploy_random_hex)"
  deploy_env_ensure "$env_file" SESSION_SECRET "$(deploy_random_hex)"
  deploy_env_ensure "$env_file" CRYPTO_SECRET "$(deploy_random_hex)"

  local postgres_user
  local postgres_password
  local postgres_db
  local redis_password
  local timezone_env_file
  postgres_user=$(deploy_env_get "$env_file" POSTGRES_USER)
  postgres_password=$(deploy_env_get "$env_file" POSTGRES_PASSWORD)
  postgres_db=$(deploy_env_get "$env_file" POSTGRES_DB)
  redis_password=$(deploy_env_get "$env_file" REDIS_PASSWORD)
  deploy_env_ensure "$env_file" SQL_DSN "postgresql://${postgres_user}:${postgres_password}@postgres:5432/${postgres_db}"
  deploy_env_ensure "$env_file" REDIS_CONN_STRING "redis://:${redis_password}@redis:6379"

  # Subscription upstream timestamps and reset windows are expressed in UTC.
  timezone_env_file=$(mktemp "${env_file}.XXXXXX")
  awk '
    BEGIN { found = 0 }
    /^[[:space:]]*TZ=/ {
      if (!found) print "TZ=UTC"
      found = 1
      next
    }
    { print }
    END { if (!found) print "TZ=UTC" }
  ' "$env_file" >"$timezone_env_file"
  chmod 600 "$timezone_env_file"
  mv "$timezone_env_file" "$env_file"

  deploy_env_migrate_default "$env_file" CLAUDE_CODE_OAUTH_MAX_CONCURRENCY 5 10
  deploy_env_ensure "$env_file" CLAUDE_CODE_OAUTH_MIN_REQUEST_INTERVAL_MS 750
  deploy_env_migrate_default "$env_file" CODEX_OAUTH_MAX_CONCURRENCY 5 10
  deploy_env_ensure "$env_file" CODEX_OAUTH_MIN_REQUEST_INTERVAL_MS 750
  deploy_env_ensure "$env_file" MAX_REQUEST_BODY_MB 128
  deploy_env_migrate_default "$env_file" SUBSCRIPTION_OAUTH_RESPONSE_HEADER_TIMEOUT 120 30
  deploy_env_ensure "$env_file" CHANNEL_MANAGEMENT_REQUEST_TIMEOUT 30
  deploy_env_ensure "$env_file" CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED true
  deploy_env_ensure "$env_file" CODEX_OAUTH_CLIENT_ID app_EMoamEEZ73f0CkXaXp7hrann
  deploy_env_ensure "$env_file" CODEX_OAUTH_REDIRECT_URI http://localhost:1455/auth/callback
  deploy_env_ensure "$env_file" CODEX_OAUTH_SCOPE "openid profile email offline_access"
  deploy_env_ensure "$env_file" UPSTREAM_LOCATION_MODE strip
  deploy_env_ensure "$env_file" UPSTREAM_SYSTEM_PROXY_ENABLED false
  deploy_env_ensure "$env_file" UPSTREAM_LOCATION_DISCOVERY_ENABLED true
  deploy_env_ensure "$env_file" UPSTREAM_LOCATION_DISCOVERY_TIMEOUT 8
}

deploy_server_main() {
  : "${DEPLOY_NAME:?}" "${DEPLOY_SLUG:?}" "${DEPLOY_TARGET:?}" "${REMOTE_DIR:?}"
  : "${COMPOSE_FILE:?}" "${CADDY_FILE:?}" "${PROXY_SERVICE:?}" "${TARGET_IMAGE:?}"
  : "${ROLLBACK_IMAGE:?}" "${HEALTH_URL:?}" "${BUILD_IMAGE:?}"

  local no_cache=${NO_CACHE:-false}
  local goproxy=${GOPROXY:-https://goproxy.cn,direct}
  local goproxy_fallback=${GOPROXY_FALLBACK:-https://proxy.golang.org,direct}
  local local_archive remote_archive remote_lock remote_state control_path initial_env_file askpass_script
  local ssh_master_active=false
  local_archive=$(mktemp "${TMPDIR:-/tmp}/new-api-${DEPLOY_SLUG}.XXXXXX")
  remote_archive="/tmp/new-api-${DEPLOY_SLUG}-$$.tar.gz"
  remote_lock="$REMOTE_DIR/.deploy-lock-$DEPLOY_SLUG"
  remote_state="$REMOTE_DIR/.deploy-state-$DEPLOY_SLUG.env"
  control_path="/tmp/new-api-${DEPLOY_SLUG}-ssh-${UID}-$$"
  initial_env_file=""
  askpass_script=""

  SSH_OPTIONS=(
    -o StrictHostKeyChecking=accept-new
    -o ConnectTimeout=15
    -o ControlMaster=auto
    -o ControlPersist=5m
    -o ControlPath="$control_path"
  )
  if [[ -n "${DEPLOY_SSH_KEY:-}" ]]; then
    [[ -r "$DEPLOY_SSH_KEY" ]] || deploy_die "Missing SSH deployment key: $DEPLOY_SSH_KEY"
    SSH_OPTIONS=(-i "$DEPLOY_SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes "${SSH_OPTIONS[@]}")
  elif [[ -n "${SSHPASS:-}" ]]; then
    export SSH_DEPLOY_PASSWORD=$SSHPASS
    unset SSHPASS
    askpass_script=$(mktemp "${TMPDIR:-/tmp}/new-api-${DEPLOY_SLUG}-askpass.XXXXXX")
    chmod 700 "$askpass_script"
    printf '%s\n' '#!/bin/sh' 'printf "%s\n" "$SSH_DEPLOY_PASSWORD"' >"$askpass_script"
  fi

  ssh_remote() {
    if [[ -n "$askpass_script" ]]; then
      DISPLAY="${DISPLAY:-new-api-deploy}" SSH_ASKPASS="$askpass_script" SSH_ASKPASS_REQUIRE=force \
        command ssh "${SSH_OPTIONS[@]}" "$@"
      return
    fi
    command ssh "${SSH_OPTIONS[@]}" "$@"
  }
  scp_remote() {
    if [[ -n "$askpass_script" ]]; then
      DISPLAY="${DISPLAY:-new-api-deploy}" SSH_ASKPASS="$askpass_script" SSH_ASKPASS_REQUIRE=force \
        command scp "${SSH_OPTIONS[@]}" "$@"
      return
    fi
    command scp "${SSH_OPTIONS[@]}" "$@"
  }
  deploy_server_cleanup() {
    if [[ "$ssh_master_active" == true ]]; then
      ssh_remote "$DEPLOY_TARGET" "rm -f '$remote_archive'; rmdir '$remote_lock' 2>/dev/null || true" >/dev/null 2>&1 || true
      ssh_remote -O exit "$DEPLOY_TARGET" >/dev/null 2>&1 || true
    fi
    rm -f "$askpass_script" "$control_path" "$local_archive" "$initial_env_file"
  }
  trap deploy_server_cleanup EXIT
  trap 'exit 130' INT TERM

  deploy_ensure_docker_cli
  deploy_require_commands docker ssh scp gzip curl git sed awk tail od tr
  docker info >/dev/null 2>&1 || deploy_die "Local Docker daemon is unavailable"
  docker buildx version >/dev/null 2>&1 || deploy_die "Local docker buildx is unavailable"
  for file in "$ROOT_DIR/$COMPOSE_FILE" "$ROOT_DIR/$CADDY_FILE"; do
    [[ -f "$file" ]] || deploy_die "Missing deployment input: $file"
  done

  local app_version archive_sha remote_start_time version start_time status_json
  app_version=$(deploy_build_version "$ROOT_DIR")
  deploy_build_image "$ROOT_DIR" "$BUILD_IMAGE" linux/amd64 "$app_version" "$goproxy" "$goproxy_fallback" "$no_cache" \
    || deploy_die "$DEPLOY_NAME local source build failed"
  deploy_assert_image_platform "$BUILD_IMAGE" linux/amd64
  deploy_assert_image_runs "$BUILD_IMAGE" "$app_version" linux/amd64
  docker save "$BUILD_IMAGE" | gzip >"$local_archive"
  [[ -s "$local_archive" ]] || deploy_die "$DEPLOY_NAME image archive is empty"
  archive_sha=$(deploy_file_sha256 "$local_archive")

  deploy_log "Connecting to $DEPLOY_NAME"
  ssh_remote -MNf "$DEPLOY_TARGET"
  ssh_master_active=true
  ssh_remote "$DEPLOY_TARGET" "docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1" \
    || deploy_die "Remote Docker is unavailable"
  ssh_remote "$DEPLOY_TARGET" "mkdir -p '$REMOTE_DIR'"
  if ! ssh_remote "$DEPLOY_TARGET" "test -f '$REMOTE_DIR/.env'"; then
    if ! deploy_flag_enabled DEPLOY_INITIALIZE_ENV false; then
      deploy_die "Remote .env is missing: $REMOTE_DIR/.env"
    fi
    initial_env_file=$(mktemp "${TMPDIR:-/tmp}/new-api-${DEPLOY_SLUG}-env.XXXXXX")
    deploy_prepare_env_file "$initial_env_file"
    scp_remote "$initial_env_file" "$DEPLOY_TARGET:$REMOTE_DIR/.env"
    ssh_remote "$DEPLOY_TARGET" "chmod 600 '$REMOTE_DIR/.env'"
  fi
  ssh_remote "$DEPLOY_TARGET" "mkdir '$remote_lock' 2>/dev/null || { find '$remote_lock' -maxdepth 0 -mmin +60 -print -quit | grep -q . && rmdir '$remote_lock' && mkdir '$remote_lock'; }" \
    || deploy_die "Another $DEPLOY_NAME deployment is active"

  scp_remote "$ROOT_DIR/$COMPOSE_FILE" "$DEPLOY_TARGET:$REMOTE_DIR/$COMPOSE_FILE"
  scp_remote "$ROOT_DIR/$CADDY_FILE" "$DEPLOY_TARGET:$REMOTE_DIR/$CADDY_FILE"
  scp_remote "$local_archive" "$DEPLOY_TARGET:$remote_archive"

  ssh_remote "$DEPLOY_TARGET" "bash -s -- '$REMOTE_DIR' '$COMPOSE_FILE' '$PROXY_SERVICE' '$remote_archive' '$BUILD_IMAGE' '$TARGET_IMAGE' '$ROLLBACK_IMAGE' '$archive_sha' '$app_version' '$remote_state' '${DEPLOY_DATABASE_BACKUP_ENABLED:-true}' '$DEPLOY_NAME'" <<'REMOTE_DEPLOY'
set -Eeuo pipefail
remote_dir=$1; compose_file=$2; proxy_service=$3; archive=$4; build_image=$5
target_image=$6; rollback_image=$7; expected_sha=$8; expected_version=$9
state_file=${10}; backup_enabled=${11}; deploy_name=${12}
cd "$remote_dir"
compose=(docker compose --env-file .env -f "$compose_file")
switched=false; rollback_available=false
file_sha256() { command -v sha256sum >/dev/null 2>&1 && sha256sum "$1" | awk '{print $1}' || shasum -a 256 "$1" | awk '{print $1}'; }
wait_healthy() {
  local container=$1 attempts=${2:-45} health
  for ((attempt=1; attempt<=attempts; attempt++)); do
    health=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || true)
    [[ "$health" == healthy ]] && return 0
    [[ "$health" != unhealthy && "$health" != exited && "$health" != dead ]] || return 1
    sleep 2
  done
  return 1
}
wait_status() {
  for ((attempt=1; attempt<=45; attempt++)); do
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
  status=$?; trap - EXIT; rm -f "$archive"
  if ((status != 0)) && [[ "$switched" == true ]]; then
    rollback || echo "[deploy] Warning: $deploy_name rollback failed" >&2
  fi
  exit "$status"
}
trap finish EXIT

[[ "$(file_sha256 "$archive")" == "$expected_sha" ]] || { echo "Transferred image checksum mismatch" >&2; exit 1; }
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
  mapfile -t backups < <(find backups -maxdepth 1 -name 'predeploy-*.sql.gz' -type f | sort)
  while ((${#backups[@]} > 3)); do rm -f "${backups[0]}"; backups=("${backups[@]:1}"); done
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
REMOTE_DEPLOY

  remote_start_time=$(ssh_remote "$DEPLOY_TARGET" "sed -n 's/^START_TIME=//p' '$remote_state'")
  version=""; start_time=""
  for ((attempt=1; attempt<=30; attempt++)); do
    if status_json=$(curl --noproxy '*' --fail --silent --show-error --max-time 5 --header 'Cache-Control: no-cache' "$HEALTH_URL?deploy_check=$remote_start_time" 2>/dev/null); then
      version=$(printf '%s' "$status_json" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
      start_time=$(printf '%s' "$status_json" | sed -n 's/.*"start_time":\([0-9][0-9]*\).*/\1/p')
      [[ "$version" == "$app_version" && "$start_time" == "$remote_start_time" ]] && break
    fi
    sleep 2
  done
  if [[ "$version" != "$app_version" || "$start_time" != "$remote_start_time" ]] || ! deploy_verify_relay_routes "${HEALTH_URL%/api/status}"; then
    ssh_remote "$DEPLOY_TARGET" "cd '$REMOTE_DIR' && docker image inspect '$ROLLBACK_IMAGE' >/dev/null && docker tag '$ROLLBACK_IMAGE' '$TARGET_IMAGE' && docker compose --env-file .env -f '$COMPOSE_FILE' up -d --no-build --no-deps --force-recreate --remove-orphans new-api" || true
    deploy_die "$DEPLOY_NAME deployment verification failed"
  fi
  # Clean only obsolete deployment-managed files after the new release has
  # passed both container and public endpoint verification.
  ssh_remote "$DEPLOY_TARGET" "find '$REMOTE_DIR' -maxdepth 1 -type f \
    \( -name 'docker-compose*.yml' -o -name 'Caddyfile*' \) \
    ! -name '$COMPOSE_FILE' ! -name '$CADDY_FILE' -delete" \
    || deploy_log "Warning: obsolete deployment file cleanup failed"
  if deploy_flag_enabled DEPLOY_PRUNE_DANGLING_IMAGES true; then
    ssh_remote "$DEPLOY_TARGET" "docker image prune --force --filter 'label=org.opencontainers.image.title=new-api'" || deploy_log "Warning: image cleanup failed"
  fi
  deploy_log "$DEPLOY_NAME deployment completed: version=$app_version start_time=$remote_start_time"
}
