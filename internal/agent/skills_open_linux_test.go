package agent

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestSkillFIFOIsRejectedWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openSkillFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := readSkillBody(path); err == nil {
		t.Fatal("FIFO accepted as skill body")
	}
	if _, err := DiscoverSkills([]string{filepath.Dir(path)}); err == nil {
		t.Fatal("FIFO accepted in catalog")
	}
}
