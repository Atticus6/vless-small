#!/usr/bin/env bash
#
# vless-small 一键安装脚本.
#
# 推荐用法（私有仓库；sudo 会自动注册 systemd 服务）:
#   GITHUB_TOKEN='<token>'; \
#     curl -fsSL -H "Authorization: token $GITHUB_TOKEN" \
#       https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
#     | sudo GITHUB_TOKEN="$GITHUB_TOKEN" bash -s -- \
#         --config "https://dsh.example.com_hk_<token>" --path /ws
#
# 脚本会下载当前平台对应的 Release 二进制、校验 sha256，并在 Linux + root +
# systemd 环境下生成 systemd 服务。重复执行即可升级，未传配置参数时会保留
# 已有的 /etc/vless-small/vless-small.env。
#
# systemd 安装默认使用 /opt/vless-small（属主为服务用户），这样 Web 下发的
# 自更新才能替换二进制；已有旧节点请重跑一次本脚本完成迁移。
#
# 依赖: curl、tar、sha256sum 或 shasum；注册服务时额外依赖 systemctl。

# 脚本使用了 bash 语法，被 sh 执行时要明确报错，而不是在半途失败。
if [ -z "${BASH_VERSION:-}" ]; then
  echo "install.sh 需要 bash，请使用: curl ... | sudo bash -s -- ..." >&2
  exit 1
fi

set -euo pipefail

DEFAULT_REPO="Atticus6/vless-small"
SERVICE_NAME="vless-small"
ENV_DIR="/etc/vless-small"
ENV_FILE="${ENV_DIR}/vless-small.env"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

# 命令行参数默认取自同名环境变量，方便 `CONFIG=... bash install.sh` 的写法。
REPO="${VLESS_SMALL_REPO:-${DEFAULT_REPO}}"
VERSION="${VLESS_SMALL_VERSION:-latest}"
# 私有仓库（默认 Atticus6/vless-small）下载 Release 需要 token；
# 公开仓库不设置也能安装。Web 代理模式使用 --download-base，不需要 GitHub Token。
GITHUB_TOKEN="${GITHUB_TOKEN:-}"
DOWNLOAD_BASE="${VLESS_SMALL_DOWNLOAD_BASE:-}"
INSTALL_DIR="${INSTALL_DIR:-}"
CONFIG="${CONFIG:-}"
UUID="${UUID:-}"
PORT_ARG="${PORT:-}"
WS_PATH="${WS_PATH:-}"
PUBLIC_HOSTS="${PUBLIC_HOSTS:-}"
NODE_RUNTIME="${NODE_RUNTIME:-}"
# auto = root + systemd 时自动注册服务；yes/no 强制开关。
SERVICE_MODE="${VLESS_SMALL_SERVICE:-auto}"
UNINSTALL=0

# 有任何配置输入时视为“更新配置”；否则升级时保留已有 env 文件。
CONFIG_CHANGED=0
for _value in "$CONFIG" "$UUID" "$PORT_ARG" "$WS_PATH" "$PUBLIC_HOSTS" "$NODE_RUNTIME"; do
  if [ -n "$_value" ]; then
    CONFIG_CHANGED=1
  fi
done
unset _value

info() { printf '==> %s\n' "$*"; }
warn() { printf '警告: %s\n' "$*" >&2; }
die() {
  printf '错误: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
vless-small 一键安装脚本

用法:
  curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
    | sudo bash -s -- [选项]

选项:
  --config VALUE        主控配置 <baseURL>_<name>_<token>，设置后自动同步设备
  --uuid VALUE          逗号分隔的静态 VLESS UUID；与 --config 至少提供一个
  --port VALUE          HTTP 监听端口，默认 8080（建议由反向代理转发 443）
  --path VALUE          WebSocket 路径，例如 /ws；默认接受任意路径
  --public-host VALUE   对外域名，可重复；也支持 --public-hosts a,b
  --node-runtime VALUE  节点上报的 runtime 名称，默认自动识别
  --version VALUE       Release 版本，默认 latest
  --repo OWNER/REPO     下载来源仓库，默认 Atticus6/vless-small
  --download-base URL   资产代理地址（Web 控制台生成）；设置后无需 GitHub Token
  --github-token VALUE  GitHub Token；直连私有仓库下载时使用
  --install-dir DIR     二进制安装目录；root 默认 /opt/vless-small（服务用户可写，支持自更新），
                        非 root 默认 ~/.local/bin；root 安装会在 /usr/local/bin 建符号链接
  --service             强制注册 systemd 服务（需要 root + systemd）
  --no-service          只安装二进制，不注册服务
  --uninstall           停止并删除服务、配置和二进制
  -h, --help            显示帮助

环境变量:
  CONFIG / UUID / PORT / WS_PATH / PUBLIC_HOSTS / NODE_RUNTIME
  VLESS_SMALL_VERSION / VLESS_SMALL_REPO / VLESS_SMALL_DOWNLOAD_BASE / VLESS_SMALL_SERVICE
  GITHUB_TOKEN          直连私有仓库时使用；Web 代理模式不需要
  INSTALL_DIR           等价于 --install-dir

示例:
  # Web 控制台代理（推荐，不需要 GitHub Token；命令由“VLESS 管理”页生成）
  NODE_NAME='hk-01'; NODE_TOKEN='<node token>'; \
    curl -fsSL -H "Authorization: Bearer $NODE_TOKEN" -H "X-Node-Name: $NODE_NAME" \
      "https://dsh.example.com/api/releases/v0.1.2/install.sh" \
    | sudo bash -s -- --config "https://dsh.example.com_hk-01_<node token>" \
        --version v0.1.2 \
        --download-base "https://dsh.example.com/api/releases/v0.1.2" --path /ws

  # 直连私有仓库：需要 GitHub Token
  GITHUB_TOKEN='<token>'; curl -fsSL -H "Authorization: token $GITHUB_TOKEN" \
    https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
    | sudo GITHUB_TOKEN="$GITHUB_TOKEN" bash -s -- \
        --config "https://dsh.example.com_hk_<token>" --path /ws

  # 下面的示例省略鉴权；Web 代理/直连私有仓库请按上面的方式处理。

  # 升级到最新版（保留已有配置）
  curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
    | sudo bash

  # 只装到当前用户，不注册服务
  curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
    | bash -s -- --uuid "14725836-1234-5678-9abc-def012345678" --no-service

  # 卸载
  curl -fsSL https://github.com/Atticus6/vless-small/releases/latest/download/install.sh \
    | sudo bash -s -- --uninstall
EOF
}

# 读取一个参数值；缺值时给出明确错误。
need_value() {
  [ "$#" -ge 2 ] || die "$1 缺少值"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --config)
      need_value "$@"
      CONFIG="$2"
      CONFIG_CHANGED=1
      shift 2
      ;;
    --uuid)
      need_value "$@"
      UUID="$2"
      CONFIG_CHANGED=1
      shift 2
      ;;
    --port)
      need_value "$@"
      PORT_ARG="$2"
      CONFIG_CHANGED=1
      shift 2
      ;;
    --path)
      need_value "$@"
      WS_PATH="$2"
      CONFIG_CHANGED=1
      shift 2
      ;;
    --public-host)
      need_value "$@"
      PUBLIC_HOSTS="${PUBLIC_HOSTS:+${PUBLIC_HOSTS},}$2"
      CONFIG_CHANGED=1
      shift 2
      ;;
    --public-hosts)
      need_value "$@"
      PUBLIC_HOSTS="$2"
      CONFIG_CHANGED=1
      shift 2
      ;;
    --node-runtime)
      need_value "$@"
      NODE_RUNTIME="$2"
      CONFIG_CHANGED=1
      shift 2
      ;;
    --version)
      need_value "$@"
      VERSION="$2"
      shift 2
      ;;
    --repo)
      need_value "$@"
      REPO="$2"
      shift 2
      ;;
    --download-base)
      need_value "$@"
      DOWNLOAD_BASE="$2"
      shift 2
      ;;
    --github-token)
      need_value "$@"
      GITHUB_TOKEN="$2"
      shift 2
      ;;
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
    --uninstall)
      UNINSTALL=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "未知参数: $1（用 --help 查看用法）"
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

# 安装目录：root 默认 /opt/vless-small，属于服务用户（自更新需要可写）；
# 非 root 走用户目录。--install-dir 优先。
CLI_LINK=""
if [ -z "$INSTALL_DIR" ]; then
  if [ "$IS_ROOT" -eq 1 ]; then
    INSTALL_DIR="/opt/vless-small"
  else
    [ -n "${HOME:-}" ] || die "无法确定 HOME，请用 --install-dir 指定安装目录"
    INSTALL_DIR="${HOME}/.local/bin"
  fi
fi
BIN_PATH="${INSTALL_DIR}/vless-small"
# root 安装在非 /usr/local/bin 时保留一个命令行符号链接。
if [ "$IS_ROOT" -eq 1 ] && [ "$INSTALL_DIR" != "/usr/local/bin" ]; then
  CLI_LINK="/usr/local/bin/vless-small"
fi

uninstall() {
  info "卸载 vless-small"

  if [ "$IS_ROOT" -eq 1 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl stop "$SERVICE_NAME" 2>/dev/null || true
    systemctl disable "$SERVICE_NAME" 2>/dev/null || true
    rm -f "$UNIT_FILE"
    systemctl daemon-reload 2>/dev/null || true
    info "已停止并移除 systemd 服务 ${SERVICE_NAME}"
  fi

  rm -rf "$ENV_DIR"
  rm -f "$BIN_PATH"
  if [ -n "${CLI_LINK:-}" ] && [ -L "$CLI_LINK" ]; then
    rm -f "$CLI_LINK"
  fi
  rmdir "$INSTALL_DIR" 2>/dev/null || true
  info "已删除 ${BIN_PATH} 与 ${ENV_DIR}"
}

if [ "$UNINSTALL" -eq 1 ]; then
  # 系统级安装的文件属于 root，非 root 卸载会留下半删状态，直接提示用 sudo。
  if [ "$IS_ROOT" -ne 1 ] && { [ -e "$UNIT_FILE" ] || [ -e "$ENV_FILE" ]; }; then
    die "卸载系统级安装需要 root，请使用 sudo bash install.sh --uninstall"
  fi
  uninstall
  exit 0
fi

command -v curl >/dev/null 2>&1 || die "缺少 curl，请先安装 curl"
command -v tar >/dev/null 2>&1 || die "缺少 tar，请先安装 tar"

OS_NAME="$(uname -s)"
case "$OS_NAME" in
  Linux) OS="linux" ;;
  *)
    die "仅提供 Linux 构建（当前系统: ${OS_NAME}）；其他平台请自行 go build"
    ;;
esac

MACHINE="$(uname -m)"
case "$MACHINE" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    die "Release 仅提供 linux/amd64 与 linux/arm64（当前架构: ${MACHINE}）；其他架构请自行 go build"
    ;;
esac

HAS_SYSTEMD=0
if [ "$OS" = "linux" ] && [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
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
    if [ "$IS_ROOT" -eq 1 ] && [ "$HAS_SYSTEMD" -eq 1 ]; then
      USE_SERVICE=1
    fi
    ;;
esac

# /usr/local/bin 属于 root，服务用户无法在目录里 rename，自更新会失败。
if [ "$USE_SERVICE" -eq 1 ] && [ "$INSTALL_DIR" = "/usr/local/bin" ]; then
  warn "安装目录 /usr/local/bin 不属于服务用户，Web 下发的自更新会失败；建议使用默认 /opt/vless-small"
fi

# 从已有 env 文件读取配置，用于升级时合并“新值 > 旧值 > 默认值”。
env_value() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1
}

EFFECTIVE_CONFIG="$CONFIG"
[ -n "$EFFECTIVE_CONFIG" ] || EFFECTIVE_CONFIG="$(env_value CONFIG)"
EFFECTIVE_UUID="$UUID"
[ -n "$EFFECTIVE_UUID" ] || EFFECTIVE_UUID="$(env_value UUID)"
EFFECTIVE_PORT="$PORT_ARG"
[ -n "$EFFECTIVE_PORT" ] || EFFECTIVE_PORT="$(env_value PORT)"
[ -n "$EFFECTIVE_PORT" ] || EFFECTIVE_PORT="8080"
EFFECTIVE_WS_PATH="$WS_PATH"
[ -n "$EFFECTIVE_WS_PATH" ] || EFFECTIVE_WS_PATH="$(env_value WS_PATH)"
EFFECTIVE_PUBLIC_HOSTS="$PUBLIC_HOSTS"
[ -n "$EFFECTIVE_PUBLIC_HOSTS" ] || EFFECTIVE_PUBLIC_HOSTS="$(env_value PUBLIC_HOSTS)"
EFFECTIVE_NODE_RUNTIME="$NODE_RUNTIME"
[ -n "$EFFECTIVE_NODE_RUNTIME" ] || EFFECTIVE_NODE_RUNTIME="$(env_value NODE_RUNTIME)"

# 服务升级场景允许沿用已有 env；首次安装或手动模式必须提供凭据。
if [ -z "$EFFECTIVE_CONFIG" ] && [ -z "$EFFECTIVE_UUID" ]; then
  if ! { [ "$USE_SERVICE" -eq 1 ] && [ -f "$ENV_FILE" ] && [ "$CONFIG_CHANGED" -eq 0 ]; }; then
    die "需要 --config 或 --uuid；示例: --config \"https://dsh.example.com_hk_<token>\""
  fi
fi

# systemd EnvironmentFile 不支持复杂引用，值里出现空白会破坏配置。
validate_value() {
  case "$2" in
    *[[:space:]]*) die "$1 不能包含空白字符" ;;
  esac
}
if [ -n "$EFFECTIVE_CONFIG" ]; then validate_value CONFIG "$EFFECTIVE_CONFIG"; fi
if [ -n "$EFFECTIVE_UUID" ]; then validate_value UUID "$EFFECTIVE_UUID"; fi
if [ -n "$EFFECTIVE_WS_PATH" ]; then validate_value WS_PATH "$EFFECTIVE_WS_PATH"; fi
if [ -n "$EFFECTIVE_PUBLIC_HOSTS" ]; then validate_value PUBLIC_HOSTS "$EFFECTIVE_PUBLIC_HOSTS"; fi
if [ -n "$EFFECTIVE_NODE_RUNTIME" ]; then validate_value NODE_RUNTIME "$EFFECTIVE_NODE_RUNTIME"; fi

case "$EFFECTIVE_PORT" in
  ""|*[!0-9]*) die "PORT 必须是 1-65535 的数字" ;;
esac
if [ "$EFFECTIVE_PORT" -lt 1 ] || [ "$EFFECTIVE_PORT" -gt 65535 ]; then
  die "PORT 必须是 1-65535 的数字"
fi

# 解析最新 tag。API 匿名限流（60 次/小时/IP）时回退解析 releases/latest 的跳转地址。
resolve_latest_version() {
  local api="https://api.github.com/repos/${REPO}/releases/latest"
  local tag=""

  if [ -n "${GITHUB_TOKEN:-}" ]; then
    tag="$(curl -fsSL -H "Authorization: Bearer ${GITHUB_TOKEN}" "$api" 2>/dev/null \
      | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
      | head -n 1 || true)"
  else
    tag="$(curl -fsSL "$api" 2>/dev/null \
      | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
      | head -n 1 || true)"
  fi

  if [ -z "$tag" ]; then
    local url=""
    url="$(curl -fsSIL -o /dev/null -w '%{url_effective}' \
      "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
    tag="${url##*/}"
  fi

  case "$tag" in
    # Release workflow 只处理 v* tag；其他跳转结果（例如 /releases）视为失败。
    v*) ;;
    *) return 1 ;;
  esac
  printf '%s\n' "$tag"
}

# Web 代理模式必须带具体版本：脚本不再向 GitHub 查询 latest。
if [ -n "$DOWNLOAD_BASE" ] && { [ "$VERSION" = "latest" ] || [ -z "$VERSION" ]; }; then
  die "使用 --download-base 时需要同时指定 --version"
fi

if [ -z "$DOWNLOAD_BASE" ] && { [ "$VERSION" = "latest" ] || [ -z "$VERSION" ]; }; then
  info "查询最新 Release 版本"
  VERSION="$(resolve_latest_version)" \
    || die "无法获取最新版本；请用 --version 指定，例如 --version v1.0.0"
fi
# 资产名与 workflow 保持一致，tag 统一带 v 前缀。
case "$VERSION" in
  v*) ;;
  *) VERSION="v${VERSION}" ;;
esac

ASSET="vless-small_${VERSION}_${OS}_${ARCH}.tar.gz"
if [ -z "$DOWNLOAD_BASE" ]; then
  DOWNLOAD_BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi
DOWNLOAD_BASE="${DOWNLOAD_BASE%/}"

# Web 代理模式：用 CONFIG 里的节点 name+token 鉴权，不再需要 GitHub Token。
PROXY_NAME=""
PROXY_TOKEN=""
if [ -n "$DOWNLOAD_BASE" ] && [ -n "${EFFECTIVE_CONFIG:-}" ]; then
  PROXY_TOKEN="${EFFECTIVE_CONFIG##*_}"
  PROXY_REST="${EFFECTIVE_CONFIG%_*}"
  PROXY_NAME="${PROXY_REST##*_}"
fi

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/vless-small.XXXXXX")"
trap 'rm -rf "$TMP_DIR"' EXIT

# 直连私有仓库用 GITHUB_TOKEN；Web 代理模式用节点 token，二者都不写入日志。
download() {
  local url="$1"
  local out="$2"

  if [ -n "${DOWNLOAD_BASE:-}" ]; then
    [ -n "$PROXY_NAME" ] && [ -n "$PROXY_TOKEN" ] \
      || die "使用 --download-base 时需要 --config（节点 name+token）"
    curl -fL --retry 3 --retry-delay 2 --connect-timeout 15 \
      -H "Authorization: Bearer ${PROXY_TOKEN}" \
      -H "X-Node-Name: ${PROXY_NAME}" \
      -o "$out" "$url"
    return
  fi

  if [ -n "${GITHUB_TOKEN:-}" ]; then
    curl -fL --retry 3 --retry-delay 2 --connect-timeout 15 \
      -H "Authorization: token ${GITHUB_TOKEN}" -o "$out" "$url"
  else
    curl -fL --retry 3 --retry-delay 2 --connect-timeout 15 -o "$out" "$url"
  fi
}

info "下载 ${ASSET}"
download "${DOWNLOAD_BASE}/${ASSET}" "${TMP_DIR}/${ASSET}" \
  || die "下载失败: ${DOWNLOAD_BASE}/${ASSET}（Web 代理请检查节点 token；直连请提供 --github-token 或 GITHUB_TOKEN）"
download "${DOWNLOAD_BASE}/checksums.txt" "${TMP_DIR}/checksums.txt" \
  || die "下载 checksums.txt 失败（Web 代理请检查节点 token；直连请提供 --github-token 或 GITHUB_TOKEN）"

# 兼容 sha256sum 的两种输出：文本模式 "hash  file"，二进制模式 "hash *file"。
EXPECTED="$(awk -v name="$ASSET" '{
  file = $2
  sub(/^\*/, "", file)
  if (file == name) { print $1; exit }
}' "${TMP_DIR}/checksums.txt")"
[ -n "$EXPECTED" ] || die "checksums.txt 中没有 ${ASSET} 的校验值"

if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMP_DIR}/${ASSET}" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "${TMP_DIR}/${ASSET}" | awk '{ print $1 }')"
else
  die "缺少 sha256sum 或 shasum，无法校验下载内容"
fi
[ "$ACTUAL" = "$EXPECTED" ] || die "sha256 校验失败（期望 ${EXPECTED}，实际 ${ACTUAL}）"
info "sha256 校验通过"

tar -xzf "${TMP_DIR}/${ASSET}" -C "$TMP_DIR"
[ -f "${TMP_DIR}/vless-small" ] || die "压缩包中缺少 vless-small"

# 生成 systemd 服务用户。useradd 不可用或创建失败时退回 nobody。
ensure_service_user() {
  SERVICE_USER="${VLESS_SMALL_SERVICE_USER:-vless-small}"

  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    return 0
  fi

  if command -v useradd >/dev/null 2>&1; then
    NOLOGIN="$(command -v nologin || true)"
    [ -n "$NOLOGIN" ] || NOLOGIN="/usr/sbin/nologin"
    # 优先使用长参数（systemd 发行版），失败再尝试 busybox 兼容写法。
    useradd --system --no-create-home --home-dir /nonexistent --shell "$NOLOGIN" "$SERVICE_USER" 2>/dev/null \
      || useradd -r -M -d /nonexistent -s "$NOLOGIN" "$SERVICE_USER" 2>/dev/null \
      || true
  fi

  if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
    warn "无法创建用户 ${SERVICE_USER}，改用 nobody"
    SERVICE_USER="nobody"
  fi

  id -u "$SERVICE_USER" >/dev/null 2>&1 || die "系统中没有可用的 nobody 用户"
}

# 配置写入 0600 的 EnvironmentFile：CONFIG 里含主控 token，不能全局可读。
write_env_file() {
  install -d -m 0700 "$ENV_DIR"

  local tmp
  tmp="$(mktemp "${TMPDIR:-/tmp}/vless-small-env.XXXXXX")"
  {
    echo "# 由 install.sh 生成于 $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    if [ -n "$EFFECTIVE_CONFIG" ]; then echo "CONFIG=${EFFECTIVE_CONFIG}"; fi
    if [ -n "$EFFECTIVE_UUID" ]; then echo "UUID=${EFFECTIVE_UUID}"; fi
    echo "PORT=${EFFECTIVE_PORT}"
    if [ -n "$EFFECTIVE_WS_PATH" ]; then echo "WS_PATH=${EFFECTIVE_WS_PATH}"; fi
    if [ -n "$EFFECTIVE_PUBLIC_HOSTS" ]; then echo "PUBLIC_HOSTS=${EFFECTIVE_PUBLIC_HOSTS}"; fi
    if [ -n "$EFFECTIVE_NODE_RUNTIME" ]; then echo "NODE_RUNTIME=${EFFECTIVE_NODE_RUNTIME}"; fi
  } >"$tmp"

  install -m 0600 "$tmp" "$ENV_FILE"
  rm -f "$tmp"
}

write_unit_file() {
  cat >"$UNIT_FILE" <<EOF
[Unit]
Description=vless-small (VLESS over WebSocket)
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
EnvironmentFile=${ENV_FILE}
ExecStart=${BIN_PATH}
Restart=always
RestartSec=3
TimeoutStopSec=15
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
# 自更新需要在安装目录里写临时文件并 rename，必须显式放开。
ReadWritePaths=${INSTALL_DIR}
# read-only 而非 true：允许从 /home 下的自定义安装目录启动。
ProtectHome=read-only

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 "$UNIT_FILE"
}

# 服务模式下先确定服务用户：安装目录和二进制都要 chown 给它，节点自更新
# （下载后 rename 自身）才有写权限。
if [ "$USE_SERVICE" -eq 1 ]; then
  ensure_service_user
  systemctl stop "$SERVICE_NAME" 2>/dev/null || true
fi

install -d -m 0755 "$INSTALL_DIR" 2>/dev/null \
  || die "无法创建目录 ${INSTALL_DIR}；请检查权限或用 --install-dir 指定"

if [ "$USE_SERVICE" -eq 1 ]; then
  chown "$SERVICE_USER" "$INSTALL_DIR" \
    || die "无法将 ${INSTALL_DIR} 交给服务用户 ${SERVICE_USER}"
fi

# 先写到同目录临时文件再 rename，避免覆盖正在运行的二进制时出现 ETXTBSY，
# 同时保证安装过程是原子的（不会留下写了一半的文件）。
STAGED_BIN="${INSTALL_DIR}/.vless-small.$$"
install -m 0755 "${TMP_DIR}/vless-small" "$STAGED_BIN" 2>/dev/null \
  || die "无法写入 ${INSTALL_DIR}；请检查权限（可能需要 sudo）"
mv -f "$STAGED_BIN" "$BIN_PATH" \
  || die "无法替换 ${BIN_PATH}；请检查权限"

if [ "$USE_SERVICE" -eq 1 ]; then
  chown "$SERVICE_USER" "$BIN_PATH" \
    || die "无法将 ${BIN_PATH} 交给服务用户 ${SERVICE_USER}"
fi

# 保留 /usr/local/bin/vless-small 作为命令行入口；已有旧文件时 -f 会替换成
# 指向新目录的符号链接。
if [ -n "$CLI_LINK" ]; then
  ln -sf "$BIN_PATH" "$CLI_LINK" \
    || warn "无法创建符号链接 ${CLI_LINK}"
fi

INSTALLED_VERSION="$("$BIN_PATH" -version 2>/dev/null || true)"

if [ "$USE_SERVICE" -eq 1 ]; then
  ensure_service_user

  if [ "$CONFIG_CHANGED" -eq 0 ] && [ -f "$ENV_FILE" ]; then
    info "保留已有配置 ${ENV_FILE}"
  else
    write_env_file
    info "已写入配置 ${ENV_FILE}"
  fi

  write_unit_file
  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || warn "systemctl enable 失败"
  if ! systemctl restart "$SERVICE_NAME"; then
    journalctl -u "$SERVICE_NAME" -n 30 --no-pager 2>/dev/null || true
    die "服务启动失败，请检查 ${ENV_FILE} 与 journalctl -u ${SERVICE_NAME}"
  fi

  # 健康检查能同时验证端口监听和 CONFIG 是否合法（非法配置会让进程退出）。
  healthy=0
  attempt=0
  while [ "$attempt" -lt 10 ]; do
    if curl -fsS "http://127.0.0.1:${EFFECTIVE_PORT}/healthz" >/dev/null 2>&1; then
      healthy=1
      break
    fi
    attempt=$((attempt + 1))
    sleep 1
  done

  if [ "$healthy" -ne 1 ]; then
    journalctl -u "$SERVICE_NAME" -n 30 --no-pager 2>/dev/null || true
    die "服务未通过健康检查（http://127.0.0.1:${EFFECTIVE_PORT}/healthz）"
  fi

  info "安装完成：${VERSION}"
  echo "  二进制: ${BIN_PATH}"
  echo "  配置:   ${ENV_FILE}"
  echo "  状态:   systemctl status ${SERVICE_NAME}"
  echo "  日志:   journalctl -u ${SERVICE_NAME} -f"
  echo
  echo "别忘了在反向代理上配置 TLS 与 WebSocket Upgrade；订阅默认走 443/ws。"
  exit 0
fi

# --no-service 但机器上已有同名服务时，新二进制要重启服务才会生效。
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
  warn "检测到 ${SERVICE_NAME} 服务仍在运行，请执行 systemctl restart ${SERVICE_NAME} 使用新版本"
fi

info "安装完成：${VERSION}"
echo "  二进制: ${BIN_PATH}"
if [ -n "$INSTALLED_VERSION" ]; then
  echo "  版本:   ${INSTALLED_VERSION}"
fi
if [ -n "${EFFECTIVE_CONFIG}" ] || [ -n "${EFFECTIVE_UUID}" ]; then
  echo
  echo "未注册 systemd 服务。手动运行："
  if [ -n "${EFFECTIVE_CONFIG}" ]; then echo "  export CONFIG='${EFFECTIVE_CONFIG}'"; fi
  if [ -n "${EFFECTIVE_UUID}" ]; then echo "  export UUID='${EFFECTIVE_UUID}'"; fi
  echo "  export PORT=${EFFECTIVE_PORT}"
  if [ -n "${EFFECTIVE_WS_PATH}" ]; then echo "  export WS_PATH='${EFFECTIVE_WS_PATH}'"; fi
  if [ -n "${EFFECTIVE_PUBLIC_HOSTS}" ]; then echo "  export PUBLIC_HOSTS='${EFFECTIVE_PUBLIC_HOSTS}'"; fi
  if [ -n "${EFFECTIVE_NODE_RUNTIME}" ]; then echo "  export NODE_RUNTIME='${EFFECTIVE_NODE_RUNTIME}'"; fi
  echo "  ${BIN_PATH}"
fi

case ":${PATH:-}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) echo "提示: 将 ${INSTALL_DIR} 加入 PATH 后可直接运行 vless-small" ;;
esac
