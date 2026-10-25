# SPARK agent installer for Windows (PowerShell 5.1+)
# 用法:  powershell -ExecutionPolicy Bypass -File install.ps1 [-Name "我的旧电脑"]
param(
    [string]$Name = "",
    [string]$InstallDir = "$env:LOCALAPPDATA\SPARK"
)

$ErrorActionPreference = "Stop"
$Repo = "never-die-cold/SPARK-On-premises-version"
$BaseUrl = "https://github.com/$Repo/releases/latest/download"

Write-Host "==> 平台: windows/$env:PROCESSOR_ARCHITECTURE" 

# 1) 获取二进制：Releases 优先，本地源码构建兜底
$Exe = Join-Path $InstallDir "spark-agent.exe"
if (Test-Path $Exe) {
    Write-Host "==> 已存在 $Exe"
}
else {
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $tmp = Join-Path $env:TEMP "spark-agent.exe"
    try {
        Invoke-WebRequest -Uri "$BaseUrl/spark-agent-windows-amd64.exe" -OutFile $tmp -UseBasicParsing
        Move-Item $tmp $Exe -Force
        Write-Host "==> 已安装 $Exe"
    }
    catch {
        Write-Host "==> Releases 中暂无二进制，尝试本地构建（需要 go >= 1.23）..."
        $go = Get-Command go -ErrorAction SilentlyContinue
        if (-not $go) { throw "没有 go，也无法下载二进制。请安装 go 后重试。" }
        $src = Join-Path $env:TEMP "spark-src"
        if (-not (Test-Path "$src\agent")) {
            git clone --depth 1 "https://github.com/$Repo.git" $src
        }
        Push-Location "$src\agent"
        go build -o $Exe .
        Pop-Location
        Write-Host "==> 已构建 $Exe"
    }
}

# 2) 开机自启动（当前用户登录时启动，无需管理员）
$action  = New-ScheduledTaskAction -Execute $Exe -Argument $(if ($Name) { "--name `"$Name`"" } else { "" })
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName "SPARK-Agent" -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName "SPARK-Agent"
Write-Host "==> 计划任务 SPARK-Agent 已创建并启动"

# 3) 防火墙放行（TCP 3971 控制 / UDP 3970 发现；需要管理员，失败不影响使用）
$fw = New-Object -ComObject HNetCfg.FWPolicy2 -ErrorAction SilentlyContinue
if ($fw) {
    foreach ($rule in Get-NetFirewallRule -DisplayName "SPARK agent*" -ErrorAction SilentlyContinue) {
        Remove-NetFirewallRule -Name $rule.Name
    }
    try {
        New-NetFirewallRule -DisplayName "SPARK agent control" -Direction Inbound -Program $Exe -Protocol TCP -LocalPort 3971 -Action Allow | Out-Null
        New-NetFirewallRule -DisplayName "SPARK agent discovery" -Direction Inbound -Program $Exe -Protocol UDP -LocalPort 3970 -Action Allow | Out-Null
        Write-Host "==> 防火墙规则已添加"
    }
    catch {
        Write-Warning "防火墙规则添加失败（需要管理员）。首次运行 Windows 弹窗时勾选允许即可。"
    }
}

Write-Host "==> 完成。日志: $HOME\.spark-agent\logs\；master 侧运行 spark status 查看设备"
