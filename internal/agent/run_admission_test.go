package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunAdmissionDoesNotMutateOwnerAndPreservesErrorCause(t *testing.T) {
	for _, operation := range []string{"run", "message"} {
		t.Run(operation, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			invoke := func(id string) (Snapshot, error) {
				if operation == "message" {
					return a.Message(context.Background(), id, "new input")
				}
				return a.Run(context.Background(), id)
			}
			path := filepath.Join(store.root, id+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			release, err := store.AcquireRunLock(id)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			snap, err := invoke(id)
			if !errors.Is(err, ErrRunAdmission) || snap.Run.ID != "" {
				t.Fatalf("lock rejection did not identify admission failure: %+v %v", snap.Run, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected request changed owner's snapshot: %v", err)
			}
			_, err = invoke("../invalid")
			if !errors.Is(err, ErrRunAdmission) || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("admission wrapper lost original error: %v", err)
			}
		})
	}
}

func TestRunAfterAdmissionDoesNotLabelExecutionErrorAsAdmission(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := a.Run(context.Background(), id)
	if err == nil || errors.Is(err, ErrRunAdmission) {
		t.Fatalf("post-admission error classified incorrectly: %v", err)
	}
}
