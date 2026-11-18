#!/bin/bash
set -euo pipefail
source_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/trace-entrypoint-test.XXXXXX")
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/state/service"
printf 'fixture goal\n' > "$test_root/task.md"
# Exercise real mode selection and the missing-delegation negative control.
# Positive cgroup delegation and clone3 still require the isolated server gate.
sed -e "s|/workspace|$test_root/workspace|g" \
    -e "s|/opt/trace/examples/tsecbench-task.md|$test_root/task.md|g" \
    "$source_root/entrypoint.sh" > "$test_root/entrypoint.sh"
cat > "$test_root/bin/trace" <<'SH'
#!/bin/bash
set -euo pipefail
case "$1" in
  create)
    [[ "${CLOAKBROWSER_BINARY_PATH:-}" == /opt/trace/runtime/cloakbrowser/binary/chrome ]]
    [[ "${CLOAKBROWSER_AUTO_UPDATE:-}" == false ]]
    [[ "${TRACE_REQUIRE_VERIFIED_COMPLETION:-}" == true ]]
    printf 'create\n' >> "$TRACE_FIXTURE_CREATE_LOG"
    printf '{"id":"fixture-run"}\n'
    exit "${TRACE_FIXTURE_CREATE_EXIT:-0}"
    ;;
  run)
    [[ "${TRACE_REQUIRE_VERIFIED_COMPLETION:-}" == true ]]
    [[ "$2" == fixture-run ]]
    printf '%s\n' "$2" >> "$TRACE_FIXTURE_RUN_LOG"
    if [[ -n ${TRACE_FIXTURE_HOLD:-} ]]; then
      : > "$TRACE_FIXTURE_HOLD"
      sleep 2
    fi
    ;;
  serve) printf 'serve\n' >> "$TRACE_FIXTURE_SERVE_LOG" ;;
  *) exit 99 ;;
esac
SH
chmod +x "$test_root/bin/trace"
export PATH="$test_root/bin:$PATH" TRACE_DATA_DIR="$test_root/state"
export BENCHMARK_BASE_URL=fixture BENCHMARK_TOKEN=fixture TRACE_PROVIDER_KEY=fixture
export TRACE_PROVIDER_URL=http://provider-fixture.tsecbench.gw/v1 TRACE_MODEL=fixture
cat > "$test_root/bin/curl" <<'SH'
#!/bin/bash
printf '%s\n' '{"status":"ok"}'
SH
chmod +x "$test_root/bin/curl"
export TRACE_FIXTURE_CREATE_LOG="$test_root/creates" TRACE_FIXTURE_RUN_LOG="$test_root/runs"
export TRACE_FIXTURE_SERVE_LOG="$test_root/serves"
sentinel="$TRACE_DATA_DIR/service/hosted-run.json"
# Another invocation's temporary file must survive our cleanup.
foreign="$TRACE_DATA_DIR/service/.hosted-run.foreign"
printf 'preserve\n' > "$foreign"
# Empty mode retains the strict server default. All execution entrances reject
# missing cgroups and unknown modes before creating/resuming any Run.
for mode in '' cgroup typo; do
  for command in run serve benchmark; do
    status=0
    TRACE_EXEC_RESOURCE_MODE="$mode" TRACE_EXEC_CGROUP_ROOT= \
      bash "$test_root/entrypoint.sh" "$command" fixture-run > "$test_root/isolation-output" 2>&1 || status=$?
    [[ $status == 2 && ! -e "$sentinel" && ! -e "$TRACE_FIXTURE_CREATE_LOG" && ! -e "$TRACE_FIXTURE_RUN_LOG" && ! -e "$TRACE_FIXTURE_SERVE_LOG" ]]
    if [[ "$mode" == typo ]]; then
      grep -q 'unknown TRACE_EXEC_RESOURCE_MODE' "$test_root/isolation-output"
    else
      grep -q 'TRACE_EXEC_CGROUP_ROOT must name' "$test_root/isolation-output"
    fi
  done
done
export TRACE_EXEC_RESOURCE_MODE=process-group TRACE_EXEC_CGROUP_ROOT=
TRACE_REQUIRE_VERIFIED_COMPLETION=true TRACE_FIXTURE_RUN_LOG="$test_root/probe-runs" \
  bash "$test_root/entrypoint.sh" run fixture-run > "$test_root/compat-output" 2>&1
bash "$test_root/entrypoint.sh" serve > "$test_root/compat-output" 2>&1
[[ $(cat "$test_root/probe-runs") == fixture-run && $(cat "$TRACE_FIXTURE_SERVE_LOG") == serve ]]
grep -q 'per-command memory/PID hard limits.*are absent' "$test_root/compat-output"
assert_no_owned_temp() {
  local pending=("$TRACE_DATA_DIR"/service/.hosted-run.*)
  [[ ${#pending[@]} == 1 && "${pending[0]}" == "$foreign" ]]
  [[ $(cat "$foreign") == preserve ]]
}
expect_create_failure() {
  local status=0
  TRACE_FIXTURE_CREATE_EXIT=19 bash "$test_root/entrypoint.sh" > "$test_root/output" 2>&1 || status=$?
  [[ $status == 19 && ! -s "$sentinel" && ! -e "$TRACE_FIXTURE_RUN_LOG" ]]
  assert_no_owned_temp
}
expect_create_failure
[[ ! -e "$sentinel" ]]
# Upgrade recovery: an empty sentinel left by the old entrypoint is retried.
: > "$sentinel"
expect_create_failure
bash "$test_root/entrypoint.sh" > "$test_root/output" 2>&1
[[ $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$sentinel") == fixture-run ]]
[[ $(wc -l < "$TRACE_FIXTURE_CREATE_LOG") == 3 ]]
assert_no_owned_temp
# A valid existing run is resumed, even if a fresh create would fail.
TRACE_FIXTURE_CREATE_EXIT=19 bash "$test_root/entrypoint.sh" > "$test_root/output" 2>&1
[[ $(wc -l < "$TRACE_FIXTURE_CREATE_LOG") == 3 ]]
[[ $(wc -l < "$TRACE_FIXTURE_RUN_LOG") == 2 ]]
assert_no_owned_temp
# Fresh-state contenders must not create or execute a second Run; the lock
# must remain held after the shell execs the fixture runtime.
rm -- "$sentinel"
TRACE_FIXTURE_HOLD="$test_root/held" bash "$test_root/entrypoint.sh" > "$test_root/owner-output" 2>&1 &
owner=$!
for ((i=0; i<100; i++)); do
  [[ -e $test_root/held ]] && break
  kill -0 "$owner"
  sleep 0.02
done
[[ -e $test_root/held ]]
status=0
bash "$test_root/entrypoint.sh" > "$test_root/contender-output" 2>&1 || status=$?
[[ $status == 75 ]]
[[ $(wc -l < "$TRACE_FIXTURE_CREATE_LOG") == 4 ]]
[[ $(wc -l < "$TRACE_FIXTURE_RUN_LOG") == 3 ]]
wait "$owner"
bash "$test_root/entrypoint.sh" > "$test_root/resumed-output" 2>&1
[[ $(wc -l < "$TRACE_FIXTURE_CREATE_LOG") == 4 ]]
[[ $(wc -l < "$TRACE_FIXTURE_RUN_LOG") == 4 ]]
assert_no_owned_temp
printf 'entrypoint mode preflight, sentinel recovery and lifecycle ownership: PASS\n'
