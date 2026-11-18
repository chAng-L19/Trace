package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"trace/internal/agent"
)

// Keep host execution capabilities, not ambient product configuration. In
// particular a copied binary still needs the externally provisioned exec root.
func standaloneRuntimeEnv(name string) bool {
	switch strings.ToUpper(name) {
	case "TRACE_PI_RUNTIME", "TRACE_PI_NODE", "TRACE_EXEC_CGROUP_ROOT", "TRACE_EXEC_RESOURCE_MODE",
		"TRACE_EXEC_MEMORY_BYTES", "TRACE_EXEC_PIDS", "TRACE_EXEC_OUTPUT_BYTES":
		return true
	}
	return false
}

func standaloneEnv(environ []string) []string {
	var env []string
	for _, value := range environ {
		name := strings.SplitN(value, "=", 2)[0]
		if !strings.HasPrefix(strings.ToUpper(name), "TRACE_") || standaloneRuntimeEnv(name) {
			env = append(env, value)
		}
	}
	return env
}

// Runtime context and audit continuations can follow a tool observation. Route
// these fixed fixtures by their call identity, never by the last message role.
func standaloneToolObserved(messages []agent.Message, callID string) bool {
	for _, message := range messages {
		if message.Role == "tool" && message.ToolCallID == callID {
			return true
		}
	}
	return false
}

func standaloneReport(content string) string {
	report := agent.ReportText{
		ZhCN: "检查已观察到预期标记。复现使用固定命令，结论仅适用于本次观察；清理状态仍为未知，尚无独立影响验证。",
		En:   "The check observed the expected marker. Reproduction uses the recorded command and the conclusion applies only to this observation. Cleanup remains unknown and independent impact validation is still pending.",
	}
	raw, _ := json.Marshal(map[string]any{"trace_report": report})
	return content + "\n\n```json\n" + string(raw) + "\n```"
}

func TestStandaloneHarnessPreservesRuntimeNotConfiguration(t *testing.T) {
	keep := []string{"PATH=/fixture/bin", "TRACE_PI_RUNTIME=/fixture/runtime", "TRACE_PI_NODE=/fixture/node",
		"TRACE_EXEC_CGROUP_ROOT=/fixture/exec", "TRACE_EXEC_RESOURCE_MODE=process-group", "TRACE_EXEC_MEMORY_BYTES=268435456", "TRACE_EXEC_PIDS=64", "TRACE_EXEC_OUTPUT_BYTES=8388608"}
	input := append(append([]string(nil), keep...), "TRACE_PROVIDER_KEY=private", "TRACE_CONFIG_FILE=/ambient.json",
		"TRACE_MAX_ROUNDS=1", "trace_provider_url=http://ambient.invalid", "TRACE_EXEC_UNRECOGNIZED=drop")
	if got := standaloneEnv(input); !reflect.DeepEqual(got, keep) {
		t.Fatalf("standalone environment mismatch: got=%q want=%q", got, keep)
	}
}

func TestStandaloneHarnessMatchesToolObservationBeforeRuntimeSuffix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []agent.Message
		observed bool
	}{
		{"empty", nil, false},
		{"initial", []agent.Message{{Role: "user", Content: "objective"}}, false},
		{"other_call", []agent.Message{{Role: "tool", ToolCallID: "other"}}, false},
		{"matching_call", []agent.Message{{Role: "tool", ToolCallID: "fixture-call"}}, true},
		{"runtime_suffix", []agent.Message{{Role: "tool", ToolCallID: "fixture-call"}, {Role: "user", Content: "runtime state"}}, true},
		{"audit_suffix", []agent.Message{{Role: "tool", ToolCallID: "fixture-call"}, {Role: "assistant", Content: "reported"}, {Role: "user", Content: "audit continuation"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := standaloneToolObserved(tc.messages, "fixture-call"); got != tc.observed {
				t.Fatalf("observation=%v want=%v", got, tc.observed)
			}
		})
	}
}
