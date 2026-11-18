package agent

import (
	"os"
	"syscall"
)

// Nonblocking open lets the subsequent regular-file check reject a FIFO even
// when a local file replacement races discovery or the initial Stat.
func openSkillFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
