package localtool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCapabilityInventoryOffline(t *testing.T) {
	tools := installedPresets(t)
	m, ok := tools["capability-inventory"]
	if !ok || m.TimeoutSeconds != 10 {
		t.Fatalf("missing bounded inventory: %+v", m)
	}
	// -S excludes host site-packages; fixture packages must not be imported.
	m.Args = append([]string{"-S"}, m.Args...)
	m.Args[2] = "import sys\ndef reject_effect(event, args):\n if event.startswith('socket.') or event == 'subprocess.Popen': raise RuntimeError('inventory side effect: '+event)\nsys.addaudithook(reject_effect)\n" + helperSource
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "present"}[present], func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "import-side-effect")
			browser := filepath.Join(root, "browser")
			if present {
				for _, name := range []string{"ssh", "sshpass", "xvfb-run", "xauth"} {
					if runtime.GOOS == "windows" {
						name += ".exe"
					}
					if err := os.WriteFile(filepath.Join(bin, name), []byte("do not execute"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(browser, []byte("do not launch"), 0700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"pydash", "cloakbrowser"} {
					quoted, _ := json.Marshal(marker)
					source := "from pathlib import Path\nPath(" + string(quoted) + ").write_text('unexpected import')\nraise RuntimeError('must not import')\n"
					if err := os.WriteFile(filepath.Join(root, name+".py"), []byte(source), 0600); err != nil {
						t.Fatal(err)
					}
					dist := filepath.Join(root, name+"-9.9.dist-info")
					if err := os.Mkdir(dist, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dist, "METADATA"), []byte("Metadata-Version: 2.1\nName: "+name+"\nVersion: 9.9\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			call := m
			call.Env = map[string]string{"PATH": bin, "PYTHONPATH": root, "DISPLAY": "",
				"CLOAKBROWSER_BINARY_PATH": browser, "BENCHMARK_TOKEN": "capability-private-env"}
			out := invokePreset(t, call, map[string]any{}, false)
			paths := out["tools"].(map[string]any)
			if len(paths) != 15 || paths["ssh"].(map[string]any)["available"] != present {
				t.Fatalf("PATH presence: %+v", paths)
			}
			mods := out["modules"].(map[string]any)
			pydash := mods["pydash"].(map[string]any)
			if pydash["spec_found"] != present || pydash["import_verified"] != false || mods["paramiko"].(map[string]any)["spec_found"] != false {
				t.Fatalf("module presence: %+v", mods)
			}
			if present && pydash["version"] != "9.9" {
				t.Fatalf("distribution metadata: %+v", pydash)
			}
			b := out["browser"].(map[string]any)
			display := b["display"].(map[string]any)
			if b["binary"].(map[string]any)["is_file"] != present || b["launch_verified"] != false || display["configured"] != false || display["connection_verified"] != false || display["xvfb_run_available"] != present || display["xauth_available"] != present {
				t.Fatalf("browser prerequisites: %+v", b)
			}
			ssh := out["alternatives"].(map[string]any)["ssh_password_candidates"].([]any)
			if len(ssh) != map[bool]int{false: 0, true: 1}[present] {
				t.Fatalf("installed SSH alternatives: %+v", ssh)
			}
			encoded, _ := json.Marshal(out)
			if len(encoded) > 12000 || strings.Contains(string(encoded), "capability-private-env") {
				t.Fatal("inventory exceeded fixture bound or exposed environment")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("optional package imported: %v", err)
			}
			if present {
				sshName := "ssh"
				if runtime.GOOS == "windows" {
					sshName += ".exe"
				}
				if err := os.Remove(filepath.Join(bin, sshName)); err != nil {
					t.Fatal(err)
				}
				withoutSSH := invokePreset(t, call, map[string]any{}, false)
				if len(withoutSSH["alternatives"].(map[string]any)["ssh_password_candidates"].([]any)) != 0 {
					t.Fatal("sshpass alone was presented as a complete SSH client")
				}
			}
		})
	}
	bad := invokePreset(t, m, map[string]any{"command": "ignored"}, true)
	if bad["error"].(map[string]any)["type"] != "ValueError" {
		t.Fatal(bad)
	}
}

func TestCapabilityInventoryUnknownBrowser(t *testing.T) {
	m := installedPresets(t)["capability-inventory"]
	m.Env = map[string]string{"CLOAKBROWSER_BINARY_PATH": "", "DISPLAY": "fixture-unverified-display"}
	out := invokePreset(t, m, map[string]any{}, false)
	browser := out["browser"].(map[string]any)
	binary := browser["binary"].(map[string]any)
	display := browser["display"].(map[string]any)
	if binary["configured"] != false || binary["is_file"] != nil || binary["executable"] != nil || display["configured"] != true || display["connection_verified"] != false {
		t.Fatalf("unknown and configured states conflated: %+v", browser)
	}
}
