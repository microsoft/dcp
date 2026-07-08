#!/usr/bin/env bash
#
# Runs the Aspire end-to-end regression test against a locally built DCP binary.
#
# It scaffolds a minimal AppHost with the installed (ideally latest daily) Aspire
# CLI, keeps the scaffolded daily "#:sdk" directive, appends
# test/aspire/apphost-body.cs, and runs it while pointing Aspire at the local DCP
# build via DcpPublisher__CliPath. The test passes only if every resource becomes
# healthy and the AppHost exits cleanly.
#
# Prerequisites:
#   - The Aspire CLI on PATH (install the daily channel with:
#       curl -sSL https://aspire.dev/install.sh | bash -s -- -q dev )
#   - A .NET 10 SDK on PATH
#   - A container runtime (Docker/Podman) available for the container resources
#
# Usage:
#   test/aspire/run-regression.sh [--dcp /path/to/dcp]
#
# The DCP binary defaults to <repo>/bin/dcp (or bin/dcp.exe on Windows) and can
# be overridden with --dcp or the DCP_CLI_PATH environment variable.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

dcp_ext=""
case "$(uname -s 2>/dev/null || echo)" in
    MINGW* | MSYS* | CYGWIN* | Windows_NT) dcp_ext=".exe" ;;
esac

dcp_cli_path="${DCP_CLI_PATH:-${repo_root}/bin/dcp${dcp_ext}}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dcp)
            dcp_cli_path="$2"
            shift 2
            ;;
        --dcp=*)
            dcp_cli_path="${1#*=}"
            shift
            ;;
        -h | --help)
            sed -n '2,25p' "${BASH_SOURCE[0]}"
            exit 0
            ;;
        *)
            echo "Unknown argument: $1" >&2
            exit 2
            ;;
    esac
done

if [[ ! -x "${dcp_cli_path}" ]]; then
    echo "DCP binary not found or not executable at: ${dcp_cli_path}" >&2
    echo "Build it first (e.g. 'make build') or pass --dcp <path>." >&2
    exit 1
fi

if ! command -v aspire >/dev/null 2>&1; then
    echo "The 'aspire' CLI was not found on PATH." >&2
    echo "Install the daily channel with:" >&2
    echo "  curl -sSL https://aspire.dev/install.sh | bash -s -- -q dev" >&2
    exit 1
fi

work_dir="$(mktemp -d 2>/dev/null || mktemp -d -t dcp-aspire-regression)"
run_id="$(date -u +%Y%m%d%H%M%S)-$$"

cleanup() {
    rm -rf "${work_dir}"
}
trap cleanup EXIT

echo "==> Scaffolding a minimal AppHost with the Aspire daily CLI"
aspire new aspire-empty \
    --name DcpRegression \
    --output "${work_dir}" \
    --channel daily \
    --language csharp \
    --non-interactive \
    --nologo \
    --suppress-agent-init

generated_apphost="${work_dir}/apphost.cs"
if [[ ! -f "${generated_apphost}" ]]; then
    echo "Scaffolding did not produce an apphost.cs in ${work_dir}" >&2
    exit 1
fi

# Preserve the daily SDK pin that the CLI scaffolded and append the committed
# AppHost body. The committed body intentionally has no "#:sdk" directive so the
# regression always runs against the latest daily Aspire AppHost SDK selected by
# the CLI scaffold.
sdk_line="$(grep -m1 '^#:sdk' "${generated_apphost}" || true)"
if [[ -z "${sdk_line}" ]]; then
    echo "Could not find a '#:sdk' directive in the scaffolded apphost.cs" >&2
    exit 1
fi

echo "==> Using scaffolded SDK pin: ${sdk_line}"
{
    echo "${sdk_line}"
    cat "${script_dir}/apphost-body.cs"
} >"${generated_apphost}"

echo "==> Running the regression AppHost against DCP at: ${dcp_cli_path}"
output_log="${work_dir}/run.log"
set +e
(
    cd "${work_dir}"
    DcpPublisher__CliPath="${dcp_cli_path}" \
        DCP_ASPIRE_REGRESSION_RUN_ID="${run_id}" \
        ASPIRE_ALLOW_UNSECURED_TRANSPORT="true" \
        DOTNET_ENVIRONMENT="Development" \
        dotnet run apphost.cs
) 2>&1 | tee "${output_log}"
run_status=${PIPESTATUS[0]}
set -e

if [[ ${run_status} -ne 0 ]]; then
    echo "AppHost exited with status ${run_status}" >&2
    exit "${run_status}"
fi

if ! grep -q "DCP-REGRESSION-APPHOST-STARTED" "${output_log}"; then
    echo "AppHost exited cleanly but did not report that it started" >&2
    exit 1
fi

for resource_name in worker cache persistent-cache persistent-worker web; do
    if ! grep -q "DCP-REGRESSION-RESOURCE-HEALTHY: ${resource_name}" "${output_log}"; then
        echo "AppHost exited cleanly but did not report resource healthy: ${resource_name}" >&2
        exit 1
    fi
done

if ! grep -q "DCP-REGRESSION-OK" "${output_log}"; then
    echo "AppHost exited cleanly but did not report all resources healthy" >&2
    exit 1
fi

echo "==> Aspire regression test passed"
