#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
REMOTE_SCRIPT="$ROOT_DIR/bin/deploy-remote.sh"
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/new-api-deploy-remote-test.XXXXXX")
trap 'rm -rf "$TEST_DIR"' EXIT

fail() {
  printf '[test] %s\n' "$*" >&2
  exit 1
}

write_fake_docker() {
  mkdir -p "$TEST_DIR/bin"
  cat >"$TEST_DIR/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -Eeuo pipefail

printf '%s\n' "$*" >>"$DEPLOY_TEST_LOG"

case "${1:-}" in
  compose)
    case " $* " in
      *" config -q "*) exit 0 ;;
      *" ps -q "*) printf 'caddy-test\n'; exit 0 ;;
      *" up -d "*)
        if [[ " $* " == *" new-api "* ]]; then
          printf 'new\n' >"$DEPLOY_TEST_STATE"
        fi
        exit 0
        ;;
      *" exec -T "*|*" run --rm "*) exit 0 ;;
    esac
    ;;
  inspect)
    if [[ "${2:-}" == "-f" ]]; then
      format=${3:-}
      target=${4:-}
      case "$format" in
        *Health*) printf 'healthy\n'; exit 0 ;;
        '{{.Image}}')
          if [[ "$target" == new-api ]]; then
            if [[ -f "$DEPLOY_TEST_STATE" ]]; then
              printf 'sha256:new\n'
            else
              printf 'sha256:old\n'
            fi
            exit 0
          fi
          ;;
      esac
    fi
    [[ "${2:-}" == new-api ]] && exit 0
    exit 1
    ;;
  image)
    if [[ "${2:-}" == inspect ]]; then
      if [[ "${3:-}" == sha256:old || "${3:-}" == new-api:build || "${3:-}" == new-api:target || "${3:-}" == new-api:rollback ]]; then
        if [[ "${4:-}" == "--format" ]]; then
          if [[ "${5:-}" == *'org.opencontainers.image.version'* ]]; then
            printf '%s\n' "$DEPLOY_TEST_VERSION"
          else
            printf 'sha256:new\n'
          fi
        fi
        exit 0
      fi
    fi
    ;;
  exec)
    if [[ " $* " == *' pg_dump '* ]]; then
      printf 'CREATE TABLE deploy_test ();\n'
      exit 0
    fi
    if [[ " $* " == *' wget '* ]]; then
      version=$DEPLOY_TEST_VERSION
      if [[ "${DEPLOY_TEST_BAD_IDENTITY:-false}" == true ]]; then
        version=unexpected
      fi
      printf '{"version":"%s","start_time":123}\n' "$version"
      exit 0
    fi
    ;;
  load|tag|logs)
    exit 0
    ;;
esac

exit 0
DOCKER
  chmod +x "$TEST_DIR/bin/docker"
}

file_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
    return
  fi
  shasum -a 256 "$1" | awk '{print $1}'
}

run_case() {
  local name=$1
  local bad_identity=${2:-false}
  local remote_dir="$TEST_DIR/$name"
  local archive="$TEST_DIR/$name.tar.gz"
  local state_file="$remote_dir/.deploy-state.env"

  mkdir -p "$remote_dir"
  : >"$remote_dir/.env"
  : >"$remote_dir/docker-compose.yml"
  printf '' | gzip >"$archive"

  DEPLOY_TEST_LOG="$TEST_DIR/$name.log" \
    DEPLOY_TEST_STATE="$TEST_DIR/$name.state" \
    DEPLOY_TEST_VERSION='v-test' \
    DEPLOY_TEST_BAD_IDENTITY="$bad_identity" \
    PATH="$TEST_DIR/bin:$PATH" \
    "$REMOTE_SCRIPT" "$remote_dir" docker-compose.yml caddy "$archive" \
      new-api:build new-api:target new-api:rollback \
      "$(file_sha256 "$archive")" v-test "$state_file" true deploy-test
}

write_fake_docker

run_case success
grep -q '^APP_VERSION=v-test$' "$TEST_DIR/success/.deploy-state.env" ||
  fail 'successful deployment did not persist the verified version'

if run_case rollback true >/dev/null 2>&1; then
  fail 'identity mismatch unexpectedly succeeded'
fi
grep -q '^tag new-api:rollback new-api:target$' "$TEST_DIR/rollback.log" ||
  fail 'identity mismatch did not restore the rollback image'

printf '[test] deploy-remote success and rollback simulations passed\n'
