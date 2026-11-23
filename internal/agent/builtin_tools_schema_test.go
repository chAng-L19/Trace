package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuiltinSchemasUseEmptyRequiredArrays(t *testing.T) {
	for _, tool := range builtinDefinitions() {
		params, ok := tool.Function.Parameters.(map[string]any)
		if !ok {
			t.Fatalf("%s parameters are not an object", tool.Function.Name)
		}
		if required, ok := params["required"].([]string); !ok || required == nil {
			t.Fatalf("%s required must be a non-nil array, got %#v", tool.Function.Name, params["required"])
		}
		wire, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Function struct {
				Parameters struct {
					Required json.RawMessage `json:"required"`
				} `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(wire, &schema); err != nil {
			t.Fatal(err)
		}
		var encodedRequired []any
		if err := json.Unmarshal(schema.Function.Parameters.Required, &encodedRequired); err != nil || encodedRequired == nil {
			t.Fatalf("%s wire required must be an array, got %s", tool.Function.Name, schema.Function.Parameters.Required)
		}
	}
}

func TestEvidenceMethodGuidancePreservesOptionalSchema(t *testing.T) {
	for _, tool := range evidenceChainDefinitions() {
		if tool.Function.Name != "evidence_chain_create" {
			continue
		}
		params := tool.Function.Parameters.(map[string]any)
		if got := params["required"].([]string); strings.Join(got, ",") != "name,summary,steps" {
			t.Fatalf("method guidance changed legacy required fields: %v", got)
		}
		properties := params["properties"].(map[string]any)
		for field, terms := range map[string][]string{
			"applicability": {"fingerprints", "versions", "prerequisites", "exclusion conditions"},
			"lesson":        {"effective and ineffective routes", "verification steps", "success and failure criteria", "unresolved questions"},
			"variant":       {"approach or revision", "derived_from"},
		} {
			schema, ok := properties[field].(map[string]string)
			if !ok || schema["type"] != "string" {
				t.Fatalf("%s lost its existing string contract", field)
			}
			for _, term := range terms {
				if !strings.Contains(schema["description"], term) {
					t.Fatalf("%s guidance missing from tool schema: %s", field, term)
				}
			}
		}
		step := properties["steps"].(map[string]any)["items"].(map[string]any)
		if strings.Join(step["required"].([]string), ",") != "evidence_id" {
			t.Fatal("method guidance changed legacy source step requirements")
		}
		return
	}
	t.Fatal("evidence_chain_create missing")
}
