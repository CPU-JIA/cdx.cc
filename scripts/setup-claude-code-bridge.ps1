param(
    [string]$CliJs,
    [string]$Bridge,
    [string]$AdminPassword,
    [string]$CompactMode,
    [int]$CompactThreshold = 0,
    [switch]$SkipFast,
    [switch]$SkipCompact,
    [switch]$Restore,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'

function Write-Err($msg) {
    Write-Host "[ERR] $msg" -ForegroundColor Red
}

function Find-Bash {
    $candidates = New-Object System.Collections.Generic.List[string]
    try {
        $cmd = Get-Command bash -ErrorAction Stop
        if ($cmd.Source) { $candidates.Add($cmd.Source) }
    } catch {}

    foreach ($candidate in @(
        'C:\Program Files\Git\bin\bash.exe',
        'C:\Program Files\Git\usr\bin\bash.exe',
        "$env:ProgramFiles\Git\bin\bash.exe",
        "$env:ProgramFiles\Git\usr\bin\bash.exe"
    )) {
        if ($candidate -and (Test-Path $candidate)) {
            return $candidate
        }
    }

    foreach ($candidate in $candidates) {
        if ($candidate -and (Test-Path $candidate)) {
            return $candidate
        }
    }
    return $null
}

function Convert-ToBashPath([string]$path, [string]$bashExePath) {
    if (-not $path) { return $path }
    if ($path -match '^[A-Za-z]:\\') {
        $drive = $path.Substring(0,1).ToLowerInvariant()
        $rest = $path.Substring(2).Replace('\', '/')
        if ($bashExePath -match 'Git\\') {
            return "/$drive$rest"
        }
        return "/mnt/$drive$rest"
    }
    return $path.Replace('\', '/')
}

if ($Help) {
    Write-Host 'Usage:'
    Write-Host '  powershell -ExecutionPolicy Bypass -File .\scripts\setup-claude-code-bridge.ps1 [options]'
    Write-Host ''
    Write-Host 'This is a PowerShell launcher for scripts/setup-claude-code-bridge.sh.'
    Write-Host 'It keeps the exact same behavior as the Bash script.'
    exit 0
}

$bashExe = Find-Bash
if (-not $bashExe) {
    Write-Err 'bash was not found. Install Git Bash or run the .sh script directly.'
    exit 1
}

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$bashScript = Join-Path $scriptDir 'setup-claude-code-bridge.sh'
if (-not (Test-Path $bashScript)) {
    Write-Err "Missing script: $bashScript"
    exit 1
}
$bashScript = Convert-ToBashPath $bashScript $bashExe

$argsList = @()
if ($CliJs) { $argsList += '--cli-js'; $argsList += (Convert-ToBashPath $CliJs $bashExe) }
if ($Bridge) { $argsList += '--bridge'; $argsList += $Bridge }
if ($AdminPassword) { $argsList += '--admin-password'; $argsList += $AdminPassword }
if ($CompactMode) { $argsList += '--compact-mode'; $argsList += $CompactMode }
if ($CompactThreshold -gt 0) { $argsList += '--compact-threshold'; $argsList += [string]$CompactThreshold }
if ($SkipFast) { $argsList += '--skip-fast' }
if ($SkipCompact) { $argsList += '--skip-compact' }
if ($Restore) { $argsList += '--restore' }
if ($Help) { $argsList += '--help' }

& $bashExe $bashScript @argsList
exit $LASTEXITCODE
