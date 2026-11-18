package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Adapted from pi coding-agent/core/tools/file-mutation-queue.ts: register
// canonical paths briefly, then serialize only mutations of the same file.
var fileMutations = struct {
	sync.Mutex
	paths map[string]*fileMutationLock
}{paths: map[string]*fileMutationLock{}}

type fileMutationLock struct {
	mu    sync.Mutex
	users int
}

func mutationPath(path string) (string, error) {
	return mutationPathDepth(path, 0)
}

func mutationPathDepth(path string, depth int) (string, error) {
	if depth > 40 {
		return "", fmt.Errorf("too many symbolic links: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if info, lstatErr := os.Lstat(path); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return mutationPathDepth(filepath.Clean(target), depth+1)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = mutationPathDepth(parent, depth+1)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

func lockFileMutation(path string) (string, func(), error) {
	fileMutations.Lock()
	resolved, err := mutationPath(path)
	if err != nil {
		fileMutations.Unlock()
		return "", nil, err
	}
	entry := fileMutations.paths[resolved]
	if entry == nil {
		entry = &fileMutationLock{}
		fileMutations.paths[resolved] = entry
	}
	entry.users++
	fileMutations.Unlock()
	entry.mu.Lock()
	return resolved, func() {
		entry.mu.Unlock()
		fileMutations.Lock()
		entry.users--
		if entry.users == 0 {
			delete(fileMutations.paths, resolved)
		}
		fileMutations.Unlock()
	}, nil
}
