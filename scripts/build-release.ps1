# 交叉编译发布产物：Linux/macOS/Windows 二进制 + 校验和。
#
# 用法（在仓库根目录）：
#   pwsh scripts/build-release.ps1 -Version v0.1.0
#
# 产物（dist/，不进仓库）：
#   safeguard-agent_v0.1.0_linux_amd64.tar.gz
#   safeguard-agent_v0.1.0_linux_arm64.tar.gz
#   safeguard-agent_v0.1.0_darwin_amd64.tar.gz
#   safeguard-agent_v0.1.0_darwin_arm64.tar.gz
#   safeguard-agent_v0.1.0_windows_amd64.zip
#   SHA256SUMS
#
# 每个包里包含：二进制 + LICENSE + README.md。
# 版本号与 commit 通过 -ldflags 注入，所以 `safeguard-agent -version` 能对上 tag。

param(
    [string]$Version = "v0.1.0",
    [string]$Commit = "",
    [string]$OutDir = "dist"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$targets = @(
    @{ os = "linux";   arch = "amd64"; ext = "" },
    @{ os = "linux";   arch = "arm64"; ext = "" },
    @{ os = "darwin";  arch = "amd64"; ext = "" },
    @{ os = "darwin";  arch = "arm64"; ext = "" },
    @{ os = "windows"; arch = "amd64"; ext = ".exe" }
)

$ldflags = "-s -w -X main.version=$Version"

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
Get-ChildItem $OutDir -File | Remove-Item -Force
$stageRoot = Join-Path $OutDir ".stage"
Remove-Item $stageRoot -Recurse -Force -ErrorAction SilentlyContinue

$savedCGO = $env:CGO_ENABLED
$savedGOOS = $env:GOOS
$savedGOARCH = $env:GOARCH
$env:CGO_ENABLED = "0"   # 纯 Go 依赖：关掉 CGO 才能跨平台编译

try {
    foreach ($t in $targets) {
        $env:GOOS = $t.os
        $env:GOARCH = $t.arch

        $name = "safeguard-agent_${Version}_$($t.os)_$($t.arch)"
        $stage = Join-Path $stageRoot $name
        New-Item -ItemType Directory -Force -Path $stage | Out-Null

        $exe = if ($t.ext) { "safeguard-agent.exe" } else { "safeguard-agent" }
        Write-Host "==> $($t.os)/$($t.arch)"
        go build -trimpath -ldflags $ldflags -o (Join-Path $stage $exe) ./cmd/agent
        if ($LASTEXITCODE -ne 0) { throw "构建失败: $($t.os)/$($t.arch)" }

        Copy-Item LICENSE, README.md -Destination $stage

        if ($t.ext) {
            Compress-Archive -Path (Join-Path $stage "*") -DestinationPath (Join-Path $OutDir "$name.zip") -Force
        } else {
            tar -czf (Join-Path $OutDir "$name.tar.gz") -C $stage .
        }
    }
} finally {
    $env:CGO_ENABLED = $savedCGO
    $env:GOOS = $savedGOOS
    $env:GOARCH = $savedGOARCH
    Remove-Item $stageRoot -Recurse -Force -ErrorAction SilentlyContinue
}

# 校验和：sha256sum 格式（"<hash>  <file>"），Linux/macOS 的 sha256sum -c 可直接用
$lines = Get-ChildItem $OutDir -File |
    Where-Object { $_.Name -match '\.(zip|tar\.gz)$' } |
    Sort-Object Name |
    ForEach-Object {
        $hash = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower()
        "$hash  $($_.Name)"
    }
Set-Content -Path (Join-Path $OutDir "SHA256SUMS") -Value $lines -Encoding ascii

Write-Host ""
Write-Host "完成：$Version"
Get-ChildItem $OutDir -File | ForEach-Object {
    "{0,10:N0} KB  {1}" -f ($_.Length / 1KB), $_.Name
}
