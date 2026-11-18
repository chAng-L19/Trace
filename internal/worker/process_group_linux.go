//go:build linux

package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Zombies have exited and cannot retain output pipes or mutate resources.
// A container's init may reap them late, so kill(0) alone is not exit proof.
func processGroupAlive(group int) (bool, error) {
	if err := syscall.Kill(-group, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		end := strings.LastIndexByte(string(raw), ')')
		if end < 0 {
			return false, fmt.Errorf("invalid proc stat")
		}
		fields := strings.Fields(string(raw)[end+1:])
		if len(fields) < 3 {
			return false, fmt.Errorf("short proc stat")
		}
		pgrp, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if pgrp == group && fields[0] != "Z" && fields[0] != "X" {
			return true, nil
		}
	}
	return false, nil
}
