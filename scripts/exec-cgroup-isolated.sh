#!/bin/sh
# NEW isolated delegated unit only; never edits an existing service. Defaults:
# controller 1GiB/128 tasks + exec 1.5GiB/384 + >=256MiB/64 parent headroom.
# MemoryMax >=2952790016 (2816MiB), MemorySwapMax=0, TasksMax >=576.
# Compile separately in a bounded build unit, then run a prebuilt test binary.
# Example test-only profile in a 2GiB/768 unit: set controller and aggregate
# memory to 805306368 each; do not assume that this is enough for a compiler.
# This reserves space inside this unit, not against unrelated host workloads.
set -eu
fail() { printf '%s\n' "exec isolation: $*; target not started" >&2; exit 2; }
positive() {
    case "$2" in ''|*[!0-9]*|0*) fail "$1 must be a canonical positive integer";; esac
    [ "${#2}" -le 15 ] || fail "$1 exceeds supported integer range"
}
read_value() { cat "$1" || fail "read $1"; }
set_value() {
    printf '%s\n' "$2" > "$1" || fail "write $1"
    [ "$(read_value "$1")" = "$2" ] || fail "readback mismatch: $1"
}
has_controllers() {
    value=" $(read_value "$1") "
    case "$value" in *' memory '*) ;; *) fail "memory controller absent: $1";; esac
    case "$value" in *' pids '*) ;; *) fail "pids controller absent: $1";; esac
}
[ "$#" -gt 0 ] || fail 'command required'
controller_memory=${TRACE_CONTROLLER_MEMORY_BYTES:-1073741824}
controller_pids=${TRACE_CONTROLLER_PIDS:-128}
exec_memory=${TRACE_EXEC_AGGREGATE_MEMORY_BYTES:-1610612736}
exec_pids=${TRACE_EXEC_AGGREGATE_PIDS:-384}
positive controller_memory "$controller_memory"
positive controller_pids "$controller_pids"
positive exec_memory "$exec_memory"
positive exec_pids "$exec_pids"
# Go's soft GC target leaves controller headroom for pi and JSON encoding;
# the cgroup hard limit stays unchanged. Preserve every explicit setting.
if [ "${GOMEMLIMIT+x}" != x ]; then
    GOMEMLIMIT=$((controller_memory / 2))
    export GOMEMLIMIT
fi
# Keep these minimum margins in sync with resources_linux.go.
memory_headroom=268435456
pids_headroom=64
[ "$exec_memory" -gt 67108864 ] || fail 'exec aggregate requires >64MiB'
[ "$exec_pids" -gt 8 ] || fail 'exec aggregate requires >8 tasks'
relative=$(sed -n 's/^0:://p' /proc/self/cgroup)
case "$relative" in /*) ;; *) fail 'cgroup v2 required';; esac
[ "$relative" != / ] || fail 'delegated non-root unit required'
root=/sys/fs/cgroup$relative
[ "$(stat -f -c %T "$root")" = cgroup2fs ] || fail 'standard cgroup v2 mount required'
has_controllers "$root/cgroup.controllers"
unit_memory=$(read_value "$root/memory.max")
unit_pids=$(read_value "$root/pids.max")
positive MemoryMax "$unit_memory"
positive TasksMax "$unit_pids"
[ "$(read_value "$root/memory.swap.max")" = 0 ] || fail 'MemorySwapMax=0 required'
[ "$unit_memory" -ge "$((controller_memory + exec_memory + memory_headroom))" ] || fail 'MemoryMax cannot contain controller + exec + 256MiB headroom'
[ "$unit_pids" -ge "$((controller_pids + exec_pids + pids_headroom))" ] || fail 'TasksMax cannot contain controller + exec + 64 tasks headroom'
# No reuse, unaccounted siblings, or extra resident processes. cgroup.procs is
# read without a subprocess (a cat here would itself appear in the same unit).
for entry in "$root"/*; do [ ! -d "$entry" ] || fail 'unit already has child cgroups'; done
seen_self=0
while IFS= read -r pid; do
    [ "$pid" = "$$" ] || fail 'unit contains another process'
    seen_self=1
done < "$root/cgroup.procs"
[ "$seen_self" = 1 ] || fail 'wrapper not resident in unit'
# Moving a task does not migrate already charged memory. Reject a unit whose
# initial charges already consume the fixed parent margin.
initial_memory=$(read_value "$root/memory.current")
[ "$initial_memory" -lt "$memory_headroom" ] || fail 'initial unit charges consume parent headroom'
mkdir "$root/controller" || fail 'create controller cgroup'
printf '%s\n' "$$" > "$root/controller/cgroup.procs" || fail 'move wrapper to controller'
printf '+memory +pids\n' > "$root/cgroup.subtree_control" || fail 'delegate memory+pids'
has_controllers "$root/cgroup.subtree_control"
set_value "$root/controller/memory.max" "$controller_memory"
set_value "$root/controller/memory.swap.max" 0
set_value "$root/controller/pids.max" "$controller_pids"
set_value "$root/controller/memory.oom.group" 1
mkdir "$root/exec" || fail 'create exec aggregate'
set_value "$root/exec/memory.max" "$exec_memory"
set_value "$root/exec/memory.swap.max" 0
set_value "$root/exec/pids.max" "$exec_pids"
printf '+memory +pids\n' > "$root/exec/cgroup.subtree_control" || fail 'delegate leaf controllers'
has_controllers "$root/exec/cgroup.subtree_control"
export TRACE_EXEC_CGROUP_ROOT="$root/exec"
export TRACE_TEST_CGROUP_ROOT="$root/exec"
# Managed HTTP command takes precedence over the host's native curl.
release_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
[ -f "$release_root/runtime/bin/curl" ] && [ -x "$release_root/runtime/bin/curl" ] || fail 'managed curl-cffi entry missing; install frozen HTTP runtime'
PATH="$release_root/runtime/bin:$PATH"
export PATH
exec "$@"
