# test-ci.ps1 — Windows CI test driver
#
# Replicates the behavior of `make test-ci` without depending on GNU make or
# mingw, so the Windows CI agent does not need to acquire those tools from
# chocolatey (or any other public package manager).
#
# IMPORTANT: This script must be kept in sync with the Makefile targets
# `test-ci`, `test-ci-prereqs`, and the `TEST_PREREQS` variable. If you change
# any of those (add/remove a prerequisite, change build flags, change the final
# `go test` invocation, change the protoc version, etc.) you MUST update this
# script to match. See also the comments above those targets in the Makefile.
#
# The sequence implemented here mirrors, for the non-make-4.4 TEST_PREREQS:
#   generate-grpc build-dcp build-dcptun-containerexe
#   delay-tool lfwriter-tool parrot-tool parrot-tool-containerexe
# followed by `go test ./... -coverprofile cover.out -count 1`.
#
# The Windows CI job runs with CGO_ENABLED=0, so no C toolchain (mingw/gcc) is
# required and -race is not passed (matching the Makefile's TEST_OPTS).

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Makes Invoke-WebRequest / Expand-Archive noticeably faster.

# Keep in sync with Makefile: PROTOC_VERSION.
$ProtocVersion = '33.5'

$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$OutputBin = Join-Path $RepoRoot 'bin'
$ToolBin = Join-Path $RepoRoot '.toolbin'
$ProtocDir = Join-Path $ToolBin 'protoc'
$ProtocExe = Join-Path $ProtocDir 'bin\protoc.exe'

function Invoke-Native {
    param([Parameter(Mandatory)][scriptblock]$Script, [string]$Description)
    & $Script
    if ($LASTEXITCODE -ne 0) {
        throw "$Description failed with exit code $LASTEXITCODE"
    }
}

function New-DirectoryIfMissing {
    param([Parameter(Mandatory)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) {
        New-Item -ItemType Directory -Force -Path $Path | Out-Null
    }
}

function Install-Protoc {
    if (Test-Path -LiteralPath $ProtocExe) {
        Write-Host "protoc already present at $ProtocExe"
        return
    }

    New-DirectoryIfMissing $ToolBin
    # Mirrors Makefile PROTOC_ZIP for Windows amd64.
    $zipName = "protoc-$ProtocVersion-win64.zip"
    $zipPath = Join-Path $ToolBin $zipName
    $url = "https://github.com/protocolbuffers/protobuf/releases/download/v$ProtocVersion/$zipName"

    Write-Host "Downloading $url"
    Invoke-WebRequest -Uri $url -OutFile $zipPath -UseBasicParsing

    Write-Host "Extracting $zipPath -> $ProtocDir"
    Expand-Archive -Path $zipPath -DestinationPath $ProtocDir -Force

    if (-not (Test-Path -LiteralPath $ProtocExe)) {
        throw "protoc extraction did not produce expected binary at $ProtocExe"
    }
}

function Get-GoToolPluginPath {
    param([Parameter(Mandatory)][string]$ModulePath)
    # Mirrors `go tool -n <path>` used by the Makefile to locate generator plugin binaries.
    # We clear GOOS/GOARCH so the plugin binary is resolved for the host (Windows), not any
    # cross-compile target that may have been set.
    $savedGoos = $env:GOOS
    $savedGoarch = $env:GOARCH
    try {
        $env:GOOS = ''
        $env:GOARCH = ''
        $path = & go tool -n $ModulePath
        if ($LASTEXITCODE -ne 0) {
            throw "go tool -n $ModulePath failed with exit code $LASTEXITCODE"
        }
        return ($path | Select-Object -First 1).Trim()
    } finally {
        $env:GOOS = $savedGoos
        $env:GOARCH = $savedGoarch
    }
}

function Invoke-GenerateGrpc {
    Install-Protoc

    $protoSources = Get-ChildItem -Path (Join-Path $RepoRoot 'internal') -Filter '*.proto' -Recurse -File |
        Select-Object -ExpandProperty FullName

    if (-not $protoSources) {
        Write-Host 'No .proto sources found; skipping generate-grpc.'
        return
    }

    $goPlugin = Get-GoToolPluginPath 'google.golang.org/protobuf/cmd/protoc-gen-go'
    $goGrpcPlugin = Get-GoToolPluginPath 'google.golang.org/grpc/cmd/protoc-gen-go-grpc'

    Push-Location $RepoRoot
    try {
        foreach ($proto in $protoSources) {
            $rel = Resolve-Path -Relative -LiteralPath $proto
            Write-Host "protoc (go) $rel"
            Invoke-Native -Description "protoc-gen-go on $rel" -Script {
                & $ProtocExe `
                    --go_out=. `
                    --go_opt=paths=source_relative `
                    "--plugin=protoc-gen-go=$goPlugin" `
                    $rel
            }

            Write-Host "protoc (go-grpc) $rel"
            Invoke-Native -Description "protoc-gen-go-grpc on $rel" -Script {
                & $ProtocExe `
                    --go-grpc_out=. `
                    --go-grpc_opt=paths=source_relative `
                    "--plugin=protoc-gen-go-grpc=$goGrpcPlugin" `
                    $rel
            }
        }
    } finally {
        Pop-Location
    }
}

function Invoke-GoBuild {
    param(
        [Parameter(Mandatory)][string]$Output,
        [Parameter(Mandatory)][string]$Package,
        [string]$TargetGoos
    )

    $savedGoos = $env:GOOS
    try {
        if ($TargetGoos) {
            $env:GOOS = $TargetGoos
        }
        Write-Host "go build -> $Output ($Package, GOOS=$(if ($TargetGoos) { $TargetGoos } else { 'host' }))"
        Invoke-Native -Description "go build $Package" -Script {
            & go build -o $Output $Package
        }
    } finally {
        $env:GOOS = $savedGoos
    }
}

function Build-TestPrereqs {
    New-DirectoryIfMissing $OutputBin
    New-DirectoryIfMissing $ToolBin

    Invoke-GenerateGrpc

    # build-dcp
    Invoke-GoBuild -Output (Join-Path $OutputBin 'dcp.exe') -Package './cmd/dcp'
    # build-dcptun-containerexe (Linux binary, used inside containers)
    Invoke-GoBuild -Output (Join-Path $OutputBin 'dcptun_c') -Package './cmd/dcptun' -TargetGoos 'linux'
    # delay-tool / lfwriter-tool / parrot-tool
    Invoke-GoBuild -Output (Join-Path $ToolBin 'delay.exe') -Package 'github.com/microsoft/dcp/test/delay'
    Invoke-GoBuild -Output (Join-Path $ToolBin 'lfwriter.exe') -Package 'github.com/microsoft/dcp/test/lfwriter'
    Invoke-GoBuild -Output (Join-Path $ToolBin 'parrot.exe') -Package 'github.com/microsoft/dcp/test/parrot'
    # parrot-tool-containerexe (Linux binary)
    Invoke-GoBuild -Output (Join-Path $ToolBin 'parrot_c') -Package 'github.com/microsoft/dcp/test/parrot' -TargetGoos 'linux'
}

function Invoke-Tests {
    # Matches TEST_OPTS in the Makefile when CGO_ENABLED=0: no -race, no -parallel override.
    Push-Location $RepoRoot
    try {
        Write-Host 'go test ./... -coverprofile cover.out -count 1'
        Invoke-Native -Description 'go test' -Script {
            & go test ./... -coverprofile cover.out -count 1
        }
    } finally {
        Pop-Location
    }
}

Push-Location $RepoRoot
try {
    Build-TestPrereqs
    Invoke-Tests
} finally {
    Pop-Location
}
