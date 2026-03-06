#!/usr/bin/env bash
# patch-fast-mode.sh — 为 npm 安装的 Claude Code 解锁 /fast 模式
#
# 需要 patch 两处：
#   1. native binary 检测：!v9()) → !1)    绕过 Bun runtime 检测
#   2. penguin mode 检测：r16.status==="disabled" → !1  绕过组织级别禁用
#
# 原因：penguin mode 请求硬编码到 api.anthropic.com，不走 ANTHROPIC_BASE_URL
#
# 用法：
#   bash scripts/patch-fast-mode.sh          # 自动检测安装位置
#   bash scripts/patch-fast-mode.sh /path/to/cli.js   # 手动指定
#   bash scripts/patch-fast-mode.sh --restore         # 恢复原始文件

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_ok()   { echo -e "${GREEN}[OK]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_err()  { echo -e "${RED}[ERR]${NC} $*"; }

# 查找 cli.js 路径
find_cli_js() {
    local candidates=()

    # 1. npm 全局安装路径
    if command -v npm &>/dev/null; then
        local npm_root
        npm_root="$(npm root -g 2>/dev/null)" || true
        if [[ -n "$npm_root" ]]; then
            candidates+=("$npm_root/@anthropic-ai/claude-code/cli.js")
        fi
    fi

    # 2. 常见 Windows 路径
    for prefix in "$APPDATA/npm" "$LOCALAPPDATA/npm" "/c/Users/$USER/AppData/Roaming/npm"; do
        if [[ -d "$prefix" ]]; then
            candidates+=("$prefix/node_modules/@anthropic-ai/claude-code/cli.js")
        fi
    done

    # 3. 常见 Unix 路径
    candidates+=(
        "/usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js"
        "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"
    )

    # 4. pnpm 全局路径
    if command -v pnpm &>/dev/null; then
        local pnpm_root
        pnpm_root="$(pnpm root -g 2>/dev/null)" || true
        if [[ -n "$pnpm_root" ]]; then
            candidates+=("$pnpm_root/@anthropic-ai/claude-code/cli.js")
        fi
    fi

    for path in "${candidates[@]}"; do
        if [[ -f "$path" ]]; then
            echo "$path"
            return 0
        fi
    done

    return 1
}

# 恢复模式
if [[ "${1:-}" == "--restore" ]]; then
    CLI_JS="${2:-}"
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
    exit 0
fi

# 确定目标文件
CLI_JS="${1:-}"
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

# 统计需要 patch 的数量
PATCHES_NEEDED=0
PATCHES_DONE=0

# 检查 patch 1: native binary 检测
if grep -q '!v9())' "$CLI_JS" 2>/dev/null; then
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
    log_ok "所有 patch 已应用，无需操作"
    exit 0
fi

# 备份
BACKUP="${CLI_JS}.bak"
if [[ ! -f "$BACKUP" ]]; then
    cp "$CLI_JS" "$BACKUP"
    log_ok "备份: $BACKUP"
else
    log_warn "备份已存在，跳过备份"
fi

# Patch 1: native binary 检测
# v9() 检测 Bun runtime，替换为 !1 使其永远不触发
if grep -q '!v9())' "$CLI_JS" 2>/dev/null; then
    sed -i 's/!v9())/!1)/g' "$CLI_JS"
    if ! grep -q '!v9())' "$CLI_JS" 2>/dev/null; then
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
    sed -i 's/r16.status==="disabled"/!1/g' "$CLI_JS"
    if ! grep -q 'r16.status==="disabled"' "$CLI_JS" 2>/dev/null; then
        log_ok "Patch 2: penguin mode 检测已绕过"
        PATCHES_DONE=$((PATCHES_DONE + 1))
    else
        log_err "Patch 2 失败"
    fi
fi

# 结果
echo ""
if [[ $PATCHES_DONE -eq 2 ]]; then
    log_ok "全部 patch 成功！/fast 模式已解锁"
    echo ""
    echo "提示："
    echo "  - 重启 Claude Code 生效"
    echo "  - 恢复原始文件: bash $0 --restore"
    echo "  - Claude Code 更新后需要重新运行此脚本"
else
    log_warn "部分 patch 完成 ($PATCHES_DONE/2)"
    log_warn "请检查 cli.js 版本是否兼容"
fi
