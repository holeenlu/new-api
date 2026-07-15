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

deploy_source_version() {
  local root_dir=$1
  git -C "$root_dir" describe --tags --always --dirty 2>/dev/null || sed -n '1p' "$root_dir/VERSION"
}

deploy_build_version() {
  local root_dir=$1
  local release_version
  release_version=$(git -C "$root_dir" describe --tags --abbrev=0 2>/dev/null || sed -n '1p' "$root_dir/VERSION")
  [[ -n "$release_version" ]] || deploy_die "Unable to determine the release version"
  printf '%s\n' "$release_version"
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

  deploy_log "Building image=$image platform=${platform:-native} version=$app_version"
  "${args[@]}" "$root_dir"
  deploy_prune_build_cache
}

deploy_prune_build_cache() {
  local enabled=${DEPLOY_PRUNE_BUILD_CACHE:-true}
  local max_used_space=${DEPLOY_BUILDX_CACHE_MAX_USED_SPACE:-20GB}
  if [[ "$enabled" != "true" && "$enabled" != "1" ]]; then
    return 0
  fi

  deploy_log "Pruning Buildx cache to keep at most $max_used_space"
  if ! docker buildx prune --force --max-used-space "$max_used_space"; then
    deploy_log "Warning: Buildx cache cleanup failed; deployment will continue"
  fi
}

deploy_prune_project_images() {
  local enabled=${DEPLOY_PRUNE_PROJECT_IMAGES:-true}
  if [[ "$enabled" != "true" && "$enabled" != "1" ]]; then
    return 0
  fi

  deploy_log "Pruning unused new-api images"
  if ! docker image prune --force --filter "label=org.opencontainers.image.title=new-api"; then
    deploy_log "Warning: unused new-api image cleanup failed; deployment will continue"
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

deploy_file_sha256() {
  local file=$1
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$file" | awk '{print $1}'
    return
  fi
  deploy_die "Neither sha256sum nor shasum is available"
}

deploy_random_hex() {
  od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
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

  deploy_env_ensure "$env_file" CLAUDE_CODE_OAUTH_MAX_CONCURRENCY 5
  deploy_env_ensure "$env_file" CLAUDE_CODE_OAUTH_MIN_REQUEST_INTERVAL_MS 750
  deploy_env_ensure "$env_file" CODEX_OAUTH_MAX_CONCURRENCY 5
  deploy_env_ensure "$env_file" CODEX_OAUTH_MIN_REQUEST_INTERVAL_MS 750
  deploy_env_migrate_default "$env_file" SUBSCRIPTION_OAUTH_RESPONSE_HEADER_TIMEOUT 120 30
  deploy_env_ensure "$env_file" CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED true
  deploy_env_ensure "$env_file" CODEX_OAUTH_CLIENT_ID app_EMoamEEZ73f0CkXaXp7hrann
  deploy_env_ensure "$env_file" CODEX_OAUTH_REDIRECT_URI http://localhost:1455/auth/callback
  deploy_env_ensure "$env_file" CODEX_OAUTH_SCOPE "openid profile email offline_access"
  deploy_env_ensure "$env_file" UPSTREAM_LOCATION_MODE strip
  deploy_env_ensure "$env_file" UPSTREAM_SYSTEM_PROXY_ENABLED false
  deploy_env_ensure "$env_file" UPSTREAM_LOCATION_DISCOVERY_ENABLED true
  deploy_env_ensure "$env_file" UPSTREAM_LOCATION_DISCOVERY_TIMEOUT 8
}
