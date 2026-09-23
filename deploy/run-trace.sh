#!/bin/sh
set -eu
umask 077

prefix=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
export TRACE_HOME="${TRACE_HOME:-${XDG_STATE_HOME:-$HOME/.local/state}/trace}"
export REDTEAM_AGENT_HOME="$TRACE_HOME"
export XDG_CACHE_HOME="${XDG_CACHE_HOME:-$HOME/.cache}"
export TRACE_TOOLS_HOME="${TRACE_TOOLS_HOME:-$XDG_CACHE_HOME/trace/tools}"
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-$XDG_CACHE_HOME/ms-playwright}"
export PYTHONDONTWRITEBYTECODE=1
trace_bin=${TRACE_BIN:-$prefix/current/bin}
mkdir -p "$TRACE_HOME" "$XDG_CACHE_HOME"

# Optional mounted secret files are read only at runtime and never printed.
if [ -n "${TRACE_ADMIN_PASSWORD_FILE:-}" ]; then
    TRACE_ADMIN_PASSWORD=$(cat "$TRACE_ADMIN_PASSWORD_FILE")
    export TRACE_ADMIN_PASSWORD
fi
if [ -n "${OPENAI_API_KEY_FILE:-}" ]; then
    OPENAI_API_KEY=$(cat "$OPENAI_API_KEY_FILE")
    export OPENAI_API_KEY
fi

mode=${1:-web}
if [ "$#" -gt 0 ]; then shift; fi
case "$mode" in
    web) exec "$trace_bin/trace-web" --root "$TRACE_HOME" \
        --host "${TRACE_WEB_HOST:-127.0.0.1}" --port "${TRACE_WEB_PORT:-8765}" "$@" ;;
    mcp) exec "$trace_bin/trace-mcp" --root "$TRACE_HOME" "$@" ;;
    cli) exec "$trace_bin/trace" "$@" ;;
    *) exec "$mode" "$@" ;;
esac
