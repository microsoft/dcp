$json = go mod edit -json
$deps = $json | ConvertFrom-Json | Select-Object -ExpandProperty Require

$rootDeps = $deps | where {$_.Indirect -notmatch $true}
$inderectDeps = $deps | where {$_.Indirect -match $true}

$manifest = @{"`$schema" = "https://json.schemastore.org/component-detection-manifest.json"; version = 1; registrations = @()}

foreach ($dep in $rootDeps) {
    $manifest.registrations += @{component = @{type = "go"; go = @{name = $dep.Path; version = $dep.Version}}}
}

foreach ($dep in $inderectDeps) {
    $registration = @{component = @{type = "go"; go = @{name = $dep.Path; version = $dep.Version}; dependencyRoots = @()}}
    $why = go mod why -m $dep.Path
    $why = $why | Select-Object -Skip 1 | where {$_ -notmatch "^github.com/microsoft/dcp/"}
    foreach ($root in $why) {
        $registration.component.dependencyRoots += $root
    }
    $manifest.registrations += $registration
}

$manifest | ConvertTo-Json -Depth 10 | Out-File -Encoding utf8 -Path "cgmanifest.json"
