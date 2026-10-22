param(
    [string]$Go = "go"
)

$ErrorActionPreference = "Stop"
$skillRoot = Split-Path -Parent $PSScriptRoot
$targets = @(
    @{ OS = "windows"; Arch = "amd64"; Extension = ".exe" },
    @{ OS = "windows"; Arch = "arm64"; Extension = ".exe" },
    @{ OS = "darwin"; Arch = "amd64"; Extension = "" },
    @{ OS = "darwin"; Arch = "arm64"; Extension = "" },
    @{ OS = "linux"; Arch = "amd64"; Extension = "" },
    @{ OS = "linux"; Arch = "arm64"; Extension = "" }
)

$previousGoos = $env:GOOS
$previousGoarch = $env:GOARCH
$previousCgo = $env:CGO_ENABLED
$previousToolchain = $env:GOTOOLCHAIN

try {
    $env:CGO_ENABLED = "0"
    $env:GOTOOLCHAIN = "local"
    Push-Location $PSScriptRoot
    foreach ($target in $targets) {
        $env:GOOS = $target.OS
        $env:GOARCH = $target.Arch
        $directory = Join-Path $skillRoot ("bin/{0}-{1}" -f $target.OS, $target.Arch)
        New-Item -ItemType Directory -Force $directory | Out-Null
        $output = Join-Path $directory ("foxapi-imagegen{0}" -f $target.Extension)
        & $Go build -trimpath -ldflags "-s -w" -o $output .
        if ($LASTEXITCODE -ne 0) {
            throw "Go build failed for $($target.OS)/$($target.Arch)"
        }
        Write-Host "Built $($target.OS)/$($target.Arch): $output"
    }
} finally {
    Pop-Location
    $env:GOOS = $previousGoos
    $env:GOARCH = $previousGoarch
    $env:CGO_ENABLED = $previousCgo
    $env:GOTOOLCHAIN = $previousToolchain
}