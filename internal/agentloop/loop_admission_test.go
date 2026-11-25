package agentloop

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCancelledModelResponseDoesNotEnterFinish(t *testing.T) {
	ctx, cancel := context.WithCancel(testContext(t))
	f := &fake{model: func() (Turn, error) { cancel(); return Turn{}, nil }}
	if err := Run(ctx, f, errors.New("halt")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.log, ","), "finish") {
		t.Fatal("cancelled response entered Finish")
	}
}
