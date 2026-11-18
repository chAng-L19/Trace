package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillAvailabilityDoesNotReviveShadowedSource(t *testing.T) {
	root := t.TempDir()
	project, home := filepath.Join(root, "project"), filepath.Join(root, "home")
	paths := []string{filepath.Join(root, "explicit"), filepath.Join(project, ".trace", "skills", "same"), filepath.Join(home, ".trace", "skills", "same")}
	for i, path := range paths {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: same\ndescription: source\n---\n" + strings.Repeat("winner", i+1)
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	a, _ := newTestAgent(t, successProvider{})
	if err := a.ConfigureSkills([]string{paths[0]}, "same"); err != nil {
		t.Fatal(err)
	}
	if skills := a.Skills(); len(skills) != 1 || !skills[0].Disabled || skills[0].Source != paths[0] {
		t.Fatalf("management lost disabled winning source: %+v", skills)
	}
	if body, err := a.ReadSkill("same"); err != nil || !strings.HasSuffix(body, "winner") {
		t.Fatalf("management read: %q %v", body, err)
	}
	child := *a // runChild copies the configured catalog before cloning tools.
	for _, runner := range []*Agent{a, &child} {
		if runner.skillCatalog() != "" {
			t.Fatal("disabled skill leaked into model prompt")
		}
		rows, err := runner.builtin(context.Background(), "", ToolCall{Name: "skills_list", Arguments: "{}"})
		if err != nil || len(rows.([]Skill)) != 0 {
			t.Fatalf("disabled model directory: %+v %v", rows, err)
		}
		if _, err := runner.builtin(context.Background(), "", ToolCall{Name: "read_skill", Arguments: `{"name":"same"}`}); err == nil {
			t.Fatal("model read accepted disabled skill")
		}
	}
	if err := a.ConfigureSkills([]string{paths[0]}); err != nil {
		t.Fatal(err)
	}
	if len(a.ModelSkills()) != 1 || !strings.Contains(a.skillCatalog(), paths[0]) {
		t.Fatal("reenabling did not restore winning source")
	}
}
