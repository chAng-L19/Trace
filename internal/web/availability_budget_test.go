package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestSkillAvailabilityPersistsAndRollsBackFailures(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	s, live := fixture(t, nil)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: fixture\ndescription: metadata\n---\nmanagement body"), 0600); err != nil {
		t.Fatal(err)
	}
	s.config.SkillPaths = []string{root}
	if err := live.ConfigureSkills(s.config.SkillPaths); err != nil {
		t.Fatal(err)
	}
	failBuild := false
	s.options.BuildAgent = func(_ context.Context, c Config) (*agent.Agent, error) {
		if failBuild {
			return nil, errors.New("candidate rejected")
		}
		a, err := agent.New(live.Store(), nil, "")
		if err == nil {
			err = a.ConfigureSkills(c.SkillPaths, c.DisabledSkills...)
		}
		return a, err
	}
	h := s.Handler()
	request(t, h, "PUT", "/api/skills/fixture/availability", map[string]bool{"disabled": true}, 200)
	if s.a == live || len(s.a.ModelSkills()) != 0 || len(s.config.DisabledSkills) != 1 {
		t.Fatal("disabled state not applied")
	}
	savedPath := s.options.ConfigPath
	before, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(savedPath, Config{}, t.TempDir())
	if err != nil || len(loaded.Config.DisabledSkills) != 1 || loaded.Config.DisabledSkills[0] != "fixture" {
		t.Fatalf("availability did not persist: %+v %v", loaded.Config.DisabledSkills, err)
	}
	if body := request(t, h, "GET", "/api/skills/fixture", nil, 200).Body.String(); !strings.Contains(body, "management body") {
		t.Fatalf("disabled management read blocked: %s", body)
	}
	retained := s.a
	failBuild = true
	request(t, h, "PUT", "/api/skills/fixture/availability", map[string]bool{"disabled": false}, 409)
	failBuild = false
	s.options.ConfigPath = t.TempDir() // atomic file replace must fail on a directory.
	request(t, h, "PUT", "/api/skills/fixture/availability", map[string]bool{"disabled": false}, 409)
	s.options.ConfigPath = savedPath
	after, err := os.ReadFile(savedPath)
	if err != nil || string(before) != string(after) || s.a != retained || len(s.a.ModelSkills()) != 0 {
		t.Fatal("failed build/save replaced persisted config or live Agent")
	}
	s.active["busy"] = true
	request(t, h, "PUT", "/api/skills/fixture/availability", map[string]bool{"disabled": false}, 409)
	delete(s.active, "busy")
	request(t, h, "PUT", "/api/skills/fixture/availability", map[string]bool{}, 400)
	request(t, h, "PUT", "/api/skills/missing/availability", map[string]bool{"disabled": true}, 404)
	request(t, h, "PUT", "/api/skills/fixture/availability", map[string]bool{"disabled": false}, 200)
	if len(s.a.ModelSkills()) == 0 || len(s.config.DisabledSkills) != 0 {
		t.Fatal("reenable did not restore model availability")
	}
}

func TestRunBudgetHTTPIsExplicitAndRunScoped(t *testing.T) {
	s, a := fixture(t, nil)
	first, err := a.Create("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Create("second")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + first.Run.ID + "/budget"
	h := s.Handler()
	request(t, h, "PUT", path, map[string]int{"max_actions": 4}, 400)
	request(t, h, "PUT", path, agent.Budget{MaxSeconds: -1}, 400)
	s.active[first.Run.ID] = true
	request(t, h, "PUT", path, agent.Budget{MaxActions: 4}, 409)
	delete(s.active, first.Run.ID)
	request(t, h, "PUT", path, agent.Budget{MaxActions: 4}, 200)
	saved, err := a.Store().Load(first.Run.ID)
	if err != nil || saved.Run.Budget.MaxActions != 4 || !saved.Run.BudgetConfigured {
		t.Fatalf("explicit budget was not saved: %+v %v", saved.Run, err)
	}
	other, err := a.Store().Load(second.Run.ID)
	if err != nil || other.Run.Budget != (agent.Budget{}) || other.Run.BudgetConfigured {
		t.Fatal("another run's budget changed")
	}
	request(t, h, "PUT", path, agent.Budget{}, 200)
}
