package agent

import (
	"context"
	"time"
)

// Reference: DeepSeek Harness 5719020 session-persistence-jsonl/storage.ts:
// bounded live-write window and final drain. Trace keeps its Store transaction
// and event format. At most 64 queued frames plus one 64 KiB batch; a single
// provider frame may exceed that byte threshold. Backpressure never drops data.
func modelStreamBatch(ctx context.Context, cancel context.CancelFunc, persist func([]StreamDelta) error) (func(StreamDelta) error, func() error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	return modelStreamBatchTicks(ctx, cancel, persist, ticker.C, ticker.Stop)
}

// The tick source is injectable so blocked-persistence regression tests do not
// depend on wall-clock sleeps or the scheduler's choice of a pending timer.
func modelStreamBatchTicks(ctx context.Context, cancel context.CancelFunc, persist func([]StreamDelta) error, ticks <-chan time.Time, stopTicks func()) (func(StreamDelta) error, func() error) {
	queue := make(chan StreamDelta, 64)
	done := make(chan struct{})
	var persistErr error
	go func() {
		defer close(done)
		defer stopTicks()
		var pending []StreamDelta
		bytes := 0
		closed := false
		appendDelta := func(delta StreamDelta) {
			bytes += len(delta.Text) + len(delta.ID) + len(delta.Name) + len(delta.Arguments)
			if len(pending) > 0 && pending[len(pending)-1].Kind == delta.Kind && pending[len(pending)-1].Index == delta.Index {
				last := &pending[len(pending)-1]
				last.Text += delta.Text
				last.ID += delta.ID
				last.Name += delta.Name
				last.Arguments += delta.Arguments
			} else {
				pending = append(pending, delta)
			}
		}
		flush := func() bool {
			// A slow commit can leave both a tick and accepted frames queued.
			// Drain at most one bounded queue before another full-snapshot write;
			// never wait for new frames or defer the current tick by another 200ms.
		drain:
			for n := 0; n < cap(queue) && bytes < 64<<10 && len(pending) < 256; n++ {
				select {
				case delta, ok := <-queue:
					if !ok {
						closed = true
						break drain
					}
					appendDelta(delta)
				default:
					break drain
				}
			}
			if len(pending) == 0 {
				return true
			}
			persistErr = persist(pending)
			if persistErr != nil {
				cancel()
				return false
			}
			pending, bytes = nil, 0
			return true
		}
		for {
			select {
			case delta, ok := <-queue:
				if !ok {
					flush()
					return
				}
				appendDelta(delta)
				if (bytes >= 64<<10 || len(pending) >= 256) && !flush() {
					return
				}
			case <-ticks:
				if !flush() || closed {
					return
				}
			}
		}
	}()
	emit := func(delta StreamDelta) error {
		select {
		case <-done:
			return persistErr
		case <-ctx.Done():
			return ctx.Err()
		case queue <- delta:
			return nil
		}
	}
	return emit, func() error {
		close(queue)
		<-done
		return persistErr
	}
}
