#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
export DEPLOY_TARGET=${DEPLOY_TARGET:-root@174.137.56.226}
export REMOTE_DIR=${REMOTE_DIR:-/opt/newapi-proxy}
export TARGET_COMPOSE=${TARGET_COMPOSE:-docker-compose.server-174.137.56.226.yml}
export PLATFORM=${PLATFORM:-linux/amd64}
export BUILD_IMAGE=${BUILD_IMAGE:-new-api:deploy-174-amd64}
export REMOTE_IMAGE=${REMOTE_IMAGE:-new-api:oauth-local}
export HEALTH_URL=${HEALTH_URL:-https://nextcode.buildtoconnect.com/api/status}
export EXTRA_SERVICES=${EXTRA_SERVICES:-caddy}

exec "$ROOT_DIR/bin/deploy-remote.sh" "$@"
