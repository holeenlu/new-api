#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
source "$ROOT_DIR/bin/deploy-common.sh"

DEPLOY_NAME=192.168.11.12
DEPLOY_SLUG=192
DEPLOY_TARGET=kdan@192.168.11.12
DEPLOY_SSH_KEY=${DEPLOY_SSH_KEY:-$HOME/.ssh/new-api-192-deploy}
REMOTE_DIR=/home/kdan/newapi-proxy
COMPOSE_FILE=docker-compose.server-192.168.11.12.yml
CADDY_FILE=Caddyfile.192.168.11.12
PROXY_SERVICE=gateway
TARGET_IMAGE=new-api:release-192
ROLLBACK_IMAGE=new-api:rollback-192
BUILD_IMAGE=new-api:build-192-amd64
HEALTH_URL=http://192.168.11.12:3000/api/status

deploy_server_main
