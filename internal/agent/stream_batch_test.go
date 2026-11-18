package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"trace/internal/worker"
)

func TestModelStreamBatchDrainsAndPreservesFragments(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var saved []StreamDelta
	writes := 0
	emit, drain := modelStreamBatch(ctx, cancel, func(d []StreamDelta) error {
		writes++
		saved = append(saved, d...)
		return nil
	})
	for i := 0; i < 2000; i++ {
		if err := emit(StreamDelta{Kind: "thinking", Text: "中"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []StreamDelta{{Kind: "tool_call", Index: 0, ID: "c", Name: "ex", Arguments: "{"}, {Kind: "tool_call", Index: 0, Name: "ec", Arguments: "}"}, {Kind: "text", Text: "tail"}} {
		if err := emit(d); err != nil {
			t.Fatal(err)
		}
	}
	cancel() // Accepted tail must survive cancellation.
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	var thinking, name, args, tail string
	for _, d := range saved {
		if d.Kind == "thinking" {
			thinking += d.Text
		}
		if d.Kind == "tool_call" {
			name += d.Name
			args += d.Arguments
		}
		if d.Kind == "text" {
			tail += d.Text
		}
	}
	if thinking != strings.Repeat("中", 2000) || name != "exec" || args != "{}" || tail != "tail" || writes >= 20 {
		t.Fatalf("lost fragments or excessive writes: thinking=%d name=%s args=%s tail=%s writes=%d", len(thinking), name, args, tail, writes)
	}
}

func TestModelStreamBatchPropagatesPersistenceFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := errors.New("fixture disk failure")
	emit, drain := modelStreamBatch(ctx, cancel, func([]StreamDelta) error { return want })
	if err := emit(StreamDelta{Kind: "text", Text: "accepted"}); err != nil {
		t.Fatal(err)
	}
	if err := drain(); !errors.Is(err, want) {
		t.Fatalf("lost failure: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("provider not cancelled after persistence failure")
	}
}

func TestModelStreamBatchBlockedCommitDrainsAcceptedBacklogBeforeStaleTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
	secondCommitted := make(chan []StreamDelta, 1)
	var batches [][]StreamDelta
	emit, drain := modelStreamBatchTicks(ctx, cancel, func(d []StreamDelta) error {
		copy := append([]StreamDelta(nil), d...)
		batches = append(batches, copy)
		if len(batches) == 1 {
			close(firstEntered)
			<-releaseFirst
		} else if len(batches) == 2 {
			secondCommitted <- copy
		}
		return nil
	}, ticks, func() {})
	if err := emit(StreamDelta{Kind: "thinking", Text: strings.Repeat("x", 64<<10)}); err != nil {
		t.Fatal(err)
	}
	<-firstEntered // The threshold commit is blocked, so the queue cannot drain.
	var expected strings.Builder
	for i := 0; i < 64; i++ {
		text := fmt.Sprintf("frame-%02d;", i)
		expected.WriteString(text)
		if err := emit(StreamDelta{Kind: "text", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	ticks <- time.Unix(1, 0) // The only tick represents a stale, already due flush.
	close(releaseFirst)
	var second []StreamDelta
	select {
	case second = <-secondCommitted:
	case <-time.After(5 * time.Second):
		cancel()
		if err := drain(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("stale tick was reset instead of flushing accepted backlog")
	}
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].Text != expected.String() || len(batches) != 2 {
		t.Fatalf("backlog split into extra full-snapshot writes: batches=%d second=%+v", len(batches), second)
	}
}

func TestModelStreamBatchBlockedCommitRetainsBoundsAndCancelledTail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	var batches [][]StreamDelta
	emit, drain := modelStreamBatchTicks(ctx, cancel, func(d []StreamDelta) error {
		batches = append(batches, append([]StreamDelta(nil), d...))
		if len(batches) == 1 {
			close(entered)
			<-release
		}
		return nil
	}, ticks, func() {})
	first := strings.Repeat("a", 64<<10)
	if err := emit(StreamDelta{Kind: "thinking", Text: first}); err != nil {
		t.Fatal(err)
	}
	<-entered
	var expected strings.Builder
	expected.WriteString(first)
	for i := 0; i < 64; i++ {
		fragment := fmt.Sprintf("%02d", i) + strings.Repeat("b", 2046)
		expected.WriteString(fragment)
		if err := emit(StreamDelta{Kind: "text", Text: fragment}); err != nil {
			t.Fatal(err)
		}
	}
	ticks <- time.Unix(1, 0)
	cancel()
	close(release)
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	var actual strings.Builder
	for _, batch := range batches {
		size := 0
		for _, delta := range batch {
			actual.WriteString(delta.Text)
			size += len(delta.Text) + len(delta.ID) + len(delta.Name) + len(delta.Arguments)
		}
		if size > 64<<10 || len(batch) > 256 {
			t.Fatalf("batch bounds exceeded: bytes=%d frames=%d", size, len(batch))
		}
	}
	if actual.String() != expected.String() || len(batches) != 3 {
		t.Fatalf("accepted tail lost or split: writes=%d bytes=%d want=%d", len(batches), actual.Len(), expected.Len())
	}
}

func TestModelStreamBatchBacklogPersistenceFailureCancelsAndReturnsCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	want := errors.New("backlog commit failure")
	writes := 0
	emit, drain := modelStreamBatchTicks(ctx, cancel, func(d []StreamDelta) error {
		writes++
		if writes == 1 {
			close(entered)
			<-release
			return nil
		}
		return want
	}, ticks, func() {})
	if err := emit(StreamDelta{Kind: "text", Text: strings.Repeat("a", 64<<10)}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := emit(StreamDelta{Kind: "text", Text: "accepted tail"}); err != nil {
		t.Fatal(err)
	}
	ticks <- time.Unix(1, 0)
	close(release)
	if err := drain(); !errors.Is(err, want) || ctx.Err() == nil {
		t.Fatalf("failure swallowed or provider not cancelled: %v %v", err, ctx.Err())
	}
}

type finalFlushProvider struct{ beforeEmit func() error }

func (p finalFlushProvider) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{}, errors.New("streaming fixture expected")
}

func (p finalFlushProvider) CompleteStream(_ context.Context, _ ModelRequest, emit func(StreamDelta) error) (ModelResponse, error) {
	if err := p.beforeEmit(); err != nil {
		return ModelResponse{}, err
	}
	err := emit(StreamDelta{Kind: "tool_call", Index: 0, ID: "c1", Name: "exec", Arguments: `{"command":"printf should-not-dispatch"}`})
	return ModelResponse{ToolCalls: []ToolCall{{ID: "c1", Name: "exec", Arguments: `{"command":"printf should-not-dispatch"}`}}, StopReason: "tool_calls"}, err
}

func TestCompleteFailsWhenFinalStreamFlushFails(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	a.provider = finalFlushProvider{beforeEmit: func() error {
		_, err := store.Update(id, func(s *Snapshot) error { s.Run.Deleted = true; return nil })
		return err
	}}
	req := ModelRequest{ID: "flush-failure"}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Requests = append(s.Requests, req); return nil }); err != nil {
		t.Fatal(err)
	}
	response, err := a.complete(context.Background(), id, req)
	if err == nil || len(response.ToolCalls) != 1 {
		t.Fatalf("final flush failed without blocking dispatch: response=%+v err=%v", response, err)
	}
}

func TestPreviewCaptureFixture(t *testing.T) {
	if os.Getenv("TRACE_PREVIEW_CAPTURE_FIXTURE") != "1" {
		return
	}
	done := make(chan struct{})
	go func() { fmt.Fprint(os.Stderr, strings.Repeat("e", 1<<20)); close(done) }()
	fmt.Fprint(os.Stdout, strings.Repeat("o", 1<<20))
	<-done
	os.Exit(0)
}

func TestPreviewPersistenceDoesNotBlockProcessCapture(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	want := errors.New("fixture preview disk failure")
	emit, drain := outputPreviewBatch(func([]outputPreview) error {
		close(entered)
		<-release
		return want
	})
	emit("stdout", "seed")
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		drain()
		t.Fatal("preview not flushed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type execution struct {
		result worker.Result
		err    error
	}
	finished := make(chan execution, 1)
	captureDir := t.TempDir()
	go func() {
		result, err := worker.Run(ctx, worker.Request{Command: []string{os.Args[0], "-test.run=^TestPreviewCaptureFixture$"}, Env: []string{"TRACE_PREVIEW_CAPTURE_FIXTURE=1"}, CaptureDir: captureDir, OnOutput: emit})
		finished <- execution{result, err}
	}()
	var output execution
	select {
	case output = <-finished:
	case <-time.After(3 * time.Second):
		close(release)
		cancel()
		<-finished
		drain()
		t.Fatal("blocked preview prevented process capture from completing")
	}
	result, err := output.result, output.err
	close(release)
	if flushErr := drain(); !errors.Is(flushErr, want) {
		t.Fatalf("lost preview persistence error: %v", flushErr)
	}
	if err != nil || result.Status != worker.StatusCompleted {
		t.Fatalf("preview stalled process: %+v %v", result, err)
	}
	for path, expected := range map[string]string{result.StdoutPath: strings.Repeat("o", 1<<20), result.StderrPath: strings.Repeat("e", 1<<20)} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != expected {
			t.Fatalf("capture bytes changed: %s bytes=%d err=%v", path, len(data), err)
		}
	}
}

func TestPreviewFinalDrainOffsetsAndBound(t *testing.T) {
	var saved []outputPreview
	emit, drain := outputPreviewBatch(func(batch []outputPreview) error { saved = append(saved, batch...); return nil })
	emit("stdout", strings.Repeat("x", 8192))
	emit("stderr", "last error")
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 3 || len(saved[0].content) != 4096 || saved[0].end != 4096 || saved[0].next != 8192 || saved[1].offset != 4096 || saved[1].end != 8192 || saved[2].content != "last error" {
		t.Fatalf("invalid bounded previews: %+v", saved)
	}
}

func TestPreviewCumulativeBudgetAndFinalTail(t *testing.T) {
	ticks := make(chan time.Time)
	committed := make(chan struct{}, 64)
	var saved []outputPreview
	emit, drain := outputPreviewBatchTicks(func(batch []outputPreview) error {
		saved = append(saved, batch...)
		committed <- struct{}{}
		return nil
	}, ticks, func() {})
	raw := map[string]string{}
	for i := 0; i < artifactThreshold/1024; i++ {
		for _, stream := range []string{"stdout", "stderr"} {
			chunk := strings.Repeat(stream[:1], 1024)
			raw[stream] += chunk
			emit(stream, chunk)
		}
		ticks <- time.Now()
		<-committed
	}
	// Slow output after the lifetime preview budget must not keep adding events.
	for i := 0; i < 40; i++ {
		for _, stream := range []string{"stdout", "stderr"} {
			chunk := strings.Repeat("middle-"+stream+"\n", 500)
			raw[stream] += chunk
			emit(stream, chunk)
		}
		ticks <- time.Now()
	}
	for _, stream := range []string{"stdout", "stderr"} {
		chunk := "final-" + stream + "-汉字😀"
		raw[stream] += chunk
		emit(stream, chunk)
	}
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2*(artifactThreshold/1024+1) {
		t.Fatalf("ticks kept persisting after the preview budget: %d", len(saved))
	}
	for _, stream := range []string{"stdout", "stderr"} {
		bodyBytes, end := 0, int64(0)
		for _, p := range saved {
			if p.stream != stream {
				continue
			}
			bodyBytes += len(p.content)
			if p.offset < end || p.end > p.next || p.content != raw[stream][p.offset:p.end] || !utf8.ValidString(p.content) {
				t.Fatalf("preview overlapped or changed source bytes: %+v", p)
			}
			end = p.end
		}
		if bodyBytes > artifactThreshold+outputPreviewBytes || end != int64(len(raw[stream])) {
			t.Fatalf("unbounded body or missing final tail: stream=%s bytes=%d end=%d", stream, bodyBytes, end)
		}
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result := ToolResult{Stdout: raw["stdout"], Stderr: raw["stderr"]}
	if err := (&Agent{store: store}).archiveOutput(&result); err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "" || result.Stderr != "" || len(result.Artifacts) != 2 {
		t.Fatal("full output remained inline instead of using the existing CAS")
	}
	for _, ref := range result.Artifacts {
		actual, err := artifactOutput(store.root, result, ref.Kind)
		if err != nil || actual != raw[ref.Kind] || ref.Hash != promptHash(raw[ref.Kind]) {
			t.Fatalf("preview budget changed exact output or provenance: %s %v", ref.Kind, err)
		}
	}
}

func TestPreviewUTF8AcrossTicksAndFinalOverlap(t *testing.T) {
	ticks := make(chan time.Time)
	committed := make(chan struct{}, 8)
	var saved []outputPreview
	emit, drain := outputPreviewBatchTicks(func(batch []outputPreview) error {
		saved = append(saved, batch...)
		committed <- struct{}{}
		return nil
	}, ticks, func() {})
	// The second tick proves that the incomplete first byte was already flushed.
	emit("stdout", "汉"[:1])
	ticks <- time.Now()
	ticks <- time.Now()
	emit("stdout", "汉"[1:]+"字")
	ticks <- time.Now()
	<-committed
	emit("stdout", "😀tail")
	if err := drain(); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].content != "汉字" || saved[0].offset != 0 || saved[0].end != 6 || saved[1].content != "😀tail" || saved[1].offset != 6 || saved[1].end != 14 {
		t.Fatalf("UTF-8 split or final tail duplicated: %+v", saved)
	}
}

func TestPreviewFinalIncompleteUTF8AndBinaryWatermark(t *testing.T) {
	for name, raw := range map[string]string{
		"incomplete":         "\xe6",
		"continuation":       "\x80\x80\x80",
		"large-continuation": strings.Repeat("\x80", 2*outputPreviewBytes),
		"valid-prefix":       "valid-汉字\xe6",
	} {
		t.Run(name, func(t *testing.T) {
			var saved []outputPreview
			emit, drain := outputPreviewBatchTicks(func(batch []outputPreview) error { saved = append(saved, batch...); return nil }, nil, func() {})
			emit("stdout", raw)
			if err := drain(); err != nil {
				t.Fatal(err)
			}
			if len(saved) == 0 || saved[len(saved)-1].end != int64(len(raw)) || saved[len(saved)-1].next != int64(len(raw)) {
				t.Fatalf("final binary/UTF-8 bytes disappeared without a watermark: %+v", saved)
			}
			for _, p := range saved {
				if !utf8.ValidString(p.content) || p.content != strings.ToValidUTF8(raw[p.offset:p.end], "?") {
					t.Fatalf("preview has invalid text or source offsets: %+v", p)
				}
			}
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			result := ToolResult{Stdout: raw}
			if err := (&Agent{store: store}).archiveOutput(&result); err != nil {
				t.Fatal(err)
			}
			actual, err := artifactOutput(store.root, result, "stdout")
			if err != nil || actual != raw || len(result.Artifacts) != 1 || result.Artifacts[0].Hash != promptHash(raw) {
				t.Fatalf("binary source bytes or hash changed: %v", err)
			}
		})
	}
}
