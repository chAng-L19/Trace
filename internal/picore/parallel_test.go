package picore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativePiParallelBoundedAndOrdered(t *testing.T) {
	var mu sync.Mutex
	active, peak, modelCalls := 0, 0, 0
	results := []string{}
	started := time.Time{}
	elapsed := time.Duration(0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	err := Run(ctx, func(method string, p json.RawMessage) (any, error) {
		switch method {
		case "prepare":
			return map[string]any{"revision": 1, "base_revision": 0, "message_prefix": 0, "messages": []any{map[string]string{"role": "user", "content": "parallel fixture"}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}, "parallel_tools": []string{"read"}}, nil
		case "model":
			modelCalls++
			calls := []map[string]string{}
			for i := 0; i < 5; i++ {
				calls = append(calls, map[string]string{"id": fmt.Sprint(i), "name": "read", "arguments": "{}"})
			}
			started = time.Now()
			return map[string]any{"tool_calls": calls}, nil
		case "tool":
			mu.Lock()
			active++
			peak = max(peak, active)
			mu.Unlock()
			time.Sleep(160 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return map[string]string{"content": "observed"}, nil
		case "tool_result":
			var result struct {
				CallID string `json:"call_id"`
			}
			_ = json.Unmarshal(p, &result)
			results = append(results, result.CallID)
			return map[string]bool{"ok": true}, nil
		case "finish":
			elapsed = time.Since(started)
			return map[string]string{"action": "end"}, nil
		}
		return nil, fmt.Errorf("unexpected method %s", method)
	}, errors.New("halt"))
	if err != nil {
		t.Fatal(err)
	}
	if peak != 5 || modelCalls != 1 || fmt.Sprint(results) != "[0 1 2 3 4]" {
		t.Fatalf("peak=%d model=%d order=%v", peak, modelCalls, results)
	}
	t.Logf("parallel tool RPC elapsed=%s sequential lower bound=800ms peak=%d", elapsed, peak)
	if elapsed > 550*time.Millisecond {
		t.Fatalf("RPC stayed sequential: %s", elapsed)
	}
}

func TestNativePiParallelHaltDoesNotHidePersistenceFailure(t *testing.T) {
	halt := errors.New("controlled pause")
	disk := errors.New("fixture CAS commit failed")
	entered := make(chan struct{}, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, func(method string, p json.RawMessage) (any, error) {
		switch method {
		case "prepare":
			return map[string]any{"revision": 1, "base_revision": 0, "message_prefix": 0, "messages": []any{map[string]string{"role": "user", "content": "fault"}}, "tools": []any{map[string]any{"function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}}}, "parallel_tools": []string{"read"}}, nil
		case "model":
			return map[string]any{"tool_calls": []map[string]string{{"id": "pause", "name": "read", "arguments": "{}"}, {"id": "disk", "name": "read", "arguments": "{}"}}}, nil
		case "tool":
			entered <- struct{}{}
			for len(entered) < 2 {
				time.Sleep(time.Millisecond)
			}
			var call struct {
				ID string `json:"call_id"`
			}
			_ = json.Unmarshal(p, &call)
			if call.ID == "pause" {
				return nil, halt
			}
			time.Sleep(30 * time.Millisecond)
			return nil, disk
		}
		return nil, fmt.Errorf("unexpected call after halt: %s", method)
	}, halt)
	if !errors.Is(err, disk) || IsHaltOnly(err, halt) {
		t.Fatalf("real persistence error masked: %v", err)
	}
}

func TestIsHaltOnlyRejectsMixedCauses(t *testing.T) {
	halt, disk := errors.New("halt"), errors.New("disk")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false}, {"plain", halt, true}, {"wrapped", fmt.Errorf("boundary: %w", halt), true},
		{"joined-only", errors.Join(halt, fmt.Errorf("again: %w", halt)), true},
		{"mixed", errors.Join(halt, disk), false}, {"nested-mixed", fmt.Errorf("outer: %w", errors.Join(halt, disk)), false},
		{"reverse-mixed", errors.Join(disk, halt), false}, {"real", disk, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHaltOnly(tc.err, halt); got != tc.want {
				t.Fatalf("IsHaltOnly(%v)=%v", tc.err, got)
			}
		})
	}
}

func TestNativePiSingleCallbackJoinedHaltPreservesFailure(t *testing.T) {
	halt, disk, cancelCause := errors.New("controlled pause"), errors.New("fixture CAS commit failed"), errors.New("operator cancelled")
	for _, tc := range []struct {
		name   string
		err    error
		cancel bool
	}{
		{"mixed", errors.Join(halt, disk), false},
		{"nested-mixed", fmt.Errorf("store: %w", errors.Join(halt, disk)), false},
		{"mixed-with-cancel", errors.Join(halt, disk), true},
		{"pure-wrapped", fmt.Errorf("boundary: %w", halt), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			ctx, cancel := context.WithCancelCause(deadline)
			defer cancel(nil)
			calls := 0
			err := Run(ctx, func(method string, _ json.RawMessage) (any, error) {
				calls++
				if method != "prepare" {
					t.Errorf("unexpected callback %s", method)
				}
				if tc.cancel {
					cancel(cancelCause)
				}
				return nil, tc.err
			}, halt)
			if calls != 1 {
				t.Fatalf("unexpected callbacks: %d", calls)
			}
			if tc.name == "pure-wrapped" {
				if !IsHaltOnly(err, halt) {
					t.Fatalf("normal boundary became failure: %v", err)
				}
				return
			}
			if !errors.Is(err, disk) || IsHaltOnly(err, halt) {
				t.Fatalf("joined persistence failure hidden: %v", err)
			}
			if tc.cancel && !errors.Is(err, cancelCause) {
				t.Fatalf("cancel cause lost: %v", err)
			}
		})
	}
}

func TestNativePiParallelPureHaltAllowsLateSuccessfulCallback(t *testing.T) {
	halt := errors.New("controlled pause")
	entered := make(chan struct{}, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, func(method string, p json.RawMessage) (any, error) {
		switch method {
		case "prepare":
			return map[string]any{"revision": 1, "base_revision": 0, "message_prefix": 0, "messages": []any{map[string]string{"role": "user", "content": "normal halt"}}, "tools": []any{map[string]any{"function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}}}, "parallel_tools": []string{"read"}}, nil
		case "model":
			return map[string]any{"tool_calls": []map[string]string{{"id": "pause", "name": "read", "arguments": "{}"}, {"id": "late", "name": "read", "arguments": "{}"}}}, nil
		case "tool":
			entered <- struct{}{}
			for len(entered) < 2 {
				time.Sleep(time.Millisecond)
			}
			var call struct {
				ID string `json:"call_id"`
			}
			_ = json.Unmarshal(p, &call)
			if call.ID == "pause" {
				return nil, fmt.Errorf("boundary: %w", halt)
			}
			time.Sleep(75 * time.Millisecond)
			return map[string]string{"content": "read completed"}, nil
		}
		return nil, fmt.Errorf("unexpected callback after halt: %s", method)
	}, halt)
	if !IsHaltOnly(err, halt) {
		t.Fatalf("normal shutdown/late callback became failure: %v", err)
	}
}

func TestNativePiHaltAndAbnormalHostExitRemainsFailure(t *testing.T) {
	halt := errors.New("controlled pause")
	previous := host
	host += "\nprocess.exitCode = 17;\n"
	defer func() { host = previous }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, func(method string, _ json.RawMessage) (any, error) {
		if method != "prepare" {
			t.Errorf("unexpected callback %s", method)
		}
		return nil, halt
	}, halt)
	if !errors.Is(err, halt) || IsHaltOnly(err, halt) || !strings.Contains(fmt.Sprint(err), "exit status 17") {
		t.Fatalf("abnormal host exit hidden behind halt: %v", err)
	}
}
