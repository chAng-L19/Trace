package agent

import (
	"testing"
)

func TestWorkerContextSourcesRequestLifetime(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root := createTestRun(t, a)
			child, err := a.Create("source")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(child.Run.ID, func(s *Snapshot) error {
				s.Run.ParentRunID, s.Run.ParentTaskID = root, "source"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			parent, err := store.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			parent.Workers = []AgentWorker{{ID: "worker", TaskID: "source", RunID: child.Run.ID}}
			load := a.workerContextSources(&parent)
			source, err := load(child.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			loadSamples := func() int64 {
				for _, stage := range store.StoreDiagnostics().Stages {
					if stage.Stage == "load_verify" {
						return stage.Samples
					}
				}
				return 0
			}
			samples := loadSamples()
			for i := 0; i < 20; i++ {
				again, err := load(child.Run.ID)
				if err != nil || again != source {
					t.Fatalf("projection source reloaded: %p %p %v", source, again, err)
				}
			}
			if got := loadSamples(); got != samples {
				t.Fatalf("repeated context references reloaded Store: %d -> %d", samples, got)
			}
			for _, id := range []string{"", root} {
				got, err := load(id)
				if err != nil || got != &parent {
					t.Fatalf("parent alias changed: %p %v", got, err)
				}
			}
			if _, err := store.Update(child.Run.ID, func(s *Snapshot) error {
				s.Run.ParentTaskID = "foreign-task"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if same, err := load(child.Run.ID); err != nil || same != source {
				t.Fatal("one projection mixed two source snapshots")
			}
			fresh := a.workerContextSources(&parent)
			if _, err := fresh(child.Run.ID); err == nil {
				t.Fatal("next request reused stale ancestry")
			}
			samples = loadSamples()
			if _, err := fresh(child.Run.ID); err == nil {
				t.Fatal("cached failure became success")
			}
			if got := loadSamples(); got != samples {
				t.Fatal("failed source reloaded within one projection")
			}
			if _, err := store.Update(child.Run.ID, func(s *Snapshot) error {
				s.Run.ParentTaskID = "source"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fresh(child.Run.ID); err == nil {
				t.Fatal("failed projection silently changed after repair")
			}
			if _, err := a.workerContextSources(&parent)(child.Run.ID); err != nil {
				t.Fatalf("next request did not observe repaired source: %v", err)
			}
		})
	}
}
