# scripts/install.ps1 —— LLMate Gate Windows 一键安装（最小分发）
#
# 行为：
#   1. 拷 llmate-gate.exe 到 %LOCALAPPDATA%\Programs\llmate-gate\
#   2. 注册 Windows 计划任务「Logon 触发 / 后台启动 / 自动重启」
#   3. 在桌面放一个"打开调试面板"快捷方式（指向 http://127.0.0.1:8400/_debug）
#   4. 启动服务一次
#
# 用法：
#   .\scripts\install.ps1                       # 装当前 dist/llmate-gate-windows-amd64.exe
#   .\scripts\install.ps1 -Binary C:\path\to\llmate-gate.exe
#   .\scripts\install.ps1 -Port 8400 -NoAutostart
#   .\scripts\install.ps1 -Uninstall             # 卸载
#
# 设计：零外部依赖（PowerShell 5.1+ 内置）；不解压额外 DLL；不写注册表 Run 键
#       （计划任务比 Run 键更稳：能后台运行、退出码可观测、不弹 UAC）。
[CmdletBinding()]
param(
    [string]$Binary = "",
    [int]$Port = 8400,
    [string]$Listen = "",
    [switch]$NoAutostart,
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"

$ProductName = "llmate-gate"
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\$ProductName"
$DataDir = Join-Path $env:LOCALAPPDATA "$ProductName\data"
$TaskName = "LLMate Gate"
$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$DefaultBinary = Join-Path $RepoRoot "dist\llmate-gate-windows-amd64.exe"

function Log($m) { Write-Host "[install] $m" }
function Fail($m) { Write-Host "[install][FAIL] $m" -ForegroundColor Red; exit 1 }

function Find-Binary {
    if ($Binary -ne "") { return (Resolve-Path $Binary).Path }
    if (Test-Path $DefaultBinary) { return $DefaultBinary }
    # Scoop 安装位置（如果存在）
    $scoop = Join-Path $env:USERPROFILE "scoop\apps\$ProductName\current\llmate-gate.exe"
    if (Test-Path $scoop) { return $scoop }
    Fail "找不到二进制。请用 -Binary 指定，或先把 dist\llmate-gate-windows-amd64.exe 编译出来。"
}

function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    $p = New-Object Security.Principal.WindowsPrincipal($id)
    return $p.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Do-Uninstall {
    Log "卸载 $ProductName"
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
        Log "  已注销计划任务 $TaskName"
    } else {
        Log "  计划任务不存在，跳过"
    }
    # 杀进程
    Get-Process -Name "llmate-gate" -ErrorAction SilentlyContinue | Stop-Process -Force
    # 清理桌面快捷方式
    $desktop = [Environment]::GetFolderPath("Desktop")
    $lnk = Join-Path $desktop "LLMate Gate 调试面板.lnk"
    if (Test-Path $lnk) {
        Remove-Item -Force $lnk
        Log "  已删除桌面快捷方式"
    }
    if (Test-Path $InstallDir) {
        Remove-Item -Recurse -Force $InstallDir
        Log "  已删 $InstallDir"
    }
    Log "卸载完成。数据目录 $DataDir 保留（如需清空请手动 rm -r）。"
    exit 0
}

if ($Uninstall) { Do-Uninstall }

# 1. 找 binary
$src = Find-Binary
Log "binary: $src"

# 2. 拷到 InstallDir
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item -Force $src (Join-Path $InstallDir "llmate-gate.exe")
Log "已安装 → $InstallDir\llmate-gate.exe"

New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

# 3. 写一个 config 模板（如果还没有）
# 键名以 Specs/05-实现规格-AS_BUILT.md §3.1 为准。网关以严格模式解析配置：
# 未知键会导致启动失败，不会静默忽略（否则拼错的键会被无声丢弃）。
$cfg = Join-Path $DataDir "config.yaml"
if (-not (Test-Path $cfg)) {
    @"
# LLMate Gate 配置（最小可用）
gateway:
  listen: ":${Port}"
  upstream: "https://api.openai.com"     # 上游 base URL
  upstream_api_key: ""                   # 必填：上游 LLM 的 API key
  debug: true
detection:
  engine: "regex"
replacement:
  strategy: "placeholder"
"@ | Set-Content -Path $cfg -Encoding UTF8
    Log "已生成默认配置 → $cfg"
    Log "  注意：请填写 gateway.upstream_api_key（上游 API key），否则转发会被上游拒绝"
}

# 4. 计划任务
if (-not $NoAutostart) {
    $exe = Join-Path $InstallDir "llmate-gate.exe"
    # 默认也显式传 --listen：否则端口由既有配置决定，与本脚本 -Port（快捷方式与
    # 健康检查都按它走）可能不一致，表现为「装好了但打不开」。
    $effListen = if ($Listen -ne "") { $Listen } else { ":$Port" }
    $action = New-ScheduledTaskAction -Execute $exe -Argument "--config `"$cfg`" --listen $effListen"
    $trigger = New-ScheduledTaskTrigger -AtLogOn
    $settings = New-ScheduledTaskSettingsSet `
        -AllowStartIfOnBatteries `
        -DontStopIfGoingOnBatteries `
        -StartWhenAvailable `
        -RestartCount 5 -RestartInterval (New-TimeSpan -Minutes 1) `
        -ExecutionTimeLimit (New-TimeSpan -Hours 0)  # 0 = 无限制
    Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
        -Settings $settings -Description "LLMate Gate 本机 PII 脱敏网关" `
        -RunLevel Highest | Out-Null
    Log "已注册计划任务 '$TaskName'（Logon 触发）"
}

# 5. 桌面快捷方式
$desktop = [Environment]::GetFolderPath("Desktop")
$shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut(
    (Join-Path $desktop "LLMate Gate 调试面板.lnk"))
$shortcut.TargetPath = "http://127.0.0.1:$Port/_debug"
$shortcut.WorkingDirectory = $InstallDir
$shortcut.IconLocation = "shell32.dll,13"
$shortcut.Description = "打开 LLMate Gate 内嵌调试面板"
$shortcut.Save()
Log "桌面快捷方式已创建 → $desktop\LLMate Gate 调试面板.lnk"

# 6. 立即启动一次（让用户立刻能用）
if (-not $NoAutostart) {
    Start-ScheduledTask -TaskName $TaskName
    Log "已触发启动计划任务"
    Start-Sleep -Seconds 2
    try {
        $r = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/healthz" -UseBasicParsing -TimeoutSec 5
        if ($r.StatusCode -eq 200) { Log "✓ 健康检查通过" }
    } catch {
        Log "WARN 健康检查未通过（可能还在启动）。请看：Get-EventLog -LogName Application -Newest 5"
    }
}

Log "安装完成。"
Log "  二进制：$InstallDir\llmate-gate.exe"
Log "  配置：$cfg（请填 gateway.upstream_api_key）"
Log "  调试面板： http://127.0.0.1:$Port/_debug"
Log "  卸载：   powershell -File scripts\install.ps1 -Uninstall"
