#!/bin/sh
# Install a local release wheel into a fresh user-owned venv; keep the old release.
set -eu
umask 077

wheel=${1:?Usage: sh deploy/install-linux.sh /absolute/path/trace_agent-VERSION-py3-none-any.whl}
python=${TRACE_PYTHON:-python3}
"$python" -c 'import sys; assert sys.platform == "linux", "Linux required"; assert (3, 11) <= sys.version_info[:2] <= (3, 13), "Python 3.11-3.13 required"'
case "$wheel" in /*.whl) ;; *) printf '%s\n' 'Provide an absolute wheel path.' >&2; exit 2 ;; esac
[ -f "$wheel" ] || { printf '%s\n' 'Wheel does not exist.' >&2; exit 2; }
prefix=${TRACE_INSTALL_PREFIX:-${XDG_DATA_HOME:-$HOME/.local/share}/trace}
case "$prefix" in /*) ;; *) printf '%s\n' 'Install prefix must be absolute.' >&2; exit 2 ;; esac
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
mkdir -p "$prefix/releases"
release=$(mktemp -d "$prefix/releases/release-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")
"$python" -m venv "$release"
"$release/bin/python" -m pip install --disable-pip-version-check "$wheel"
"$release/bin/python" -m pip check
"$release/bin/trace" self-test
cp "$script_dir/run-trace.sh" "$release/trace-service"
chmod 700 "$release/trace-service"
ln -sfn current/trace-service "$prefix/trace-service"
if [ -L "$prefix/current" ]; then
    ln -sfn "$(readlink "$prefix/current")" "$prefix/previous"
fi
ln -s "$release" "$prefix/.current.$$"
mv -Tf "$prefix/.current.$$" "$prefix/current"
printf 'Installed: %s\nStart: %s/trace-service web\n' "$release" "$prefix"
