#!/usr/bin/env bash
#
# deploy-server-104.128.92.169.sh
#
# 本机构建 linux/amd64 的 new-api 镜像，流式传输到生产服务器
# (root@104.128.92.169 / vpn2.buildtoconnect.com) 并只重建 new-api 容器。
#
# 用法：
#   ./deploy-server-104.128.92.169.sh
#   SSHPASS='服务器密码' ./deploy-server-104.128.92.169.sh   # 非交互
#
# 可覆盖的环境变量：
#   REMOTE        默认 root@104.128.92.169
#   IMAGE         默认 new-api:oauth-local
#   PLATFORM      默认 linux/amd64
#   SERVICE       默认 new-api（compose 服务名）
#   REMOTE_DIR    显式指定服务器部署目录（自动发现失败时用）
#   COMPOSE_FILE  显式指定 compose 文件（逗号分隔多文件；配合 REMOTE_DIR）
#
# 前提：本机已装 docker(带 buildx) 和 sshpass。
#   macOS 装 sshpass：brew install hudochenkov/sshpass/sshpass
#
# 回滚：服务器仍保留上一份镜像(docker images 里同名 <none> 的旧 ID)时，可执行：
#   docker tag <旧ID> new-api:oauth-local && \
#   docker compose -f <compose文件> up -d --no-deps --no-build --force-recreate new-api
#
set -euo pipefail

REMOTE="${REMOTE:-root@104.128.92.169}"
IMAGE="${IMAGE:-new-api:oauth-local}"
PLATFORM="${PLATFORM:-linux/amd64}"
SERVICE="${SERVICE:-new-api}"
SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=15)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log()  { printf '\033[1;34m[deploy]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[  ok  ]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[ warn ]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# 0. 预检与密码
# ---------------------------------------------------------------------------
command -v docker  >/dev/null 2>&1 || die "本机未找到 docker"
docker info        >/dev/null 2>&1 || die "docker daemon 未运行，请先启动 Docker Desktop"
docker buildx version >/dev/null 2>&1 || die "docker buildx 不可用"
command -v sshpass >/dev/null 2>&1 || die "本机未找到 sshpass；macOS 安装：brew install hudochenkov/sshpass/sshpass"

if [[ -z "${SSHPASS:-}" ]]; then
  read -r -s -p "请输入 ${REMOTE} 的 SSH 密码: " SSHPASS
  echo
  [[ -n "$SSHPASS" ]] || die "密码为空"
fi
export SSHPASS

# 统一的远程执行入口：密码只经 sshpass -e 从环境变量读取，不出现在命令行参数里。
rssh() { sshpass -e ssh "${SSH_OPTS[@]}" "$REMOTE" "$@"; }

log "校验 SSH 连接 ${REMOTE} ..."
rssh 'echo ok >/dev/null' || die "SSH 连接失败：请确认主机、密码、以及服务器允许密码登录"
rssh 'command -v docker >/dev/null 2>&1' || die "服务器上未找到 docker"
rssh 'docker compose version >/dev/null 2>&1' || die "服务器上 docker compose 不可用"
ok "SSH 与服务器 docker 环境正常"

# ---------------------------------------------------------------------------
# 1. 本地交叉构建 linux/amd64 镜像
# ---------------------------------------------------------------------------
log "构建镜像 ${IMAGE} (${PLATFORM}) —— Apple Silicon 走 QEMU 模拟，前端+Go 编译需数分钟 ..."
docker buildx build --platform "$PLATFORM" --load -t "$IMAGE" "$SCRIPT_DIR"
LOCAL_IMAGE_ID="$(docker image inspect "$IMAGE" -f '{{.Id}}')"
ok "构建完成，本地镜像 ID: ${LOCAL_IMAGE_ID}"

# ---------------------------------------------------------------------------
# 2. 流式传输并在服务器加载（免临时文件）
# ---------------------------------------------------------------------------
log "传输镜像到 ${REMOTE} 并 docker load（约 ~76MB 压缩）..."
if command -v pv >/dev/null 2>&1; then
  docker save "$IMAGE" | gzip | pv | sshpass -e ssh "${SSH_OPTS[@]}" "$REMOTE" 'gunzip | docker load'
else
  docker save "$IMAGE" | gzip | sshpass -e ssh "${SSH_OPTS[@]}" "$REMOTE" 'gunzip | docker load'
fi
ok "镜像已加载到服务器"

# ---------------------------------------------------------------------------
# 3. 自动发现现有部署目录 + compose 文件（沿用现有目录）
# ---------------------------------------------------------------------------
if [[ -z "${REMOTE_DIR:-}" || -z "${COMPOSE_FILE:-}" ]]; then
  log "从运行中的 ${SERVICE} 容器发现现有部署目录 ..."
  CID="$(rssh "docker ps --filter label=com.docker.compose.service=${SERVICE} --format '{{.ID}}' | head -n1")"
  if [[ -z "$CID" ]]; then
    CID="$(rssh "docker ps --filter name=${SERVICE} --format '{{.ID}}' | head -n1")"
  fi
  [[ -n "$CID" ]] || die "未发现运行中的 ${SERVICE} 容器；请用 REMOTE_DIR 和 COMPOSE_FILE 环境变量显式指定后重跑"

  DISC_DIR="$(rssh "docker inspect -f '{{ index .Config.Labels \"com.docker.compose.project.working_dir\" }}' ${CID}")"
  DISC_FILES="$(rssh "docker inspect -f '{{ index .Config.Labels \"com.docker.compose.project.config_files\" }}' ${CID}")"
  REMOTE_DIR="${REMOTE_DIR:-$DISC_DIR}"
  COMPOSE_FILE="${COMPOSE_FILE:-$DISC_FILES}"
fi
[[ -n "${REMOTE_DIR:-}" ]]   || die "无法确定部署目录，请设置 REMOTE_DIR"
[[ -n "${COMPOSE_FILE:-}" ]] || die "无法确定 compose 文件，请设置 COMPOSE_FILE"
ok "部署目录: ${REMOTE_DIR}"
ok "compose : ${COMPOSE_FILE}"

# compose 标签里多文件以逗号分隔，展开成多个 -f 参数
COMPOSE_ARGS=""
IFS=',' read -r -a _files <<< "$COMPOSE_FILE"
for f in "${_files[@]}"; do
  f="$(echo "$f" | xargs)"   # trim
  [[ -n "$f" ]] && COMPOSE_ARGS+=" -f '$f'"
done

# ---------------------------------------------------------------------------
# 4. 只重建 new-api 容器（不动 redis/postgres/caddy，不改配置）
# ---------------------------------------------------------------------------
log "重建 ${SERVICE} 容器 ..."
rssh "cd '$REMOTE_DIR' && docker compose ${COMPOSE_ARGS} up -d --no-deps --no-build --force-recreate ${SERVICE}"
ok "${SERVICE} 已用新镜像重建"

# ---------------------------------------------------------------------------
# 5. 部署后校验
# ---------------------------------------------------------------------------
log "校验容器状态与健康 ..."
rssh "cd '$REMOTE_DIR' && docker compose ${COMPOSE_ARGS} ps ${SERVICE}"

RUNNING_IMAGE_ID="$(rssh "docker inspect -f '{{.Image}}' \$(docker ps --filter label=com.docker.compose.service=${SERVICE} --format '{{.ID}}' | head -n1)")"
if [[ "$RUNNING_IMAGE_ID" == "$LOCAL_IMAGE_ID" ]]; then
  ok "运行镜像 ID 与本地构建一致：${RUNNING_IMAGE_ID}"
else
  warn "运行镜像 ID (${RUNNING_IMAGE_ID}) 与本地构建 (${LOCAL_IMAGE_ID}) 不一致，请检查"
fi

# new-api 在 compose 里映射到 127.0.0.1:3001:3000，从服务器本地探活
log "健康检查 http://127.0.0.1:3001/api/status ..."
if rssh 'curl -fsS -m 15 http://127.0.0.1:3001/api/status >/dev/null 2>&1'; then
  ok "healthcheck 通过（/api/status 200）"
else
  warn "healthcheck 未通过，输出最近日志："
  rssh "cd '$REMOTE_DIR' && docker compose ${COMPOSE_ARGS} logs --tail=50 ${SERVICE}" || true
  die "部署完成但健康检查失败，请查看上面日志"
fi

# ---------------------------------------------------------------------------
# 6. 清理服务器悬空镜像
# ---------------------------------------------------------------------------
log "清理服务器悬空镜像 ..."
rssh 'docker image prune -f >/dev/null 2>&1 || true'

ok "部署完成 ✅  可访问 https://vpn2.buildtoconnect.com 验证；并在 /channels 重测 claude-code-pro 渠道。"
