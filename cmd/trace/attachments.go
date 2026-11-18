package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"trace/internal/agent"
)

func inputArguments(store *agent.Store, args []string) (string, []agent.InputAttachment, error) {
	var text []string
	var inputs []agent.InputAttachment
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			text = append(text, args[i+1:]...)
			break
		}
		if args[i] != "--attach" {
			text = append(text, args[i])
			continue
		}
		i++
		if i == len(args) {
			return "", nil, fmt.Errorf("--attach requires a file path")
		}
		f, err := os.Open(args[i])
		if err != nil {
			return "", nil, err
		}
		input, err := store.PutInputAttachment(filepath.Base(args[i]), f)
		f.Close()
		if err != nil {
			return "", nil, err
		}
		inputs = append(inputs, input)
	}
	return strings.TrimSpace(strings.Join(text, " ")), inputs, nil
}
