package agentloop

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSchemaContractAssertionShapes(t *testing.T) {
	for _, tc := range []struct{ schema, want string }{
		{`{"required":"x"}`, "required must be an array"},
		{`{"required":["x",0]}`, "required entries must be strings"},
		{`{"required":["x","x"]}`, "required entries must be unique"},
		{`{"type":[]}`, "type must be"},
		{`{"type":["object","object"]}`, "type entries must be unique"},
		{`{"enum":null}`, "enum must be an array"},
		{`{"properties":[]}`, "properties must be an object"},
		{`{"patternProperties":false}`, "patternProperties must be an object"},
		{`{"properties":{"missing":null}}`, "schema must be an object or boolean"},
		{`{"additionalProperties":0}`, "additionalProperties"},
		{`{"propertyNames":"x"}`, "propertyNames"},
		{`{"items":"integer"}`, "items"},
		{`{"prefixItems":{}}`, "prefixItems"},
		{`{"prefixItems":[false],"items":[true]}`, "tuple items conflicts with prefixItems"},
		{`{"items":[true],"additionalItems":1}`, "additionalItems"},
		{`{"contains":null}`, "contains"},
		{`{"allOf":{}}`, "allOf"},
		{`{"anyOf":[]}`, "anyOf"},
		{`{"oneOf":true}`, "oneOf"},
		{`{"uniqueItems":"true"}`, "uniqueItems must be a boolean"},
		{`{"minLength":-1}`, "minLength must be a non-negative integer"},
		{`{"maxItems":0.5}`, "maxItems must be a non-negative integer"},
		{`{"minContains":"2"}`, "unusable minContains constraint"},
		{`{"maxContains":-1}`, "maxContains must be a non-negative integer"},
		{`{"minimum":"1"}`, "unusable minimum constraint"},
		{`{"exclusiveMaximum":true}`, "unusable exclusiveMaximum constraint"},
		{`{"multipleOf":0}`, "multipleOf must be positive"},
		{`{"pattern":7}`, "pattern must be a string"},
		{`{"format":false}`, "format must be a string"},
		{`{"$ref":[]}`, "$ref must be a string"},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			// An absent property must not hide a bad declaration.
			schema := json.RawMessage(`{"properties":{"unused":` + tc.schema + `}}`)
			err := validateArguments("contract", schema, `{}`, nil)
			if err == nil || !strings.Contains(err.Error(), "unusable parameter schema") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected schema error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSchemaContractUnsupportedAssertions(t *testing.T) {
	for key, constraint := range map[string]any{
		"dependencies":          map[string]any{"a": []string{"b"}},
		"dependentRequired":     map[string]any{"a": []string{"b"}},
		"dependentSchemas":      map[string]any{"a": false},
		"unevaluatedProperties": false,
		"unevaluatedItems":      false,
		"$dynamicRef":           "#node",
		"$recursiveRef":         "#",
	} {
		t.Run(key, func(t *testing.T) {
			schema := map[string]any{"anyOf": []any{true, map[string]any{key: constraint}}}
			assertArgumentsValidation(t, schema, `{}`, "unsupported schema keyword "+key)
		})
	}
}

func TestSchemaContractBranchesAndReferences(t *testing.T) {
	for _, tc := range []struct{ name, schema, args, want string }{
		{"not mismatch", `{"not":{"type":"string","required":"x"}}`, `{}`, "unusable parameter schema"},
		{"matched anyOf", `{"anyOf":[true,{"type":"string","pattern":"["}]}`, `{}`, "invalid pattern"},
		{"unused named schema", `{"$defs":{"unused":{"required":"x"}}}`, `{}`, "required must be an array"},
		{"indexed definition reached via ref", `{"definitions":{"table":[{"pattern":false}]},"if":true,"else":{"$ref":"#/definitions/table/0"}}`, `{}`, "pattern must be a string"},
		{"inactive then via ref", `{"$defs":{"bad":{"patternProperties":{"^x":{"required":null}}}},"if":false,"then":{"$ref":"#/$defs/bad"}}`, `{}`, "required must be an array"},
		{"inactive else via pointer to extension", `{"x-defs":[{"items":0}],"if":true,"else":{"$ref":"#/x-defs/0"}}`, `{}`, "schema must be an object or boolean"},
		{"recursive shape with invalid sibling", `{"$defs":{"node":{"properties":{"child":{"$ref":"#/$defs/node"},"next":{"uniqueItems":1}}}},"properties":{"head":{"$ref":"#/$defs/node"}}}`, `{}`, "uniqueItems must be a boolean"},
		{"format is annotation only", `{"properties":{"v":{"type":"string","format":"date-time"}}}`, `{"v":"not a date"}`, ""},
		{"annotation and extension data are opaque", `{"examples":[{"required":false}],"default":{"$ref":42},"x-extra":{"unevaluatedItems":false},"properties":{"v":{"const":{"required":42},"enum":[{"required":42}]}}}`, `{"v":{"required":42}}`, ""},
		{"unused remote ref", `{"properties":{"v":{"$ref":"https://example.invalid/x"}}}`, `{}`, ""},
		{"evaluated remote ref", `{"properties":{"v":{"$ref":"https://example.invalid/x"}}}`, `{"v":1}`, "unresolved schema reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertArgumentsValidation(t, json.RawMessage(tc.schema), tc.args, tc.want)
		})
	}
}

func TestSchemaContractContainsBounds(t *testing.T) {
	for _, tc := range []struct{ name, constraint, array, want string }{
		{"under minimum", `"contains":{"type":"integer"},"minContains":2`, `[1]`, "at least 2"},
		{"no coercion", `"contains":{"type":"integer"},"minContains":2`, `[1,"2"]`, "at least 2"},
		{"both limits inclusive", `"contains":{"type":"integer"},"minContains":2.0,"maxContains":2`, `[1,"2",3]`, ""},
		{"over maximum", `"contains":{"type":"integer"},"maxContains":1`, `[1,"2",3]`, "at most 1"},
		{"zero matches allowed", `"contains":false,"minContains":0,"maxContains":0`, `[]`, ""},
		{"zero maximum enforced", `"contains":true,"minContains":0,"maxContains":0`, `[0]`, "at most 0"},
		{"default minimum still one", `"contains":true,"maxContains":3`, `[]`, "at least 1"},
		{"without contains no count applies", `"minContains":2,"maxContains":0`, `[1]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := json.RawMessage(`{"properties":{"a":{` + tc.constraint + `}}}`)
			assertArgumentsValidation(t, schema, `{"a":`+tc.array+`}`, tc.want)
		})
	}
}

func TestSchemaContractNeverExecutes(t *testing.T) {
	f := &fake{model: func() (Turn, error) {
		return Turn{
			Tools: []Tool{
				{Name: "shape", Parameters: json.RawMessage(`{"required":"x"}`)},
				{Name: "unsupported", Parameters: json.RawMessage(`{"anyOf":[true,{"$ref":"#/x-rule"}],"x-rule":{"dependentRequired":{"a":["b"]}}}`)},
				{Name: "count", Parameters: json.RawMessage(`{"properties":{"a":{"contains":{"type":"integer"},"minContains":2}}}`)},
			},
			Calls: []Call{
				{ID: "bad", Name: "shape", Arguments: `{}`},
				{ID: "hidden", Name: "unsupported", Arguments: `{}`},
				{ID: "too-few", Name: "count", Arguments: `{"a":[1]}`},
				{ID: "cached-bad", Name: "shape", Arguments: `{"x":true}`},
			},
		}, nil
	}}
	f.execute = func(c Call, _, _ bool) (Outcome, error) {
		t.Errorf("unexpected Execute(%s)", c.ID)
		return Outcome{}, nil
	}
	if err := Run(testContext(t), f, nil); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"bad": "required must be an array", "cached-bad": "unusable parameter schema",
		"hidden": "unsupported schema keyword dependentRequired", "too-few": "at least 2",
	} {
		if out := f.outcomes[id]; !out.IsError || !out.ValidationError || !strings.Contains(out.Content, want) {
			t.Errorf("%s: expected error result containing %q, got %+v", id, want, out)
		}
	}
}

func TestSchemaContractInspectionBudget(t *testing.T) {
	// No instance visits then. The shape walk must still be bounded, and inspect
	// shared reference targets once rather than expanding the DAG exponentially.
	var deep any = true
	for i := 0; i < maxValidationDepth; i++ {
		deep = map[string]any{"not": deep}
	}
	assertArgumentsValidation(t, map[string]any{"if": false, "then": deep}, `{}`, "validation nesting budget exceeded")

	defs := map[string]any{"0": true}
	for i := 1; i <= 25; i++ {
		ref := map[string]any{"$ref": fmt.Sprintf("#/$defs/%d", i-1)}
		defs[fmt.Sprint(i)] = map[string]any{"allOf": []any{ref, ref}}
	}
	schema := map[string]any{"$defs": defs, "if": false, "then": map[string]any{"$ref": "#/$defs/25"}}
	assertArgumentsValidation(t, schema, `{}`, "")
	budget := &validationBudget{steps: maxBatchValidationSteps - 12}
	if err := validateArguments("bounded", schema, `{}`, budget); err == nil || !strings.Contains(err.Error(), "batch validation work budget exceeded") {
		t.Fatalf("inspection ignored shared step budget: %v", err)
	}
}

func TestSchemaContractStringEqualityBudget(t *testing.T) {
	const long = 64 << 10
	const repeats = maxValidationWorkBytes / long
	for _, tc := range []struct {
		name        string
		size, count int
		wantBudget  bool
	}{
		{"long below budget", long, repeats / 2, false},
		{"same refs short strings", 1, repeats, false},
		{"same refs long strings", long, repeats, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Repeat("s", tc.size)
			refs := make([]any, tc.count)
			for i := range refs {
				refs[i] = map[string]any{"$ref": "#/$defs/text"}
			}
			schema := map[string]any{
				"$defs":      map[string]any{"text": map[string]any{"const": text}},
				"properties": map[string]any{"v": map[string]any{"allOf": refs}},
			}
			err := validateArguments("equal", schema, `{"v":"`+text+`"}`, nil)
			if tc.wantBudget {
				if err == nil || !strings.Contains(err.Error(), "validation byte work budget exceeded") {
					t.Fatalf("repeated equal strings did not exhaust the byte budget: %.200v", err)
				}
			} else if err != nil {
				t.Fatalf("positive control rejected: %.200v", err)
			}
		})
	}
}

func TestSchemaContractReplayBudgetOrder(t *testing.T) {
	long := strings.Repeat("x", 2048)
	// Leave one instance diagnostic slot. Reversing two overlapping patterns
	// would spend it on the long required name and exhaust the batch instead.
	fill := strings.TrimSuffix(strings.Repeat("false,", maxValidationErrors-1), ",")
	for _, tc := range []struct {
		name, schema, args string
		diagnostic         string
		shapeBytes         int
	}{
		{"schema and child first error", `{"required":false,"properties":{"z":{"required":["` + long + `"]},"a":{"pattern":false}}}`, `{}`,
			"unusable parameter schema: a: pattern must be a string", 0},
		{"overlapping patterns at diagnostic limit", `{"allOf":[` + fill + `],"patternProperties":{"z$":{"required":["` + long + `"]},"^z":{"required":["first"]}}}`, `{"z":{}}`,
			"z: must have required property 'first'", len(long)},
		{"object equality short circuit", `{"not":{"const":{"z":"` + long + `","a":0}}}`, `{"z":"` + long + `","a":1}`, "", 0},
		{"nested resource declaration", `{"$defs":{"gate":true},"properties":{"part":{"$id":"urn:example:inner","$defs":{"gate":false},"$ref":"#/$defs/gate"}}}`, `{"part":1}`,
			"part: nested $id is unsupported in a single-resource schema", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// After decoding, room for the expected prefix and next call, but not the long field.
			startBytes := maxBatchValidationBytes - len(tc.schema) - len(tc.args) - tc.shapeBytes - 256
			var wantSteps, wantBytes int
			for replay := 0; replay < 16; replay++ {
				budget := &validationBudget{bytes: startBytes}
				err := validateArguments("first", json.RawMessage(tc.schema), tc.args, budget)
				if (err != nil) != (tc.diagnostic != "") {
					t.Fatalf("replay %d: first call: %.200v", replay, err)
				}
				if err != nil && !strings.Contains(err.Error(), tc.diagnostic) {
					t.Fatalf("replay %d: want %q, got %.200v", replay, tc.diagnostic, err)
				}
				if err := validateArguments("next", true, `{}`, budget); err != nil {
					t.Fatalf("replay %d: next call was not admitted: %v", replay, err)
				}
				if replay == 0 {
					wantSteps, wantBytes = budget.steps, budget.bytes
				} else if budget.steps != wantSteps || budget.bytes != wantBytes {
					t.Fatalf("replay %d: steps/bytes %d/%d, want %d/%d", replay, budget.steps, budget.bytes, wantSteps, wantBytes)
				}
			}
		})
	}
}

func TestSchemaContractResourceIDs(t *testing.T) {
	for _, tc := range []struct{ name, schema, args, want string }{
		{"root type number", `{"$id":1}`, `{}`, "$id must be a string"},
		{"root type null", `{"$id":null}`, `{}`, "$id must be a string"},
		{"root type object", `{"$id":{}}`, `{}`, "$id must be a string"},
		{"nested type", `{"properties":{"part":{"$id":false}}}`, `{}`, "$id must be a string"},
		{"root local pointer", `{"$id":"urn:example:root","$defs":{"gate":true},"properties":{"part":{"$ref":"#/$defs/gate"}}}`, `{"part":1}`, ""},
		{"root local recursive pointer", `{"$id":"urn:example:root","type":"object","properties":{"part":{"$ref":"#"}}}`, `{"part":{"part":{}}}`, ""},
		{"root recursive validation still applies", `{"$id":"urn:example:root","type":"object","properties":{"part":{"$ref":"#"}}}`, `{"part":1}`, "must be object"},
		{"reference to root still detects cycles", `{"$id":"urn:example:root","$ref":"#"}`, `{}`, "cyclic schema reference"},
		{"reference to opaque nested schema", `{"x-schema":{"$id":"urn:example:inner"},"anyOf":[true,{"$ref":"#/x-schema"}]}`, `{}`, "nested $id"},
		{"reference through not", `{"x-schema":{"$id":"urn:example:inner"},"not":{"$ref":"#/x-schema"}}`, `{}`, "nested $id"},
		{"inactive branch", `{"if":false,"then":{"$id":"urn:example:inner"}}`, `{}`, "nested $id"},
		{"empty property name is nested", `{"properties":{"":{"$id":"urn:example:inner"}}}`, `{}`, "nested $id"},
		{"pointer to empty property is nested", `{"$ref":"#/","":{"$id":"urn:example:inner"}}`, `{}`, "nested $id"},
		{"opaque data", `{"$id":"urn:example:root","default":{"$id":false},"examples":[{"$id":"urn:example:data"}],"x-schema":{"$id":null},"enum":[{"$id":7}],"const":{"$id":7}}`, `{"$id":7}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArguments("resource", json.RawMessage(tc.schema), tc.args, nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if strings.Contains(tc.want, "$id") && !strings.Contains(err.Error(), "unusable parameter schema") {
				t.Fatalf("resource declaration must report a schema error: %v", err)
			}
		})
	}
}

func TestSchemaContractNestedResourceNeverExecutes(t *testing.T) {
	for _, outer := range []bool{true, false} {
		for _, branch := range []string{"direct", "not", "anyOf"} {
			t.Run(fmt.Sprintf("outer=%t/%s", outer, branch), func(t *testing.T) {
				part := map[string]any{
					"$id": "urn:example:inner", "$defs": map[string]any{"gate": !outer}, "$ref": "#/$defs/gate",
				}
				var sub any = part
				if branch == "not" {
					sub = map[string]any{"not": part}
				} else if branch == "anyOf" {
					sub = map[string]any{"anyOf": []any{true, part}}
				}
				schema := map[string]any{
					"$defs": map[string]any{"gate": outer}, "properties": map[string]any{"part": sub},
				}
				f := &fake{model: func() (Turn, error) {
					return Turn{
						Tools: []Tool{{Name: "resource", Parameters: schema}},
						Calls: []Call{{ID: "resource", Name: "resource", Arguments: `{"part":1}`}},
					}, nil
				}}
				f.execute = func(Call, bool, bool) (Outcome, error) {
					t.Error("nested resource reached Execute")
					return Outcome{}, nil
				}
				if err := Run(testContext(t), f, nil); err != nil {
					t.Fatal(err)
				}
				out := f.outcomes["resource"]
				if !out.IsError || !out.ValidationError || !strings.Contains(out.Content, "unusable parameter schema") || !strings.Contains(out.Content, "nested $id") {
					t.Fatalf("expected nested resource schema error, got %+v", out)
				}
			})
		}
	}
}
