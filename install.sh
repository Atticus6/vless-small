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
# systemd（或 Alpine/OpenRC）环境下注册系统服务。重复执行即可升级，未传配置
# 参数时会保留已有的 /etc/vless-small/vless-small.env。
#
# 服务安装默认使用 /opt/vless-small（属主为服务用户），这样 Web 下发的
# 自更新才能替换二进制；已有旧节点请重跑一次本脚本完成迁移。
#
# 依赖: curl、tar、sha256sum 或 shasum；注册服务时额外依赖 systemctl
# （systemd）或 rc-service + openrc-run（Alpine/OpenRC）。脚本本身兼容
# busybox ash，Alpine 无需额外安装 bash。

# 脚本以 POSIX sh 编写，bash 与 Alpine 的 busybox ash 都能执行。
# pipefail 在老版本 ash 上不可用，检测到才开启，避免直接失败。
set -eu
if (set -o pipefail) 2>/dev/null; then
  set -o pipefail
fi

# 统一的可移植文件操作：Alpine 的 busybox 没有 GNU install(1)，这里用
# mkdir/chmod/cp 组合替代，语义足够（不保留属主/时间戳）。
ensure_dir() {
  local dir="$1"
  local mode="$2"
  mkdir -p "$dir" || return 1
  chmod "$mode" "$dir" || return 1
}

install_file() {
  local src="$1"
  local dst="$2"
  local mode="$3"
  # 先删目标再复制，不用 cp -f：目标可能位于服务用户可写的目录里，若那里
  # 已被放置指向 /etc/passwd 的符号链接，cp -f 会跟随链接以 root 覆盖系统文件。
  rm -f "$dst" || return 1
  # noclobber 语义：目标已不存在，用 > 创建可避免再次跟随链接（O_NOFOLLOW
  # 在 busybox cp 上不可靠，先删后建是这里最稳的写法）。
  cp "$src" "$dst" || return 1
  chmod "$mode" "$dst" || return 1
}

DEFAULT_REPO="Atticus6/vless-small"
SERVICE_NAME="vless-small"
ENV_DIR="/etc/vless-small"
ENV_FILE="${ENV_DIR}/vless-small.env"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
# OpenRC（Alpine）的 init 脚本；与 systemd unit 互斥，按检测到的 init 系统选用。
OPENRC_SCRIPT="/etc/init.d/${SERVICE_NAME}"
OPENRC_CONFD="/etc/conf.d/${SERVICE_NAME}"
# supervise-daemon 的日志文件；安装阶段由 root 预建并交给服务用户。
OPENRC_LOG="/var/log/${SERVICE_NAME}.log"

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
                        非 root 默认 ~/.local/bin；root 安装会在 /usr/local/bin 建符号链接。
                        服务模式下目录必须在 /opt、/srv、/var/lib 或 /usr/local/lib 下
  --service             强制注册系统服务（需要 root + systemd 或 OpenRC）
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

  # 两套 init 都尝试清理：升级/迁移过程中可能残留另一套的注册。
  if [ "$IS_ROOT" -eq 1 ]; then
    if command -v systemctl >/dev/null 2>&1; then
      systemctl stop "$SERVICE_NAME" 2>/dev/null || true
      systemctl disable "$SERVICE_NAME" 2>/dev/null || true
      if [ -f "$UNIT_FILE" ]; then
        rm -f "$UNIT_FILE"
        systemctl daemon-reload 2>/dev/null || true
        info "已停止并移除 systemd 服务 ${SERVICE_NAME}"
      fi
    fi
    if command -v rc-update >/dev/null 2>&1 && command -v rc-service >/dev/null 2>&1; then
      rc-service "$SERVICE_NAME" stop >/dev/null 2>&1 || true
      rc-update del "$SERVICE_NAME" default >/dev/null 2>&1 || true
      if [ -f "$OPENRC_SCRIPT" ]; then
        rm -f "$OPENRC_SCRIPT" "$OPENRC_CONFD"
        info "已停止并移除 OpenRC 服务 ${SERVICE_NAME}"
      fi
    fi
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
  if [ "$IS_ROOT" -ne 1 ] && { [ -e "$UNIT_FILE" ] || [ -e "$OPENRC_SCRIPT" ] || [ -e "$ENV_FILE" ]; }; then
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

# OpenRC（Alpine、Gentoo 等）：没有 systemd，用 rc-service 管理服务。
# 两者互斥——systemd 发行版上通常也装了 openrc 兼容层，但不应走这条路。
HAS_OPENRC=0
if [ "$OS" = "linux" ] && [ "$HAS_SYSTEMD" -eq 0 ] && [ -d /run/openrc ] \
  && command -v rc-service >/dev/null 2>&1 \
  && command -v rc-update >/dev/null 2>&1; then
  HAS_OPENRC=1
fi

# INIT_SYSTEM 决定后面走哪套服务管理逻辑；空表示只装二进制。
INIT_SYSTEM=""
if [ "$HAS_SYSTEMD" -eq 1 ]; then
  INIT_SYSTEM="systemd"
elif [ "$HAS_OPENRC" -eq 1 ]; then
  INIT_SYSTEM="openrc"
fi

USE_SERVICE=0
case "$SERVICE_MODE" in
  yes)
    [ "$IS_ROOT" -eq 1 ] || die "--service 需要 root 权限"
    [ -n "$INIT_SYSTEM" ] \
      || die "--service 需要 systemd 或 OpenRC（未检测到可用的服务管理器）"
    USE_SERVICE=1
    ;;
  no)
    USE_SERVICE=0
    ;;
  auto)
    if [ "$IS_ROOT" -eq 1 ] && [ -n "$INIT_SYSTEM" ]; then
      USE_SERVICE=1
    fi
    ;;
esac

# 自更新需要在安装目录里 rename 自身，所以服务模式下目录必须归服务用户所有。
# 但把系统目录 chown 给服务用户，等于允许它替换目录里的任何命令（目录写
# 权限控制条目增删，与目标文件属主无关）。因此只允许“专用目录”：必须在
# /opt、/srv、/var/lib、/usr/local/lib 等非系统 bin 路径下。
#
# 用白名单而不是黑名单：`/`、`/usr`、`/usr/local` 这些父目录同样危险，枚举
# 黑名单容易漏。规范化必须成功——realpath 缺失或失败时若退回原字符串，
# `/opt/../usr/local/bin` 会通过 /opt/* 白名单，把系统目录交给服务用户。
if [ "$USE_SERVICE" -eq 1 ]; then
  NORMALIZED_INSTALL_DIR="$(realpath -m "$INSTALL_DIR" 2>/dev/null)" || NORMALIZED_INSTALL_DIR=""
  if [ -z "$NORMALIZED_INSTALL_DIR" ]; then
    # busybox 的 realpath 可能不支持 -m；退一步用只做词法规范化的实现。
    NORMALIZED_INSTALL_DIR="$(cd "$INSTALL_DIR" 2>/dev/null && pwd -P)" || NORMALIZED_INSTALL_DIR=""
  fi
  [ -n "$NORMALIZED_INSTALL_DIR" ] \
    || die "无法规范化安装目录 ${INSTALL_DIR}；请使用绝对路径并确认目录可访问"
  NORMALIZED_INSTALL_DIR="${NORMALIZED_INSTALL_DIR%/}"
  [ -n "$NORMALIZED_INSTALL_DIR" ] || NORMALIZED_INSTALL_DIR="/"
  case "$NORMALIZED_INSTALL_DIR" in
    /opt/*|/srv/*|/var/lib/*|/usr/local/lib/*|/usr/local/opt/*) ;;
    *)
      die "服务模式下安装目录必须在 /opt、/srv、/var/lib 或 /usr/local/lib 下（当前 ${INSTALL_DIR}）；把系统目录交给服务用户会危及同目录的其他命令"
      ;;
  esac
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
# 用独立标志区分“显式代理”和“默认 GitHub 地址”。默认值填进去之后
# DOWNLOAD_BASE 一定非空，不能再拿它判断模式，否则直连安装也会走代理分支。
PROXY_MODE=0
if [ -n "$DOWNLOAD_BASE" ]; then
  PROXY_MODE=1
else
  DOWNLOAD_BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi
DOWNLOAD_BASE="${DOWNLOAD_BASE%/}"

# Web 代理模式：用 CONFIG 里的节点 name+token 鉴权，不再需要 GitHub Token。
PROXY_NAME=""
PROXY_TOKEN=""
if [ "$PROXY_MODE" -eq 1 ] && [ -n "${EFFECTIVE_CONFIG:-}" ]; then
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

  if [ "$PROXY_MODE" -eq 1 ]; then
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

# 生成服务用户。useradd 不可用或创建失败时退回 nobody。
#
# Alpine 的 busybox 没有 useradd：优先用 adduser（Alpine 提供），再退回
# useradd。两种实现都创建系统账户、无家目录、禁止登录。
ensure_service_user() {
  SERVICE_USER="${VLESS_SMALL_SERVICE_USER:-vless-small}"

  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    return 0
  fi

  NOLOGIN="$(command -v nologin || true)"
  [ -n "$NOLOGIN" ] || NOLOGIN="/sbin/nologin"

  if command -v adduser >/dev/null 2>&1; then
    # Alpine/busybox adduser：-S 系统用户，-H 不建家目录，-s 登录 shell。
    adduser -S -H -h /nonexistent -s "$NOLOGIN" "$SERVICE_USER" 2>/dev/null || true
  fi
  if ! id -u "$SERVICE_USER" >/dev/null 2>&1 && command -v useradd >/dev/null 2>&1; then
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
  ensure_dir "$ENV_DIR" 0700

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

  install_file "$tmp" "$ENV_FILE" 0600
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
# 自动证书需要服务用户绑定 80/443；只授予低端口绑定能力。
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
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

# 写 OpenRC 配置与服务脚本。
#
# OpenRC 的 init 脚本由 openrc-run 解释，语法与 bash 不同；这里只声明依赖与
# 描述，实际参数放在 /etc/conf.d。
# - 服务参数走 /etc/conf.d，权限 0600；
# - 环境变量由 init 脚本的 start_pre 从 systemd 共用的 env 文件读入，
#   避免两套配置格式漂移（openrc-run 的 env_file 在旧版本不可用）；
# - supervisor=supervise-daemon 提供崩溃重启（等价 systemd 的 Restart=always），
#   同时托管 pidfile，不会因进程自己 fork 而丢失跟踪；
# - 端口 <1024 需要 root 或 cap_net_bind_service，这里用 capabilities 声明。
write_openrc_files() {
  ensure_dir /etc/conf.d 0755

  local tmp
  tmp="$(mktemp "${TMPDIR:-/tmp}/vless-small-conf.XXXXXX")"
  {
    echo "# 由 install.sh 生成于 $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    echo "command=\"${BIN_PATH}\""
    echo "command_user=\"${SERVICE_USER}\""
    echo "command_background=\"true\""
    echo "supervisor=\"supervise-daemon\""
    echo "respawn_delay=3"
    echo "respawn_max=0"
    echo "pidfile=\"/run/${SERVICE_NAME}.pid\""
    # supervise-daemon 直接把 stdout/stderr 写文件；日志会随重启累积，
    # 需要长期保留时由管理员自行配置 logrotate。
    echo "output_log=\"${OPENRC_LOG}\""
    echo "error_log=\"${OPENRC_LOG}\""
    # cap_iab 格式：裸写只进 inheritable，降权后仍绑不了低端口；`^` 才是
    # ambient，能随 exec 传给非 root 进程。
    echo "capabilities=\"^cap_net_bind_service\""
  } >"$tmp"
  install_file "$tmp" "$OPENRC_CONFD" 0600
  rm -f "$tmp"

  cat >"$OPENRC_SCRIPT" <<EOF
#!/sbin/openrc-run
# 由 vless-small install.sh 生成；服务参数在 /etc/conf.d/${SERVICE_NAME}。
#
# 环境变量从 ${ENV_FILE} 按数据读取，不 source：配置文件由管理员可写，
# 把它当 shell 执行等于以 root 运行任意命令（例如 WS_PATH 里的分号）。
description="vless-small (VLESS over WebSocket)"

start_pre() {
	# supervise-daemon 先降权再打开日志文件，普通服务用户无法在 root 拥有的
	# /var/log 里创建它，会在启动前就失败。这里以 root 预建并交给服务用户。
	for log in "${OPENRC_LOG}"; do
		[ -n "\$log" ] || continue
		if [ ! -e "\$log" ]; then
			: >"\$log" || return 1
		fi
		chown "${SERVICE_USER}" "\$log" 2>/dev/null || true
	done

	if [ -r "${ENV_FILE}" ]; then
		while IFS= read -r line || [ -n "\$line" ]; do
			case "\$line" in
				''|'#'*) continue ;;
			esac
			# 只接受 NAME=VALUE，按第一个等号拆分，值原样导出。
			key="\${line%%=*}"
			[ "\$key" != "\$line" ] || continue
			value="\${line#*=}"
			case "\$key" in
				*[!A-Za-z0-9_]*) continue ;;
			esac
			export "\$key=\$value"
		done <"${ENV_FILE}"
	fi
}

depend() {
	need net
	after firewall
}
EOF
  chmod 0755 "$OPENRC_SCRIPT"
}

# 按检测到的 init 系统重启服务并等待健康检查。
restart_service() {
  case "$INIT_SYSTEM" in
    systemd)
      systemctl daemon-reload
      systemctl enable "$SERVICE_NAME" >/dev/null 2>&1 || warn "systemctl enable 失败"
      systemctl restart "$SERVICE_NAME"
      ;;
    openrc)
      rc-update add "$SERVICE_NAME" default >/dev/null 2>&1 \
        || warn "rc-update add 失败"
      # restart 在服务未运行时等价于 start；Alpine 上没有 systemd 的
      # daemon-reload，脚本内容变更后 restart 会重新读取。
      rc-service "$SERVICE_NAME" restart >/dev/null 2>&1 \
        || rc-service "$SERVICE_NAME" start
      ;;
    *)
      return 1
      ;;
  esac
}

# 输出服务日志尾部，供启动失败时排查。
dump_service_log() {
  case "$INIT_SYSTEM" in
    systemd) journalctl -u "$SERVICE_NAME" -n 30 --no-pager 2>/dev/null || true ;;
    openrc) tail -n 30 "/var/log/${SERVICE_NAME}.log" 2>/dev/null || true ;;
  esac
}

# 服务模式下先确定服务用户：安装目录和二进制都要 chown 给它，节点自更新
# （下载后 rename 自身）才有写权限。安装前先停旧服务，避免覆盖运行中的二进制。
if [ "$USE_SERVICE" -eq 1 ]; then
  ensure_service_user
  case "$INIT_SYSTEM" in
    systemd) systemctl stop "$SERVICE_NAME" 2>/dev/null || true ;;
    openrc) rc-service "$SERVICE_NAME" stop >/dev/null 2>&1 || true ;;
  esac
fi

ensure_dir "$INSTALL_DIR" 0755 2>/dev/null \
  || die "无法创建目录 ${INSTALL_DIR}；请检查权限或用 --install-dir 指定"

if [ "$USE_SERVICE" -eq 1 ]; then
  chown "$SERVICE_USER" "$INSTALL_DIR" \
    || die "无法将 ${INSTALL_DIR} 交给服务用户 ${SERVICE_USER}"
fi

# 先写到同目录临时文件再 rename，避免覆盖正在运行的二进制时出现 ETXTBSY，
# 同时保证安装过程是原子的（不会留下写了一半的文件）。
#
# 暂存文件用 mktemp 而不是固定名字：安装目录属于服务用户，固定名字
# （.vless-small.$$）可以被它预置成符号链接，让 root 的写入落到别处。
STAGED_BIN="$(mktemp "${INSTALL_DIR}/.vless-small.XXXXXX" 2>/dev/null)" \
  || die "无法在 ${INSTALL_DIR} 创建临时文件；请检查权限（可能需要 sudo）"
install_file "${TMP_DIR}/vless-small" "$STAGED_BIN" 0755 2>/dev/null \
  || die "无法写入 ${INSTALL_DIR}；请检查权限（可能需要 sudo）"
# 同样先删目标再 mv：BIN_PATH 可能被替换成符号链接，mv -f 会跟随它。
rm -f "$BIN_PATH"
mv "$STAGED_BIN" "$BIN_PATH" \
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

# 服务模式下二进制已 chown 给服务用户，自更新后其内容由该用户控制。
# root 再执行它（例如 -version 探测）等于执行服务用户可替换的代码，属于
# 提权风险。这里不做“检查属主再执行”的 TOCTOU 判断：服务用户可以先让
# stat 失败、再替换文件，检查与执行之间存在竞态。服务模式直接跳过执行，
# 版本信息由服务启动后的上报提供。
if [ "$USE_SERVICE" -eq 0 ]; then
  INSTALLED_VERSION="$("$BIN_PATH" -version 2>/dev/null || true)"
else
  INSTALLED_VERSION=""
fi

if [ "$USE_SERVICE" -eq 1 ]; then
  ensure_service_user

  if [ "$CONFIG_CHANGED" -eq 0 ] && [ -f "$ENV_FILE" ]; then
    info "保留已有配置 ${ENV_FILE}"
  else
    write_env_file
    info "已写入配置 ${ENV_FILE}"
  fi

  case "$INIT_SYSTEM" in
    systemd) write_unit_file ;;
    openrc) write_openrc_files ;;
  esac

  if ! restart_service; then
    dump_service_log
    die "服务启动失败，请检查 ${ENV_FILE} 与服务日志"
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
    dump_service_log
    die "服务未通过健康检查（http://127.0.0.1:${EFFECTIVE_PORT}/healthz）"
  fi

  info "安装完成：${VERSION}"
  echo "  二进制: ${BIN_PATH}"
  echo "  配置:   ${ENV_FILE}"
  case "$INIT_SYSTEM" in
    systemd)
      echo "  状态:   systemctl status ${SERVICE_NAME}"
      echo "  日志:   journalctl -u ${SERVICE_NAME} -f"
      ;;
    openrc)
      echo "  状态:   rc-service ${SERVICE_NAME} status"
      echo "  日志:   tail -f /var/log/${SERVICE_NAME}.log"
      ;;
  esac
  echo
  echo "别忘了在反向代理上配置 TLS 与 WebSocket Upgrade；订阅默认走 443/ws。"
  exit 0
fi

# --no-service 但机器上已有同名服务时，新二进制要重启服务才会生效。
if [ -n "$INIT_SYSTEM" ]; then
  case "$INIT_SYSTEM" in
    systemd)
      if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        warn "检测到 ${SERVICE_NAME} 服务仍在运行，请执行 systemctl restart ${SERVICE_NAME} 使用新版本"
      fi
      ;;
    openrc)
      if rc-service "$SERVICE_NAME" status >/dev/null 2>&1; then
        warn "检测到 ${SERVICE_NAME} 服务仍在运行，请执行 rc-service ${SERVICE_NAME} restart 使用新版本"
      fi
      ;;
  esac
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
