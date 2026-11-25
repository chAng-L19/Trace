package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinNumericDecode(t *testing.T) {
	var in struct {
		Expected any `json:"expected"`
		Limit    int `json:"limit"`
	}
	if err := decodeBuiltin(`{"expected":9007199254740993,"limit":7}`, &in); err != nil {
		t.Fatal(err)
	}
	if in.Expected != json.Number("9007199254740993") || in.Limit != 7 {
		t.Fatalf("expected = %v (%T), limit = %d", in.Expected, in.Expected, in.Limit)
	}
	for _, raw := range []string{`{"limit":"7"}`, `{"limit":7.0}`, `{"limit":7e0}`, `{"limit":18446744073709551616}`, `{} {}`, `null`} {
		if err := decodeBuiltin(raw, &in); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestBuiltinNumericFindingReopen(t *testing.T) {
	const args = `{"id":"numeric","title":"Exact integer","severity":"info","root_cause":"JSON decoding","impact":"Adjacent IDs differ","remediation":"Preserve the number","reproduction_steps":["Compare stdout IDs"],"evidence_ids":["exact"],"predicate":{"kind":"json_equals","pointer":"/id","expected":9007199254740993}}`
	check := func(t *testing.T, s *Snapshot, p EvidencePredicate) {
		t.Helper()
		if p.Expected != json.Number("9007199254740993") {
			t.Fatalf("expected = %v (%T), want exact json.Number", p.Expected, p.Expected)
		}
		for id, want := range map[string]string{"exact": "pass", "neighbor": "fail"} {
			if got, reason := checkPredicate(s, p, []string{id}, "exec", "", 0); got != want {
				t.Errorf("%s: got %s (%s), want %s", id, got, reason, want)
			}
		}
	}
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			if backend == "sqlite" {
				if err := MigrateStoreToSQLite(root); err != nil {
					t.Fatal(err)
				}
			}
			store, err := NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			s, err := store.createInitialized("exact integer", "prompt", "hash", func(s *Snapshot) error {
				resultValidationSource(t, s, "exact", "exec", "exec", `{"id":9007199254740993}`)
				resultValidationSource(t, s, "neighbor", "exec", "exec", `{"id":9007199254740992}`)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			a := Agent{store: store}
			result, err := a.builtin(context.Background(), s.Run.ID, ToolCall{Name: "finding_submit", Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			findings, ok := result.([]Finding)
			if !ok || len(findings) != 1 || findings[0].ID != "numeric" {
				t.Fatalf("unexpected finding_submit result: %#v", result)
			}
			check(t, &s, findings[0].Predicate)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			s, err = reopened.Load(s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			f := findingByID(&s, "numeric")
			if f == nil {
				t.Fatal("missing finding after reopen")
			}
			check(t, &s, f.Predicate)
		})
	}
}

func TestBuiltinPredicateStrictDecode(t *testing.T) {
	for _, decode := range []struct {
		name string
		call func(string, any) error
	}{
		{"builtin", decodeBuiltin},
		{"snapshot", func(raw string, target any) error { return json.Unmarshal([]byte(raw), target) }},
	} {
		t.Run(decode.name, func(t *testing.T) {
			for _, predicate := range []string{
				`{"kind":"json_equals","pointer":"/n","expcted":7}`,
				`{"kind":"json_equals","pointer":"/n","expected":null,"expcted":7}`,
			} {
				var finding Finding
				err := decode.call(`{"predicate":`+predicate+`}`, &finding)
				if err == nil || !strings.Contains(err.Error(), `unknown field "expcted"`) {
					t.Fatalf("accepted or misdiagnosed %s: %v", predicate, err)
				}
			}
			for _, tc := range []struct {
				expected string
				want     any
			}{
				{`null`, nil},
				{`9007199254740993`, json.Number("9007199254740993")},
				{`[true,"literal",null,1e400]`, []any{true, "literal", nil, json.Number("1e400")}},
				{`{"expcted":7,"items":[9007199254740993,null]}`, map[string]any{"expcted": json.Number("7"), "items": []any{json.Number("9007199254740993"), nil}}},
			} {
				var finding Finding
				if err := decode.call(`{"predicate":{"kind":"json_equals","pointer":"/n","expected":`+tc.expected+`}}`, &finding); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(finding.Predicate.Expected, tc.want) {
					t.Fatalf("expected %s: got %#v, want %#v", tc.expected, finding.Predicate.Expected, tc.want)
				}
				pass, err := predicateMatches(finding.Predicate, `{"n":`+tc.expected+`}`)
				if err != nil || !pass {
					t.Fatalf("expected %s did not match: %t, %v", tc.expected, pass, err)
				}
			}
		})
	}
}
