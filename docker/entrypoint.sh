#!/bin/bash
set -euo pipefail
# Read-only preflight for a host-provisioned cgroup layout. The image grants no
# delegation and never moves PID 1. Ordinary containers opt in explicitly.
require_exec_isolation() {
  isolation_fail() {
    printf 'Trace exec isolation: %s; host-delegated bounded controller/exec cgroups required; target not started.\n' "$*" >&2
    exit 2
  }
  isolation_value() { cat -- "$1" || isolation_fail "read $1"; }
  isolation_limit() {
    local value
    value=$(isolation_value "$1")
    [[ "$value" =~ ^[1-9][0-9]*$ && ${#value} -le 15 ]] || isolation_fail "finite positive limit required: $1"
    printf '%s' "$value"
  }
  isolation_controllers() {
    local value
    value=" $(isolation_value "$1") "
    [[ "$value" == *' memory '* && "$value" == *' pids '* ]] || isolation_fail "memory+pids delegation absent: $1"
  }
  isolation_process() {
    local relative
    relative=$(sed -n 's/^0:://p' "$1")
    [[ "$relative" == /* ]] || isolation_fail "cgroup v2 process path absent: $1"
    readlink -e -- "/sys/fs/cgroup$relative" || isolation_fail "cgroup mount does not match $1"
  }
  local root parent controller path entry cap control_cap exec_cap name margin
  [[ "${TRACE_EXEC_CGROUP_ROOT:-}" == /* ]] || isolation_fail 'TRACE_EXEC_CGROUP_ROOT must name an absolute delegated exec cgroup'
  root=$(readlink -e -- "$TRACE_EXEC_CGROUP_ROOT") || isolation_fail 'exec cgroup is missing'
  [[ "${root##*/}" == exec ]] || isolation_fail 'aggregate must be named exec'
  [[ "$(stat -f -c %T -- "$root")" == cgroup2fs ]] || isolation_fail 'exec root is not cgroup v2'
  parent=${root%/*}
  controller=$parent/controller
  [[ "$(isolation_process /proc/self/cgroup)" == "$controller" ]] || isolation_fail 'entrypoint is outside the bounded controller sibling'
  [[ "$(isolation_process /proc/1/cgroup)" == "$controller" ]] || isolation_fail 'container init/tini is outside the bounded controller sibling'
  for entry in "$parent"/* "$parent"/.[!.]* "$parent"/..?*; do
    [[ ! -d "$entry" || "$entry" == "$controller" || "$entry" == "$root" ]] || isolation_fail 'resource parent has an unaccounted sibling'
  done
  for path in "$parent" "$root"; do
    [[ -z "$(isolation_value "$path/cgroup.procs")" ]] || isolation_fail "resource parent/aggregate is populated: $path"
    isolation_controllers "$path/cgroup.subtree_control"
    [[ -w "$path/cgroup.procs" ]] || isolation_fail "cgroup migration is not delegated: $path"
  done
  [[ -w "$root" && -x "$root" && -w "$root/cgroup.subtree_control" && -w "$root/cgroup.kill" ]] || isolation_fail 'exec cgroup is read-only or not delegated to the runtime UID'
  for path in "$parent" "$controller" "$root"; do
    [[ "$(isolation_value "$path/memory.swap.max")" == 0 ]] || isolation_fail "memory.swap.max must be zero: $path"
  done
  # Keep these minimum margins in sync with internal/worker/resources_linux.go.
  for name in memory.max pids.max; do
    margin=268435456
    [[ "$name" != pids.max ]] || margin=64
    cap=$(isolation_limit "$parent/$name")
    control_cap=$(isolation_limit "$controller/$name")
    exec_cap=$(isolation_limit "$root/$name")
    (( cap > margin && control_cap <= cap - margin && exec_cap <= cap - margin - control_cap )) || isolation_fail "parent $name lacks controller + exec + headroom"
    if [[ "$name" == memory.max ]]; then
      (( exec_cap > 67108864 )) || isolation_fail 'exec aggregate requires more than 64MiB'
    else
      (( exec_cap > 8 )) || isolation_fail 'exec aggregate requires more than 8 tasks'
    fi
  done
  # clone3(CLONE_INTO_CGROUP), leaf creation and cgroup.kill are finally verified
  # by the unchanged worker on each exec; this preflight does not prove them.
}
require_exec_resources() {
  case "${TRACE_EXEC_RESOURCE_MODE:-cgroup}" in
    cgroup) require_exec_isolation ;;
    process-group)
      printf 'Trace exec resources: process-group container compatibility mode; cancellation and shared output budget remain enabled; per-command memory/PID hard limits and controller/exec cgroup isolation are absent.\n' >&2
      ;;
    *)
      printf 'Trace exec resources: unknown TRACE_EXEC_RESOURCE_MODE=%s; expected cgroup or process-group; target not started.\n' "$TRACE_EXEC_RESOURCE_MODE" >&2
      exit 2
      ;;
  esac
}
export LIGHTPANDA_BINARY_PATH="${LIGHTPANDA_BINARY_PATH:-/opt/trace/runtime/lightpanda/lightpanda}"
export CLOAKBROWSER_BINARY_PATH=/opt/trace/runtime/cloakbrowser/binary/chrome
export CLOAKBROWSER_AUTO_UPDATE=false
if [[ "${1:-benchmark}" != benchmark ]]; then
  case "$1" in run|serve) require_exec_resources ;; esac
  exec trace "$@"
fi
: "${BENCHMARK_BASE_URL:?平台应注入 BENCHMARK_BASE_URL}"
: "${BENCHMARK_TOKEN:?平台应注入 BENCHMARK_TOKEN}"
: "${TRACE_PROVIDER_KEY:?请通过运行环境注入模型 API key}"
: "${TRACE_PROVIDER_URL:?请通过运行环境注入模型网关地址}"
: "${TRACE_MODEL:?请通过运行环境注入模型名称}"
require_exec_resources
if ! vpn_status=$(curl -fsS --connect-timeout 5 --max-time 10 http://10.0.100.58 | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status", ""))'); then
  printf 'TSecBench VPN health check failed; check the arena network.\n' >&2
  exit 1
fi
if [[ "$vpn_status" != ok ]]; then
  printf 'TSecBench VPN health check failed; status=%s\n' "${vpn_status:-unavailable}" >&2
  exit 1
fi
export TRACE_REQUIRE_VERIFIED_COMPLETION=true
export TRACE_MAX_SECONDS="${TRACE_MAX_SECONDS:-21180}"
export TRACE_MAX_ROUNDS="${TRACE_MAX_ROUNDS:-0}"
export TRACE_MAX_ACTIONS="${TRACE_MAX_ACTIONS:-0}"
export TRACE_BROWSER_NO_SANDBOX=1
export TRACE_DATA_DIR="${TRACE_DATA_DIR:-/state}"
mkdir -p "$TRACE_DATA_DIR/service" /workspace
# Keep one benchmark owner for this state through exec trace run.
exec 9>>"$TRACE_DATA_DIR/service/hosted-run.lock"
flock -n 9 || { printf 'Trace benchmark state is already in use.\n' >&2; exit 75; }
cd /workspace
if [[ ! -s "$TRACE_DATA_DIR/service/hosted-run.json" ]]; then
  (
    pending_run=$(mktemp "$TRACE_DATA_DIR/service/.hosted-run.XXXXXX")
    trap 'rm -f -- "$pending_run"' EXIT
    trace create "$(cat /opt/trace/examples/tsecbench-task.md)" > "$pending_run"
    mv -- "$pending_run" "$TRACE_DATA_DIR/service/hosted-run.json"
  )
fi
run_id=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$TRACE_DATA_DIR/service/hosted-run.json")
printf 'Trace 已启动会话 %s；证据保存在 %s\n' "$run_id" "$TRACE_DATA_DIR"
exec trace run "$run_id"
