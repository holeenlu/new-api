#!/usr/bin/env bash
#
# Compatibility entry point for deploying new-api to 104.128.92.169.
# The canonical implementation is bin/deploy-104.128.92.169.sh.
#
# Supported compatibility variables:
#   REMOTE / DEPLOY_TARGET
#   IMAGE / REMOTE_IMAGE
#   REMOTE_DIR
#   APP_VERSION / RELEASE_VERSION
#   PLATFORM, BUILD_IMAGE, HEALTH_URL, GOPROXY, GOPROXY_FALLBACK, NO_CACHE
#   SSHPASS (optional non-interactive SSH password)

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
export DEPLOY_TARGET=${DEPLOY_TARGET:-${REMOTE:-root@104.128.92.169}}
export REMOTE_IMAGE=${REMOTE_IMAGE:-${IMAGE:-new-api:oauth-local}}

exec "$ROOT_DIR/bin/deploy-104.128.92.169.sh" "$@"
