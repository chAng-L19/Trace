#!/bin/bash
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
export PATH="$root/runtime/bin:$root/runtime/python/bin:$root/runtime/node/bin:$PATH"
export PYTHONPATH="$root/runtime/cloakbrowser/site-packages:$root/runtime/site-packages:$root/runtime/ASC${PYTHONPATH:+:$PYTHONPATH}"
export PYTHONIOENCODING=utf-8 PYTHONUTF8=1
export TRACE_DATA_DIR="${TRACE_DATA_DIR:-$root/state}"
export TRACE_PROMPT_FILE="$root/prompts/agent.md"
export TRACE_SKILL_PATHS="$root/.trace/skills"
export TRACE_WEB_ANALYSIS_DIR="$root/tools/web-analysis"
export TRACE_PI_RUNTIME="$root/runtime/pi"
export LIGHTPANDA_BINARY_PATH="$root/runtime/lightpanda/lightpanda"
export CLOAKBROWSER_BINARY_PATH="$root/runtime/cloakbrowser/binary/chrome"
export CLOAKBROWSER_AUTO_UPDATE=false
mkdir -p "$TRACE_DATA_DIR" "$root/workspace"
cd "$root/workspace"
if [[ $# == 0 ]]; then set -- serve --addr 127.0.0.1:8765; fi
exec "$root/bin/trace" "$@"
