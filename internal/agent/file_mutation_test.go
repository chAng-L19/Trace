package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileMutationSymlinkAliasSerialAndIndependentParallel(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink fixture: %v", err)
	}
	path := filepath.Join(realDir, "data.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	canonical, release, err := lockFileMutation(path)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unlock := func() { once.Do(release) }
	defer unlock()
	if canonical != path {
		t.Fatalf("canonical path=%s", canonical)
	}
	started := make(chan struct{})
	same := make(chan error, 1)
	go func() {
		close(started)
		resolved, done, err := lockFileMutation(filepath.Join(alias, "data.txt"))
		if err == nil {
			if resolved != canonical {
				err = fmt.Errorf("alias key=%s", resolved)
			}
			done()
		}
		same <- err
	}()
	<-started
	select {
	case err := <-same:
		t.Fatalf("alias was not serialized: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	different := make(chan error, 1)
	go func() {
		_, done, err := lockFileMutation(filepath.Join(realDir, "other.txt"))
		if err == nil {
			done()
		}
		different <- err
	}()
	select {
	case err := <-different:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("independent file blocked behind shared file")
	}
	unlock()
	select {
	case err := <-same:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("alias lock did not release")
	}
	fileMutations.Lock()
	remaining := len(fileMutations.paths)
	fileMutations.Unlock()
	if remaining != 0 {
		t.Fatalf("lock registry leaked %d paths", remaining)
	}
}

func TestFileMutationMissingTargetViaDirectoryAliasUsesOneKey(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink fixture: %v", err)
	}
	a, err := mutationPath(filepath.Join(realDir, "not-created", "target"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := mutationPath(filepath.Join(alias, "not-created", "target"))
	if err != nil || a != b {
		t.Fatalf("missing alias keys differ: %s %s %v", a, b, err)
	}
}

func TestFileMutationDanglingSymlinkWritesTargetNotLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink("target", alias); err != nil {
		t.Skip(err)
	}
	if _, err := fileOperation("write", FileRequest{Path: alias, Content: "new"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(alias)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "new" {
		t.Fatalf("target not written: %q %v", raw, err)
	}
}

func TestFileMutationConcurrentEditsPreserveIndependentChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.txt")
	if err := os.WriteFile(path, []byte("alpha=0\nbeta=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, key := range []string{"alpha", "beta"} {
		go func(key string) {
			<-start
			_, err := fileOperation("edit", FileRequest{Path: path, OldText: key + "=0", NewText: key + "=1"})
			results <- err
		}(key)
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "alpha=1\nbeta=1\n" {
		t.Fatalf("lost update: %q %v", raw, err)
	}
}

func TestFileMutationExpectedHashCheckedAfterAcquiringLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	_, unlock, err := lockFileMutation(path)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := fileOperation("write", FileRequest{Path: path, Content: "lost update", ExpectedHash: promptHash("old")})
		result <- err
	}()
	if err := os.WriteFile(path, []byte("new"), 0600); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	if err := <-result; err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("stale write: %v", err)
	}
	value, err := os.ReadFile(path)
	if err != nil || string(value) != "new" {
		t.Fatalf("failed write mutated file: %q %v", value, err)
	}
}

func BenchmarkFileMutationIndependentParallel(b *testing.B) {
	dir := b.TempDir()
	var mu sync.Mutex
	next := 0
	b.RunParallel(func(pb *testing.PB) {
		mu.Lock()
		index := next
		next++
		mu.Unlock()
		path := filepath.Join(dir, fmt.Sprint(index))
		for pb.Next() {
			_, done, err := lockFileMutation(path)
			if err != nil {
				b.Error(err)
				return
			}
			done()
		}
	})
}

func BenchmarkFileMutationAtomicWritePaths(b *testing.B) {
	for _, global := range []bool{true, false} {
		name := "path"
		if global {
			name = "global_baseline"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			var registration, legacy sync.Mutex
			next := 0
			b.SetBytes(4096)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				registration.Lock()
				index := next
				next++
				registration.Unlock()
				path := filepath.Join(dir, fmt.Sprint(index))
				content := strings.Repeat("x", 4096)
				for pb.Next() {
					if global {
						legacy.Lock()
					}
					_, err := fileOperation("write", FileRequest{Path: path, Content: content})
					if global {
						legacy.Unlock()
					}
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
