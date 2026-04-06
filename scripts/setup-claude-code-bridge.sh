#!/usr/bin/env bash
# setup-claude-code-bridge.sh — 一键开启 Claude Code 的 /fast 兼容 + bridge 远程自动压缩
#
# 功能：
#   1. native binary 检测：tengu_marble_sandcastle 后的 !v9()) → !1)
#   2. penguin mode 检测：r16.status==="disabled" → !1  绕过组织级别禁用
#   3. 可选：登录 cdx.cc bridge 管理面板，开启自动远程压缩
#
# 原因：penguin mode 请求硬编码到 api.anthropic.com，不走 ANTHROPIC_BASE_URL
# 远程压缩不替换 Claude Code 自带 /compact；它是 bridge 侧的自动压缩能力。
#
# 用法：
#   bash scripts/setup-claude-code-bridge.sh
#   bash scripts/setup-claude-code-bridge.sh /path/to/cli.js
#   bash scripts/setup-claude-code-bridge.sh --bridge http://localhost:8787 --admin-password 'xxxx'
#   bash scripts/setup-claude-code-bridge.sh --compact-mode responses_compact --compact-threshold 180000
#   bash scripts/setup-claude-code-bridge.sh --restore

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_ok()   { printf "${GREEN}[OK]${NC} %s\n" "$*"; }
log_warn() { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }
log_err()  { printf "${RED}[ERR]${NC} %s\n" "$*"; }

SCRIPT_NAME="$(basename "$0")"

usage() {
    cat <<'EOF'
用法:
  bash scripts/setup-claude-code-bridge.sh [cli.js路径] [选项]

选项:
  --bridge URL               bridge 管理面板地址（默认: $ANTHROPIC_BASE_URL 或 http://localhost:8787）
  --admin-password PASS      管理面板密码；不传则在交互模式下提示输入
  --compact-mode MODE        off | context_management | responses_compact（默认: responses_compact）
  --compact-threshold N      自动压缩阈值 tokens（默认: 180000）
  --skip-fast                只配置远程自动压缩，不 patch /fast
  --skip-compact             只 patch /fast，不配置远程自动压缩
  --restore                  恢复 cli.js 备份；若同时提供 bridge 管理信息，则关闭 bridge 自动压缩
  --help                     显示帮助

说明:
  - /fast patch 针对 Claude Code 已安装的 cli.js
  - 远程压缩配置的是 bridge 侧自动压缩，不会覆盖 Claude Code 自带 /compact 命令
EOF
}

# 跨平台 sed -i（macOS BSD sed 需要 -i ''）
sedi() {
    if [[ "$OSTYPE" == darwin* ]]; then
        sed -i '' "$@"
    else
        sed -i "$@"
    fi
}

# 跨平台解析符号链接（macOS 无 readlink -f）
resolve_link() {
    local target="$1"
    while [[ -L "$target" ]]; do
        local dir
        dir="$(cd "$(dirname "$target")" && pwd)"
        target="$(readlink "$target")"
        [[ "$target" != /* ]] && target="$dir/$target"
    done
    echo "$target"
}

# 查找 cli.js 路径
find_cli_js() {
    local candidates=()

    # 0. 通过 which claude 反查（最可靠）
    if command -v claude &>/dev/null; then
        local claude_bin
        claude_bin="$(resolve_link "$(command -v claude)")"
        local bin_dir
        bin_dir="$(dirname "$claude_bin")"
        candidates+=(
            "$bin_dir/node_modules/@anthropic-ai/claude-code/cli.js"
            "$bin_dir/../lib/node_modules/@anthropic-ai/claude-code/cli.js"
        )
    fi

    # 1. npm 全局安装路径
    if command -v npm &>/dev/null; then
        local npm_root npm_prefix
        npm_root="$(npm root -g 2>/dev/null)" || true
        npm_prefix="$(npm prefix -g 2>/dev/null)" || true
        [[ -n "$npm_root" ]] && candidates+=("$npm_root/@anthropic-ai/claude-code/cli.js")
        [[ -n "$npm_prefix" ]] && candidates+=("$npm_prefix/node_modules/@anthropic-ai/claude-code/cli.js")
    fi

    # 2. 常见 Windows 路径（MSYS/Git Bash）
    for prefix in "${APPDATA:-}/npm" "${LOCALAPPDATA:-}/npm" "/c/Users/${USER:-${USERNAME:-}}/AppData/Roaming/npm"; do
        [[ -d "$prefix" ]] && candidates+=("$prefix/node_modules/@anthropic-ai/claude-code/cli.js")
    done

    # 3. 常见 Unix / macOS 路径
    candidates+=(
        "/usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js"
        "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"
        "/opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js"
        "${HOME:+$HOME/.npm-global/lib/node_modules/@anthropic-ai/claude-code/cli.js}"
    )

    # 4. pnpm 全局路径
    if command -v pnpm &>/dev/null; then
        local pnpm_root
        pnpm_root="$(pnpm root -g 2>/dev/null)" || true
        [[ -n "$pnpm_root" ]] && candidates+=("$pnpm_root/@anthropic-ai/claude-code/cli.js")
    fi

    for path in "${candidates[@]}"; do
        if [[ -f "$path" ]]; then
            echo "$path"
            return 0
        fi
    done

    return 1
}

require_cmd() {
    if ! command -v "$1" &>/dev/null; then
        log_err "缺少命令: $1"
        exit 1
    fi
}

normalize_url() {
    local url="$1"
    url="${url%/}"
    printf '%s' "$url"
}

prompt_admin_password_if_needed() {
    if [[ -n "${ADMIN_PASSWORD:-}" ]]; then
        return 0
    fi
    if [[ ! -t 0 ]]; then
        log_err "未提供 --admin-password，且当前不是交互终端"
        return 1
    fi
    printf "请输入 bridge 管理密码: " >&2
    read -r -s ADMIN_PASSWORD
    printf "\n" >&2
    [[ -n "$ADMIN_PASSWORD" ]]
}

configure_bridge_auto_compact() {
    require_cmd curl
    local desired_mode="$1"
    local desired_threshold="$2"

    if [[ -z "${BRIDGE_URL:-}" ]]; then
        log_err "bridge URL 为空"
        return 1
    fi
    if ! prompt_admin_password_if_needed; then
        return 1
    fi

    local cookie_jar
    cookie_jar="$(mktemp)"
    trap 'rm -f "$cookie_jar"' RETURN

    local login_url="${BRIDGE_URL}/admin/login"
    curl -fsS -L \
        -c "$cookie_jar" \
        -b "$cookie_jar" \
        -H 'Content-Type: application/x-www-form-urlencoded' \
        --data-urlencode "token=${ADMIN_PASSWORD}" \
        "$login_url" >/dev/null

    local payload
    payload="$(printf '{"auto_compact":{"mode":"%s","threshold_tokens":%s}}' "$desired_mode" "$desired_threshold")"

    local http_code
    http_code="$(
        curl -sS -o /dev/null -w '%{http_code}' \
            -X PUT \
            -b "$cookie_jar" \
            -H 'Content-Type: application/json' \
            --data "$payload" \
            "${BRIDGE_URL}/admin/api/config"
    )"

    if [[ "$http_code" != "200" ]]; then
        log_err "bridge 自动压缩配置失败，HTTP ${http_code}"
        return 1
    fi

    if [[ "$desired_mode" == "off" ]]; then
        log_ok "bridge 自动压缩已关闭"
    else
        log_ok "bridge 自动压缩已开启: mode=${desired_mode}, threshold=${desired_threshold}"
        log_ok "说明：这是 bridge 侧自动远程压缩，和 Claude Code 自带 /compact 分离"
    fi
}

verify_fast_patch() {
    if [[ "$SKIP_FAST" -eq 1 ]]; then
        return 0
    fi
    local ok=0
    if grep -q 'tengu_marble_sandcastle",!0)&&!1)' "$CLI_JS" 2>/dev/null; then
        log_ok "自检: native binary /fast patch 已生效"
        ok=$((ok + 1))
    else
        log_warn "自检: 未检测到 native binary /fast patch"
    fi
    if ! grep -q 'r16.status==="disabled"' "$CLI_JS" 2>/dev/null; then
        log_ok "自检: penguin mode 组织检测 patch 已生效"
        ok=$((ok + 1))
    else
        log_warn "自检: 未检测到 penguin mode 组织检测 patch"
    fi
    [[ "$ok" -eq 2 ]]
}

verify_bridge_state() {
    if [[ "$SKIP_COMPACT" -eq 1 ]]; then
        return 0
    fi
    require_cmd curl
    local cookie_jar
    cookie_jar="$(mktemp)"
    trap 'rm -f "$cookie_jar"' RETURN

    local fast_json
    fast_json="$(curl -fsS "${BRIDGE_URL}/api/claude_code_penguin_mode" 2>/dev/null || true)"
    if [[ "$fast_json" == *'"enabled":true'* ]]; then
        log_ok "自检: bridge /api/claude_code_penguin_mode 返回 enabled=true"
    else
        log_warn "自检: bridge /api/claude_code_penguin_mode 未返回 enabled=true"
    fi

    if ! prompt_admin_password_if_needed; then
        log_warn "自检: 缺少管理密码，跳过 /admin 配置校验"
        return 1
    fi

    curl -fsS -L \
        -c "$cookie_jar" \
        -b "$cookie_jar" \
        -H 'Content-Type: application/x-www-form-urlencoded' \
        --data-urlencode "token=${ADMIN_PASSWORD}" \
        "${BRIDGE_URL}/admin/login" >/dev/null

    local config_json
    config_json="$(curl -fsS -b "$cookie_jar" "${BRIDGE_URL}/admin/api/config" 2>/dev/null || true)"
    if [[ -z "$config_json" ]]; then
        log_warn "自检: 无法读取 bridge /admin/api/config"
        return 1
    fi

    if [[ "$COMPACT_MODE" == "off" ]]; then
        if [[ "$config_json" == *'"mode":"off"'* || "$config_json" != *'"auto_compact"'* ]]; then
            log_ok "自检: bridge 自动压缩当前为关闭状态"
            return 0
        fi
        log_warn "自检: bridge 自动压缩关闭状态校验失败"
        return 1
    fi

    if [[ "$config_json" == *"\"mode\":\"${COMPACT_MODE}\""* && "$config_json" == *"\"threshold_tokens\":${COMPACT_THRESHOLD}"* ]]; then
        log_ok "自检: bridge 自动压缩配置匹配 mode=${COMPACT_MODE}, threshold=${COMPACT_THRESHOLD}"
        return 0
    fi
    log_warn "自检: bridge 自动压缩配置与预期不一致"
    return 1
}

validate_compact_mode() {
    case "$1" in
        off|context_management|responses_compact) return 0 ;;
        *)
            log_err "无效的 --compact-mode: $1"
            log_err "支持: off | context_management | responses_compact"
            return 1
            ;;
    esac
}

CLI_JS=""
RESTORE=0
SKIP_FAST=0
SKIP_COMPACT=0
BRIDGE_URL="${CDX_BRIDGE_URL:-${ANTHROPIC_BASE_URL:-http://localhost:8787}}"
ADMIN_PASSWORD="${CDX_ADMIN_PASSWORD:-}"
COMPACT_MODE="${CDX_AUTO_COMPACT_MODE:-responses_compact}"
COMPACT_THRESHOLD="${CDX_AUTO_COMPACT_THRESHOLD:-180000}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --help|-h)
            usage
            exit 0
            ;;
        --restore)
            RESTORE=1
            shift
            ;;
        --skip-fast)
            SKIP_FAST=1
            shift
            ;;
        --skip-compact)
            SKIP_COMPACT=1
            shift
            ;;
        --bridge)
            BRIDGE_URL="${2:-}"
            shift 2
            ;;
        --admin-password)
            ADMIN_PASSWORD="${2:-}"
            shift 2
            ;;
        --compact-mode)
            COMPACT_MODE="${2:-}"
            shift 2
            ;;
        --compact-threshold)
            COMPACT_THRESHOLD="${2:-}"
            shift 2
            ;;
        --cli-js)
            CLI_JS="${2:-}"
            shift 2
            ;;
        --*)
            log_err "未知参数: $1"
            usage
            exit 1
            ;;
        *)
            if [[ -z "$CLI_JS" ]]; then
                CLI_JS="$1"
            else
                log_err "多余参数: $1"
                usage
                exit 1
            fi
            shift
            ;;
    esac
done

BRIDGE_URL="$(normalize_url "$BRIDGE_URL")"
validate_compact_mode "$COMPACT_MODE"
if [[ "$COMPACT_MODE" != "off" && ! "$COMPACT_THRESHOLD" =~ ^[0-9]+$ ]]; then
    log_err "--compact-threshold 必须是正整数"
    exit 1
fi
if [[ "$COMPACT_MODE" != "off" && "$COMPACT_THRESHOLD" -le 0 ]]; then
    log_err "--compact-threshold 必须大于 0"
    exit 1
fi

if [[ "$SKIP_FAST" -eq 1 && "$SKIP_COMPACT" -eq 1 ]]; then
    log_err "--skip-fast 和 --skip-compact 不能同时使用"
    exit 1
fi

if [[ "$RESTORE" -eq 1 ]]; then
    if [[ "$SKIP_FAST" -ne 1 ]]; then
        if [[ -z "$CLI_JS" ]]; then
            CLI_JS="$(find_cli_js)" || { log_err "找不到 cli.js"; exit 1; }
        fi
        BACKUP="${CLI_JS}.bak"
        if [[ ! -f "$BACKUP" ]]; then
            log_err "备份文件不存在: $BACKUP"
            exit 1
        fi
        cp "$BACKUP" "$CLI_JS"
        log_ok "已恢复: $CLI_JS"
    fi
    if [[ "$SKIP_COMPACT" -ne 1 ]]; then
        configure_bridge_auto_compact "off" "0" || log_warn "未能自动关闭 bridge 自动压缩，请手动在 /admin 调整"
    fi
    exit 0
fi

if [[ "$SKIP_FAST" -ne 1 ]]; then
    if [[ -z "$CLI_JS" ]]; then
        CLI_JS="$(find_cli_js)" || {
            log_err "找不到 Claude Code 安装位置"
            log_err "请手动指定: bash $0 /path/to/cli.js"
            exit 1
        }
    fi

    if [[ ! -f "$CLI_JS" ]]; then
        log_err "文件不存在: $CLI_JS"
        exit 1
    fi

    log_ok "找到 cli.js: $CLI_JS"
fi

PATCHES_NEEDED=0
PATCHES_DONE=0

if [[ "$SKIP_FAST" -ne 1 ]]; then
    # 检查 patch 1: native binary 检测（只检查 fast mode 那一处）
    if grep -q 'tengu_marble_sandcastle",!0)&&!v9())' "$CLI_JS" 2>/dev/null; then
        PATCHES_NEEDED=$((PATCHES_NEEDED + 1))
    elif grep -q 'tengu_marble_sandcastle",!0)&&!1)' "$CLI_JS" 2>/dev/null; then
        log_warn "Patch 1 (native binary) 已应用"
        PATCHES_DONE=$((PATCHES_DONE + 1))
    fi

    # 检查 patch 2: penguin mode 检测
    if grep -q 'r16.status==="disabled"' "$CLI_JS" 2>/dev/null; then
        PATCHES_NEEDED=$((PATCHES_NEEDED + 1))
    else
        log_warn "Patch 2 (penguin mode) 已应用"
        PATCHES_DONE=$((PATCHES_DONE + 1))
    fi

    if [[ $PATCHES_NEEDED -eq 0 ]]; then
        log_ok "所有 /fast patch 已应用，无需重复 patch"
    else
        # 备份
        BACKUP="${CLI_JS}.bak"
        if [[ ! -f "$BACKUP" ]]; then
            cp "$CLI_JS" "$BACKUP"
            log_ok "备份: $BACKUP"
        else
            log_warn "备份已存在，跳过备份"
        fi

        # Patch 1: native binary 检测（精确匹配 fast mode 检查，不动其他 v9() 调用）
        # tengu_marble_sandcastle flag 后的 !v9() 是 fast mode 专用检查
        if grep -q 'tengu_marble_sandcastle",!0)&&!v9())' "$CLI_JS" 2>/dev/null; then
            sedi 's/tengu_marble_sandcastle",!0)\&\&!v9())/tengu_marble_sandcastle",!0)\&\&!1)/g' "$CLI_JS"
            if grep -q 'tengu_marble_sandcastle",!0)&&!1)' "$CLI_JS" 2>/dev/null; then
                log_ok "Patch 1: native binary 检测已绕过"
                PATCHES_DONE=$((PATCHES_DONE + 1))
            else
                log_err "Patch 1 失败"
            fi
        fi

        # Patch 2: penguin mode 组织级别检测
        # penguin mode 请求硬编码到 api.anthropic.com，不走 bridge
        # r16.status==="disabled" → !1  使该检查永远为 false
        if grep -q 'r16.status==="disabled"' "$CLI_JS" 2>/dev/null; then
            sedi 's/r16.status==="disabled"/!1/g' "$CLI_JS"
            if ! grep -q 'r16.status==="disabled"' "$CLI_JS" 2>/dev/null; then
                log_ok "Patch 2: penguin mode 检测已绕过"
                PATCHES_DONE=$((PATCHES_DONE + 1))
            else
                log_err "Patch 2 失败"
            fi
        fi
    fi
fi

COMPACT_DONE=0
if [[ "$SKIP_COMPACT" -ne 1 ]]; then
    echo ""
    log_ok "开始配置 bridge 自动压缩"
    configure_bridge_auto_compact "$COMPACT_MODE" "$COMPACT_THRESHOLD"
    COMPACT_DONE=1
fi

echo ""
if [[ "$SKIP_FAST" -ne 1 ]]; then
    if [[ $PATCHES_DONE -ge 2 || $PATCHES_NEEDED -eq 0 ]]; then
        log_ok "/fast 兼容 patch 完成"
    else
        log_warn "/fast patch 可能未完全成功 ($PATCHES_DONE/2)"
        log_warn "请检查 cli.js 版本是否兼容"
    fi
fi
if [[ "$COMPACT_DONE" -eq 1 ]]; then
    log_ok "远程自动压缩配置完成"
fi

echo ""
log_ok "开始自检"
verify_fast_patch || true
verify_bridge_state || true

echo ""
echo "提示："
[[ "$SKIP_FAST" -ne 1 ]] && echo "  - 重启 Claude Code 后 /fast patch 生效"
[[ "$SKIP_COMPACT" -ne 1 ]] && echo "  - 远程压缩是 bridge 侧自动压缩，和 Claude Code 自带 /compact 分离"
echo "  - 推荐脚本: bash scripts/setup-claude-code-bridge.sh"
echo "  - 恢复命令: bash scripts/${SCRIPT_NAME} --restore"
echo "  - Claude Code 更新后需要重新运行此脚本"
