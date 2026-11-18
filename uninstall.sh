#!/usr/bin/env bash
#
# vless-small 卸载脚本.
#
# 用法:
#   sudo bash uninstall.sh
#   sudo bash uninstall.sh --docker
#   bash uninstall.sh --install-dir "$HOME/.local/bin"
#
# 默认清理 systemd 服务、/etc/vless-small、/opt/vless-small 和
# /usr/local/bin/vless-small 符号链接；非 root 安装只清理用户目录。
# Tailscale 的 tsnet 状态存在主控数据库，不在本脚本清理范围内。

if [ -z "${BASH_VERSION:-}" ]; then
  echo "uninstall.sh 需要 bash" >&2
  exit 1
fi

set -euo pipefail

SERVICE_NAME="vless-small"
ENV_DIR="/etc/vless-small"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
SERVICE_USER="${VLESS_SMALL_SERVICE_USER:-vless-small}"
INSTALL_DIR="${INSTALL_DIR:-}"
SERVICE_MODE="${VLESS_SMALL_SERVICE:-auto}"
REMOVE_USER=1
DOCKER=0

info() { printf '==> %s\n' "$*"; }
warn() { printf '警告: %s\n' "$*" >&2; }
die() {
  printf '错误: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
vless-small 卸载脚本

用法:
  sudo bash uninstall.sh [选项]

选项:
  --install-dir DIR   二进制安装目录；root 默认 /opt/vless-small，非 root 默认 ~/.local/bin
  --service           强制按 systemd 安装卸载
  --no-service        只删除二进制，不处理 systemd
  --docker            额外停止并删除名为 vless-small 的 Docker 容器
  --keep-user         保留 vless-small 系统用户
  -h, --help          显示帮助
EOF
}

need_value() {
  [ "$#" -ge 2 ] || die "$1 缺少值"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --install-dir)
      need_value "$@"
      INSTALL_DIR="$2"
      shift 2
      ;;
    --service)
      SERVICE_MODE="yes"
      shift
      ;;
    --no-service)
      SERVICE_MODE="no"
      shift
      ;;
    --docker)
      DOCKER=1
      shift
      ;;
    --keep-user)
      REMOVE_USER=0
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "未知参数: $1"
      ;;
  esac
done

case "$SERVICE_MODE" in
  auto|yes|no) ;;
  *) die "VLESS_SMALL_SERVICE 只能是 auto、yes 或 no" ;;
esac

IS_ROOT=0
if [ "$(id -u)" -eq 0 ]; then
  IS_ROOT=1
fi

if [ -z "$INSTALL_DIR" ]; then
  if [ "$IS_ROOT" -eq 1 ]; then
    INSTALL_DIR="/opt/vless-small"
  else
    [ -n "${HOME:-}" ] || die "无法确定 HOME，请用 --install-dir 指定"
    INSTALL_DIR="${HOME}/.local/bin"
  fi
fi
BIN_PATH="${INSTALL_DIR}/vless-small"

# 系统级安装的文件属于 root，非 root 清理会留下半删状态。
if [ "$IS_ROOT" -ne 1 ] && { [ -e "$UNIT_FILE" ] || [ -e "$ENV_DIR" ] || [ -e "/opt/vless-small" ]; }; then
  die "系统级安装需要 root，请使用 sudo bash uninstall.sh"
fi

CLI_LINK=""
if [ "$IS_ROOT" -eq 1 ] && [ "$INSTALL_DIR" != "/usr/local/bin" ]; then
  CLI_LINK="/usr/local/bin/vless-small"
fi

HAS_SYSTEMD=0
if [ "$(uname -s)" = "Linux" ] && [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
  HAS_SYSTEMD=1
fi

USE_SERVICE=0
case "$SERVICE_MODE" in
  yes)
    [ "$IS_ROOT" -eq 1 ] || die "--service 需要 root 权限"
    [ "$HAS_SYSTEMD" -eq 1 ] || die "--service 需要 systemd"
    USE_SERVICE=1
    ;;
  no)
    USE_SERVICE=0
    ;;
  auto)
    if [ "$IS_ROOT" -eq 1 ] && [ -f "$UNIT_FILE" ]; then
      USE_SERVICE=1
    fi
    ;;
esac

if [ "$USE_SERVICE" -eq 1 ]; then
  info "停止并移除 systemd 服务 ${SERVICE_NAME}"
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true
  systemctl disable "$SERVICE_NAME" 2>/dev/null || true
  rm -f "$UNIT_FILE"
  systemctl daemon-reload 2>/dev/null || true
fi

rm -rf "$ENV_DIR"
rm -f "$BIN_PATH"
if [ -n "$CLI_LINK" ] && [ -L "$CLI_LINK" ]; then
  rm -f "$CLI_LINK"
fi
rmdir "$INSTALL_DIR" 2>/dev/null || true
info "已删除 ${BIN_PATH} 与 ${ENV_DIR}"

if [ "$DOCKER" -eq 1 ]; then
  if command -v docker >/dev/null 2>&1; then
    docker rm -f "$SERVICE_NAME" >/dev/null 2>&1 \
      && info "已删除 Docker 容器 ${SERVICE_NAME}" \
      || warn "没有找到 Docker 容器 ${SERVICE_NAME}"
    info "Docker 镜像需要手动删除（docker image ls）"
  else
    warn "没有 docker 命令，跳过容器清理"
  fi
fi

if [ "$IS_ROOT" -eq 1 ] && [ "$REMOVE_USER" -eq 1 ] && [ "$SERVICE_USER" != "nobody" ]; then
  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    userdel "$SERVICE_USER" 2>/dev/null \
      && info "已删除系统用户 ${SERVICE_USER}" \
      || warn "删除用户 ${SERVICE_USER} 失败，可手动处理"
  fi
fi

info "卸载完成"
