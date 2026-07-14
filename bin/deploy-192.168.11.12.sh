#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
export DEPLOY_TARGET=${DEPLOY_TARGET:-kdan@192.168.11.12}
export REMOTE_DIR=${REMOTE_DIR:-/home/kdan/newapi-proxy}
export TARGET_COMPOSE=${TARGET_COMPOSE:-docker-compose.server-192.168.11.12.yml}
export PLATFORM=${PLATFORM:-linux/amd64}
export BUILD_IMAGE=${BUILD_IMAGE:-new-api:deploy-192-amd64}
export REMOTE_IMAGE=${REMOTE_IMAGE:-new-api:oauth-local}
export HEALTH_URL=${HEALTH_URL:-http://192.168.11.12:3000/api/status}

exec "$ROOT_DIR/bin/deploy-remote.sh" "$@"
