
function dcpdKubectl() {
    $DcpPid = $null
    if ($args.Length -gt 1 -and $args[0] -eq "-DcpPid") {
        $DcpPid = $args[1]
        $args = $args | Select-Object -Skip 2
    }

    if ($null -ne $DcpPid) {
        $dcpProcess = Get-Process -Id $DcpPid
        if ($? -eq $false) {
            throw "No DCP process with PID $DcpPid found"
        }

        $dcpCommandLine = $dcpProcess | Select-Object -ExpandProperty CommandLine | ForEach-Object { $_.Split() }
    } else {
        $dcpProcesses = @(Get-Process | Where-Object { $_.Name -ceq "dcp" })
        if ($dcpProcesses.Count -eq 0) {
            throw "No DCP processes found"
        } elseif ($dcpProcesses.Count -gt 1) {
            Write-Error "Multiple DCP processes found, use -DcpPid parameter to specify which one to use (pass DCP process ID)"
            $dcpProcesses | Select-Object Id, CommandLine | Format-List
            return
        } else {
            $dcpCommandLine = $dcpProcesses[0].CommandLine.Split()
        }
    }
    
    $kubeconfig = $dcpCommandLine | Where-Object { $_.Contains("kubeconfig") -and ($_ -ne "--kubeconfig") }
    
    $tokenOptionIndex = $dcpCommandLine.IndexOf("--token")
    if ($tokenOptionIndex -ge 0) {
        $token = $dcpCommandLine[$tokenOptionIndex + 1]
    }
    
    if ($token) {
        & kubectl --token "$token" --kubeconfig "$kubeconfig" $args
    } else {
        & kubectl --kubeconfig "$kubeconfig" $args
    }
    
}

Set-Alias kk -Value dcpdKubectl
