#!/usr/bin/env bash
#
# vless-small 卸载脚本.
#
# 用法:
#   sudo bash uninstall.sh
#   sudo bash uninstall.sh --docker
#   bash uninstall.sh --install-dir "$HOME/.local/bin"
#
# 默认清理 systemd 或 OpenRC 服务、/etc/vless-small、/opt/vless-small 和
# /usr/local/bin/vless-small 符号链接；非 root 安装只清理用户目录。

# 脚本以 POSIX sh 编写，Alpine 的 busybox ash 也能执行。
# pipefail 在老版本 ash 上不可用，检测到才开启。
set -eu
if (set -o pipefail) 2>/dev/null; then
  set -o pipefail
fi

SERVICE_NAME="vless-small"
ENV_DIR="/etc/vless-small"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
# OpenRC（Alpine）的 init 脚本与 conf.d 配置。
OPENRC_SCRIPT="/etc/init.d/${SERVICE_NAME}"
OPENRC_CONFD="/etc/conf.d/${SERVICE_NAME}"
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
  --service           强制按 systemd/OpenRC 安装卸载
  --no-service        只删除二进制，不处理系统服务
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
if [ "$IS_ROOT" -ne 1 ] && { [ -e "$UNIT_FILE" ] || [ -e "$OPENRC_SCRIPT" ] || [ -e "$ENV_DIR" ] || [ -e "/opt/vless-small" ]; }; then
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

# OpenRC（Alpine、Gentoo）：没有 systemd 时用 rc-service/rc-update 清理。
HAS_OPENRC=0
if [ "$(uname -s)" = "Linux" ] && [ "$HAS_SYSTEMD" -eq 0 ] && [ -d /run/openrc ] \
  && command -v rc-service >/dev/null 2>&1; then
  HAS_OPENRC=1
fi

USE_SERVICE=0
case "$SERVICE_MODE" in
  yes)
    [ "$IS_ROOT" -eq 1 ] || die "--service 需要 root 权限"
    { [ "$HAS_SYSTEMD" -eq 1 ] || [ "$HAS_OPENRC" -eq 1 ]; } \
      || die "--service 需要 systemd 或 OpenRC"
    USE_SERVICE=1
    ;;
  no)
    USE_SERVICE=0
    ;;
  auto)
    if [ "$IS_ROOT" -eq 1 ] && { [ -f "$UNIT_FILE" ] || [ -f "$OPENRC_SCRIPT" ]; }; then
      USE_SERVICE=1
    fi
    ;;
esac

if [ "$USE_SERVICE" -eq 1 ]; then
  # 停止服务按当前可用的管理器，但注册文件两套都清理：机器可能在
  # systemd ↔ OpenRC 之间迁移过，残留另一套的 unit/init 会让重装出现
  # 两个服务同时启动。
  if [ "$HAS_SYSTEMD" -eq 1 ]; then
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    systemctl disable "$SERVICE_NAME" 2>/dev/null || true
  fi
  if [ "$HAS_OPENRC" -eq 1 ]; then
    rc-service "$SERVICE_NAME" stop >/dev/null 2>&1 || true
    rc-update del "$SERVICE_NAME" default >/dev/null 2>&1 || true
  fi

  if [ -f "$UNIT_FILE" ]; then
    info "移除 systemd 服务 ${SERVICE_NAME}"
    rm -f "$UNIT_FILE"
    [ "$HAS_SYSTEMD" -eq 1 ] && systemctl daemon-reload 2>/dev/null || true
  fi
  if [ -f "$OPENRC_SCRIPT" ] || [ -f "$OPENRC_CONFD" ]; then
    info "移除 OpenRC 服务 ${SERVICE_NAME}"
    rm -f "$OPENRC_SCRIPT" "$OPENRC_CONFD"
  fi
fi

# 只有显式 --no-service 才保留配置目录和服务用户（用于“只换二进制”）。
# auto 模式下即使没检测到已注册服务也要清理：安装可能写到一半就失败，
# 此时含 token 的 env 文件仍留在磁盘上，默认卸载必须能删掉它。
KEEP_SYSTEM_STATE=0
if [ "$SERVICE_MODE" = "no" ]; then
  KEEP_SYSTEM_STATE=1
fi

# 显式 --no-service 只删二进制：配置目录和服务用户属于系统安装的一部分，
# 保留它们才能让同一个 unit/账户在重装后继续使用。其余情况（含 auto 下
# 没检测到服务）都要清理，否则写到一半的安装会把含 token 的 env 留在磁盘。
if [ "$KEEP_SYSTEM_STATE" -eq 0 ]; then
  rm -rf "$ENV_DIR"
fi
rm -f "$BIN_PATH"
if [ -n "$CLI_LINK" ] && [ -L "$CLI_LINK" ]; then
  rm -f "$CLI_LINK"
fi
rmdir "$INSTALL_DIR" 2>/dev/null || true
if [ "$KEEP_SYSTEM_STATE" -eq 0 ]; then
  info "已删除 ${BIN_PATH} 与 ${ENV_DIR}"
else
  info "已删除 ${BIN_PATH}（--no-service：保留 ${ENV_DIR} 与服务用户）"
fi

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

# 与服务配置同样的规则：只有显式 --no-service 才保留账户。
if [ "$KEEP_SYSTEM_STATE" -eq 0 ] && [ "$IS_ROOT" -eq 1 ] && [ "$REMOVE_USER" -eq 1 ] && [ "$SERVICE_USER" != "nobody" ]; then
  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    # Alpine 的 busybox 只有 deluser，没有 userdel。
    if command -v deluser >/dev/null 2>&1; then
      deluser "$SERVICE_USER" >/dev/null 2>&1 \
        && info "已删除系统用户 ${SERVICE_USER}" \
        || warn "删除用户 ${SERVICE_USER} 失败，可手动处理"
    elif command -v userdel >/dev/null 2>&1; then
      userdel "$SERVICE_USER" 2>/dev/null \
        && info "已删除系统用户 ${SERVICE_USER}" \
        || warn "删除用户 ${SERVICE_USER} 失败，可手动处理"
    else
      warn "没有 deluser/userdel，跳过用户 ${SERVICE_USER} 清理"
    fi
  fi
fi

info "卸载完成"
