//go:build !linux

package agent

import "os"

func openSkillFile(path string) (*os.File, error) { return os.Open(path) }
