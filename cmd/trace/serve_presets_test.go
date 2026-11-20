package main

import (
	"testing"

	"trace/internal/localtool"
	"trace/internal/web"
)

func TestSavedWebConfigAddsNewCLIPresets(t *testing.T) {
	c := web.Config{LocalTools: []localtool.Manifest{{Name: "operator", Command: "custom", Disabled: true}}}
	presets := []localtool.Manifest{{Name: "operator", Command: "default"}, {Name: "cloakbrowser", Command: "python"}}
	mergeWebPresets(&c, presets)
	mergeWebPresets(&c, presets)
	if len(c.LocalTools) != 2 || !c.LocalTools[0].Disabled || c.LocalTools[0].Command != "custom" || c.LocalTools[1].Name != "cloakbrowser" {
		t.Fatalf("saved config lost precedence or newly installed CLI: %+v", c.LocalTools)
	}
}
