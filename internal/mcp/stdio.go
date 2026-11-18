package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"trace/internal/worker"
)

type incoming struct {
	data []byte
	err  error
}

type stdioTransport struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	cancel   context.CancelFunc
	frames   chan incoming
	done     chan struct{}
	stopRead chan struct{}
	readDone chan struct{}
	waitErr  error
	stopOnce sync.Once
	writeMu  sync.Mutex
	readOnce sync.Once
}

func newStdio(config Config) (*stdioTransport, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, config.Command, config.Args...)
	worker.ConfigureCommand(cmd)
	cmd.Dir = config.Dir
	cmd.Env = os.Environ()
	for name, value := range config.Env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	// MCP diagnostics never share the JSON-RPC channel and do not grow memory.
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, writer, err := os.Pipe()
	if err != nil {
		stdin.Close()
		cancel()
		return nil, err
	}
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		writer.Close()
		cancel()
		return nil, err
	}
	writer.Close()
	t := &stdioTransport{cmd: cmd, stdin: stdin, stdout: stdout, cancel: cancel, frames: make(chan incoming, 1), done: make(chan struct{}), stopRead: make(chan struct{}), readDone: make(chan struct{})}
	go t.read()
	go func() { t.waitErr = cmd.Wait(); close(t.done) }()
	return t, nil
}

func (t *stdioTransport) setProtocol(string) {}

func (t *stdioTransport) read() {
	defer close(t.readDone)
	scanner := bufio.NewScanner(t.stdout)
	scanner.Buffer(make([]byte, 4096), MaxMessageBytes+1)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		frame := incoming{data: append([]byte(nil), line...)}
		select {
		case t.frames <- frame:
		case <-t.stopRead:
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case t.frames <- incoming{err: err}:
	case <-t.stopRead:
	}
}

func (t *stdioTransport) stop() {
	t.stopOnce.Do(func() {
		t.readOnce.Do(func() { close(t.stopRead) })
		// Kill the process tree before closing stdin, so a parent cannot exit first.
		t.cancel()
	})
}

func (t *stdioTransport) write(rpc request) error {
	data, err := json.Marshal(rpc)
	if err != nil {
		return err
	}
	if len(data) > MaxMessageBytes {
		return errors.New("MCP request exceeds frame limit")
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err = t.stdin.Write(append(data, '\n'))
	return err
}

func (t *stdioTransport) roundTrip(ctx context.Context, rpc request) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-t.stopRead:
		return nil, errors.New("MCP stdio session is closed")
	default:
	}
	// A cancelled stdio call closes this session instead of guessing whether a
	// remote side effect completed. The owner may reconnect, but must not replay.
	// Termination must not wait for writeMu: a peer that stopped reading stdin
	// may already have blocked the active request inside Write.
	stop := context.AfterFunc(ctx, t.stop)
	defer stop()
	if err := t.write(rpc); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("MCP stdio write: %w", err)
	}
	if rpc.ID == 0 {
		return nil, nil
	}
	for {
		select {
		case <-t.stopRead:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("MCP stdio session is closed")
		case <-ctx.Done():
			t.stop()
			return nil, ctx.Err()
		case frame := <-t.frames:
			if frame.err != nil {
				t.stop()
				return nil, fmt.Errorf("MCP stdio read: %w", frame.err)
			}
			observeProgress(ctx, frame.data, rpc.ID)
			result, err := decodeResponse(frame.data, rpc.ID)
			if err != nil {
				var rpcError *RPCError
				if !errors.As(err, &rpcError) {
					t.stop()
				}
				return result, err
			}
			if result != nil {
				return result, nil
			}
		}
	}
}

func (t *stdioTransport) close(ctx context.Context) error {
	// EOF lets the server release provider sessions and retained resources.
	// Stop the reader first so unsolicited shutdown frames cannot block it.
	t.readOnce.Do(func() { close(t.stopRead) })
	t.writeMu.Lock()
	_ = t.stdin.Close()
	t.writeMu.Unlock()
	select {
	case <-t.done:
		t.cancel()
		t.stdout.Close()
		<-t.readDone
		if t.waitErr == nil {
			return nil
		}
		return fmt.Errorf("MCP process did not exit cleanly: %w", t.waitErr)
	case <-ctx.Done():
		t.stop()
		t.stdout.Close()
		<-t.readDone
		return fmt.Errorf("MCP process cleanup: %w", ctx.Err())
	}
}
