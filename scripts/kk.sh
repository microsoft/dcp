#!/usr/bin/env bash
set -o pipefail

usage ()
{
    cat <<END
kk [--dcp-pid PID] args...

Runs a kubectl command to interact with the DCP process, automatically supplying the right kubeconfig file.

END
}

if [[ ($# -gt 0) && (($1 = '-h') || ($1 = '--help')) ]]; then
    usage
    exit 1
fi

if [[ ($# -gt 1) && ($1 = '--dcp-pid') ]]; then
    shift
    dcp_pid=$1
    shift
else
    dcp_pid=$(pgrep -f 'dcp start-apiserver')
    if [[ $? -ne 0 ]]; then
        echo "DCP processes could not be found (pgrep failed). Please specify the PID of the DCP process you want to interact with."
        exit 2
    fi
    dcp_process_count=$(echo "$dcp_pid" | wc -l)
    if [[ $dcp_process_count -eq 0 ]]; then
        echo "No DCP process found"
        exit 2
    elif [[ $dcp_process_count -gt 1 ]]; then
        echo "Multiple DCP processes found. Please specify the PID of the DCP process you want to interact with."
        exit 2
    fi
fi

dcp_command=$(ps -o command -p $dcp_pid)
if [[ $? -ne 0 ]]; then
    echo "Could not get the command of the DCP process with PID $dcp_pid"
    exit 3
fi

# Extract the kubeconfig file path from the dcp command
# Break the command into words (one per line), remove "--kubeconfig" flag, and take the path to kubeconfig file
kubeconfig_file=$(echo "$dcp_command" | tr ' ' '\n' | grep -v -e '--kubeconfig' | grep -e 'kubeconfig')
if [[ $? -ne 0 ]]; then
    echo "Could not extract the kubeconfig file path from the DCP command: $dcp_command"
    exit 4
fi

# Extract security token from the command, if any
security_token_arg=''
security_token=$(echo "$dcp_command" | grep -E -o -e '--token (\w+)' | tr ' ' '\n' | grep -v -e '--token')
if [[ $? -eq 0 ]]; then
    security_token_arg="--token $security_token"
fi

kubectl --kubeconfig=$kubeconfig_file $security_token_arg $@
