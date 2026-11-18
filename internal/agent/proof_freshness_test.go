package agent

import (
	"context"
	"testing"
)

func TestFindingRetestBindsExecutionAdapterAndCommand(t *testing.T) {
	for _, tc := range []struct {
		name, adapter string
		command       []string
		want          bool
	}{
		{"same", "exec", []string{"sh", "-c", "printf 'observed-marker'"}, true},
		{"different-command", "exec", []string{"sh", "-c", "echo observed-marker"}, false},
		{"different-adapter", "local", []string{"sh", "-c", "printf 'observed-marker'"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, id, s := reviewProofFixture(t)
			if _, err := a.SubmitFinding(id, reviewProofFinding(s)); err != nil {
				t.Fatal(err)
			}
			a.provider = manualProvider(ToolCall{ID: "retest", Name: "external", Arguments: `{}`})
			waitTestRun(t, a, id)
			// The adapter fixture intentionally returns identical output: routing
			// identity, not the marker alone, must decide whether this is a retest.
			s, err := a.executeAction(context.Background(), id, "retest", tc.adapter, tc.command, func() ToolResult { return ToolResult{Stdout: "observed-marker"} })
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.RetestFinding(id, "review-fixture", []string{s.Evidence[len(s.Evidence)-1].ID})
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%t want=%t err=%v", err == nil, tc.want, err)
			}
		})
	}
}

func TestFindingProbeIdentityBindsRemoteRoute(t *testing.T) {
	original := Action{Name: "tool", Arguments: `{}`, Adapter: "mcp", ServerName: "one", RemoteName: "probe", RemoteMethod: "tools/call"}
	for _, change := range []func(*Action){
		func(a *Action) { a.ServerName = "two" }, func(a *Action) { a.RemoteName = "echo" }, func(a *Action) { a.RemoteMethod = "resources/read" }, func(a *Action) { a.LocalName = "different" },
	} {
		changed := original
		change(&changed)
		if findingProbeHash(original) == findingProbeHash(changed) {
			t.Fatal("probe route change did not change identity")
		}
	}
}

func cleanupProofFixture(t *testing.T) (*Agent, *Store, string) {
	t.Helper()
	a, store := newTestAgent(t, manualProvider(
		ToolCall{ID: "mutate", Name: "external", Arguments: `{"operation":"dirty"}`},
		ToolCall{ID: "probe", Name: "external", Arguments: `{"probe":"clean"}`},
		ToolCall{ID: "fresh", Name: "external", Arguments: `{"probe":"clean"}`},
	))
	id := createTestRun(t, a)
	r := VerificationRequirement{ID: "cleanup", Description: "target clean after executions settle", Category: "cleanup", ToolID: "external", ArgumentsHash: promptHash(`{"probe":"clean"}`), Predicate: EvidencePredicate{Kind: "equals", Value: "clean"}}
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
		t.Fatal(err)
	}
	waitTestRun(t, a, id)
	return a, store, id
}

func TestCleanupProbeMustStartAfterMutationSettles(t *testing.T) {
	a, store, id := cleanupProofFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := a.executeAction(context.Background(), id, "mutate", "exec", []string{"fixture-mutation"}, func() ToolResult { close(entered); <-release; return ToolResult{Stdout: "dirty"} })
		done <- err
	}()
	<-entered
	s, probeErr := a.executeCommand(context.Background(), id, "probe", []string{"sh", "-c", "printf clean"})
	if probeErr != nil {
		close(release)
		<-done
		t.Fatal(probeErr)
	}
	probeID := s.Evidence[len(s.Evidence)-1].ID
	s, checkErr := a.CheckEvidence(context.Background(), id, "cleanup", []string{probeID})
	close(release)
	mutationErr := <-done
	if checkErr != nil || mutationErr != nil {
		t.Fatalf("check=%v mutation=%v", checkErr, mutationErr)
	}
	if latestCheck(&s, "cleanup").Status == "pass" {
		t.Fatal("probe passed while another mutation was active")
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if latestCheck(&s, "cleanup").Status == "pass" {
		t.Fatal("overlapping probe became valid after mutation settled")
	}
	s, err = a.executeCommand(context.Background(), id, "fresh", []string{"sh", "-c", "printf clean"})
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.CheckEvidence(context.Background(), id, "cleanup", []string{s.Evidence[len(s.Evidence)-1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if latestCheck(&s, "cleanup").Status != "pass" {
		t.Fatal("new probe after settlement did not pass")
	}
}

func TestCleanupRequiresProbeAfterWorkerSettlement(t *testing.T) {
	a, store, id := cleanupProofFixture(t)
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Workers = append(s.Workers, AgentWorker{ID: "child", Status: "running", CleanupStatus: "pending"})
		addEvent(s, "worker_started", "fixture worker started", nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.executeCommand(context.Background(), id, "probe", []string{"sh", "-c", "printf clean"})
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.CheckEvidence(context.Background(), id, "cleanup", []string{s.Evidence[len(s.Evidence)-1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if latestCheck(&s, "cleanup").Status == "pass" {
		t.Fatal("cleanup passed with active child")
	}
	s, err = store.Update(id, func(s *Snapshot) error {
		s.Workers[0].Status = "completed"
		s.Workers[0].CleanupStatus = "completed"
		addEvent(s, "worker_finished", "fixture worker settled", nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if latestCheck(&s, "cleanup").Status == "pass" {
		t.Fatal("worker settlement revived an earlier probe")
	}
	s, err = a.executeCommand(context.Background(), id, "fresh", []string{"sh", "-c", "printf clean"})
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.CheckEvidence(context.Background(), id, "cleanup", []string{s.Evidence[len(s.Evidence)-1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if latestCheck(&s, "cleanup").Status != "pass" {
		t.Fatal("post-worker probe did not pass")
	}
}
