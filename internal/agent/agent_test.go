package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type sequenceProvider struct {
	mu sync.Mutex
	n  int
}

type successProvider struct{}

func (successProvider) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{Content: "ok"}, nil
}

type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingProvider) Complete(ctx context.Context, _ ModelRequest) (ModelResponse, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return ModelResponse{Content: "ok"}, nil
	case <-ctx.Done():
		return ModelResponse{}, ctx.Err()
	}
}

func (p *sequenceProvider) Complete(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	if p.n == 1 {
		return ModelResponse{}, errors.New("injected outage")
	}
	return ModelResponse{Content: "recovered"}, nil
}

func TestPromptIsPersistedAndUsedAsSystemMessage(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, "prompts.md")
	prompt := "trace runtime prompt"
	if err := os.WriteFile(promptPath, []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	runner, err := New(store, MockProvider{}, promptPath)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := runner.Create("inspect target")
	if err != nil {
		t.Fatal(err)
	}
	finished, err := runner.Run(context.Background(), snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Run.Status != StatusCompleted || finished.Run.EventSeq != 5 {
		t.Fatalf("unexpected run: %+v", finished.Run)
	}
	prepared := 0
	for _, event := range finished.Events {
		if event.Type == "recon_prepared" {
			prepared++
		}
	}
	if prepared != 1 {
		t.Fatalf("startup inventory should add one deterministic event: %d", prepared)
	}
	if len(finished.Requests) != 1 || finished.Requests[0].Messages[0].Role != "system" || finished.Requests[0].Messages[0].Content != prompt+runtimeDataHint || finished.Requests[0].Messages[0].CreatedAt.IsZero() {
		t.Fatalf("prompt was not used as system message: %+v", finished.Requests)
	}
	if !strings.Contains(finished.Messages[1].Content, finished.Run.PromptHash[:12]) {
		t.Fatalf("mock response missing prompt hash: %q", finished.Messages[1].Content)
	}
	reloaded, err := store.Load(snap.Run.ID)
	if err != nil || reloaded.Prompt != prompt {
		t.Fatalf("prompt snapshot was not persisted: %v", err)
	}
}

func TestEventSequenceStartsAtOnePerRun(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(filepath.Join(root, "state"))
	runner, _ := New(store, MockProvider{}, promptPath)
	first, _ := runner.Create("one")
	second, _ := runner.Create("two")
	if first.Events[0].Sequence != 1 || second.Events[0].Sequence != 1 {
		t.Fatalf("sequence leaked between runs: %d %d", first.Events[0].Sequence, second.Events[0].Sequence)
	}
}

func TestFailedRunCanResumeAndClearsPreviousError(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &sequenceProvider{}
	runner, err := New(store, provider, promptPath)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := runner.Create("retry")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runner.Run(context.Background(), snap.Run.ID); err == nil {
		t.Fatal("expected provider failure")
	}
	failed, _ := store.Load(snap.Run.ID)
	if failed.Run.Status != StatusFailed || failed.Run.Error == "" {
		t.Fatalf("failure not persisted: %+v", failed.Run)
	}
	finished, err := runner.Run(context.Background(), snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Run.Status != StatusCompleted || finished.Run.Error != "" {
		t.Fatalf("retry state invalid: %+v", finished.Run)
	}
}

func TestConcurrentRunHasSingleOwner(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(filepath.Join(root, "state"))
	provider := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	runner, _ := New(store, provider, promptPath)
	snap, _ := runner.Create("concurrency")
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes, activeErrors int
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := runner.Run(context.Background(), snap.Run.ID)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else if errors.Is(err, ErrRunLockBusy) {
				activeErrors++
			}
		}()
	}
	close(start)
	<-provider.entered
	time.Sleep(20 * time.Millisecond)
	close(provider.release)
	wg.Wait()
	if successes != 1 || activeErrors != 1 {
		t.Fatalf("owner lock failed: successes=%d active_errors=%d", successes, activeErrors)
	}
}

type emptyProvider struct{}

func (emptyProvider) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{}, nil
}

func TestEmptyReplyFailsAndUnavailableToolContinues(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(filepath.Join(root, "state"))
	empty, _ := New(store, emptyProvider{}, promptPath)
	emptyRun, _ := empty.Create("empty")
	if _, err := empty.Run(context.Background(), emptyRun.Run.ID); err == nil {
		t.Fatal("empty response should fail")
	}
	failed, _ := store.Load(emptyRun.Run.ID)
	if failed.Run.Status != StatusFailed {
		t.Fatalf("empty response status: %+v", failed.Run)
	}
	provider := manualProvider(ToolCall{ID: "call-1", Name: "echo", Arguments: `{"value":"x"}`})
	tool, _ := New(store, provider, promptPath)
	toolRun, _ := tool.Create("tool")
	finished, err := tool.Run(piMigrationContext(t), toolRun.Run.ID)
	if err != nil || finished.Run.Status != StatusCompleted || len(provider.requests) != 2 || len(finished.Actions) != 0 || len(finished.Evidence) != 1 || finished.Evidence[0].Verification != "unverified" {
		t.Fatalf("unavailable tool did not continue without execution: %+v %v", finished.Run, err)
	}
	if message := lastConversationMessage(provider.requests[1]); message.Role != "tool" || !strings.Contains(message.Content, "was not executed") {
		t.Fatalf("missing tool diagnostic was not returned to the model: %+v", message)
	}
}

func TestPromptCorruptionFailsRun(t *testing.T) {
	root := t.TempDir()
	promptPath := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := NewStore(filepath.Join(root, "state"))
	runner, _ := New(store, successProvider{}, promptPath)
	snap, _ := runner.Create("corrupt")
	_, err := store.Update(snap.Run.ID, func(s *Snapshot) error { s.Prompt = "changed"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), snap.Run.ID); err == nil {
		t.Fatal("expected prompt mismatch")
	}
	failed, _ := store.Load(snap.Run.ID)
	if failed.Run.Status != StatusFailed {
		t.Fatalf("prompt corruption status: %+v", failed.Run)
	}
}

func TestConcurrentStoreUpdatesPreserveEvents(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("goal", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.Update(snap.Run.ID, func(s *Snapshot) error { addEvent(s, "base", "base", nil); return nil })
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.Update(snap.Run.ID, func(s *Snapshot) error { addEvent(s, "concurrent", "concurrent", nil); return nil })
		}()
	}
	wg.Wait()
	final, err := store.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Events) != 3 || final.Run.EventSeq != 3 {
		t.Fatalf("lost concurrent event: events=%d seq=%d", len(final.Events), final.Run.EventSeq)
	}
}
