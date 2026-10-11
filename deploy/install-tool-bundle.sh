#!/bin/sh
# Run from an extracted trace-tool-bundle. No package manager network operations.
set -eu
umask 077

bundle=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
python=${TRACE_PYTHON:-python3}
prefix=${TRACE_INSTALL_PREFIX:-${XDG_DATA_HOME:-$HOME/.local/share}/trace}
case "$prefix" in /*) ;; *) printf '%s\n' 'Install prefix must be absolute.' >&2; exit 2 ;; esac
unset PYTHONPATH PYTHONHOME PIP_TARGET PIP_PREFIX PIP_USER
export PIP_CONFIG_FILE=/dev/null PIP_NO_INDEX=1 PIP_DISABLE_PIP_VERSION_CHECK=1
export PYTHONDONTWRITEBYTECODE=1 PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
export CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true
"$python" -c 'import json,sys,sysconfig; from pathlib import Path; b=json.loads(Path(sys.argv[1]).read_text()); assert sys.platform == "linux"; assert list(sys.version_info[:2]) == b["target"]["python"], "Use the build Python minor version"; assert sysconfig.get_platform() == b["target"]["platform"], "Bundle platform mismatch"; assert sys.implementation.name == b["target"]["implementation"]; assert sorted(p.name for p in Path(sys.argv[1]).with_name("wheels").glob("*.whl")) == b["wheels"], "Missing or unexpected bundle wheels"' "$bundle/bundle.json"
mkdir -p "$prefix/releases"
release=$(mktemp -d "$prefix/releases/release-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")
"$python" -m venv "$release"
"$release/bin/python" -m pip install --no-index --no-deps --find-links "$bundle/wheels" "$bundle"/wheels/*.whl
"$release/bin/python" -m pip check
cp -R "$bundle/tools" "$release/tools"
cp "$bundle/bundle.json" "$bundle/bundle.py" "$bundle/run-trace.sh" "$bundle/cloud_sdk_smoke.py" "$release/"
"$release/bin/python" "$release/bundle.py" --verify "$release"
"$release/bin/trace" self-test
# Publish canonical executable paths for diagnostics that inspect PATH only.
"$release/bin/python" - "$release" <<'PY'
import json
import sys
from pathlib import Path
from redteam_agent.runtime.managed_tools import managed_install, manifest
release = Path(sys.argv[1])
for name in json.loads((release / "bundle.json").read_text())["portable"]:
    executable = Path(managed_install(name, release / "tools")["path"])
    (release / "bin" / manifest()["tools"][name]["commands"][0]).symlink_to(executable)
PY
cat > "$release/trace-service" <<'SH'
#!/bin/sh
set -eu
release=$(dirname -- "$(readlink -f -- "$0")")
export TRACE_BIN="$release/bin" TRACE_TOOLS_HOME="$release/tools"
export PATH="$release/bin:$PATH"
export CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true
exec sh "$release/run-trace.sh" "$@"
SH
chmod 700 "$release/trace-service"
# Preserve the service-account ownership fix from install-linux.sh.
if [ "$(id -u)" -eq 0 ]; then
    chown --reference="$prefix" "$prefix/releases"
    chown --reference="$prefix" -R "$release"
fi
ln -sfn current/trace-service "$prefix/trace-service"
if [ -L "$prefix/current" ]; then
    ln -sfn "$(readlink "$prefix/current")" "$prefix/previous"
fi
ln -s "$release" "$prefix/.current.$$"
mv -Tf "$prefix/.current.$$" "$prefix/current"
printf 'Installed offline: %s\nStart: %s/trace-service web\n' "$release" "$prefix"
