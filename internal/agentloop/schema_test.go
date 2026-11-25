package agentloop

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestValidateArgumentsStrictSubset(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"mode"},
		"$defs":                map[string]any{"port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}},
		"properties": map[string]any{
			"mode":  map[string]any{"enum": []any{"get", "post"}},
			"port":  map[string]any{"$ref": "#/$defs/port"},
			"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}, "uniqueItems": true},
			"limit": map[string]any{"type": []any{"integer", "null"}},
			"body":  map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "object"}}},
		},
	}
	for _, tc := range []struct {
		args string
		want string
	}{
		{`{"mode":"get"}`, ""},
		{`{"mode":"get","port":443,"tags":["a"],"limit":null,"body":{}}`, ""},
		{`{"mode":"get","port":1.0}`, ""},
		{`{"mode":"put"}`, "mode: must be equal to one of the allowed values"},
		{`{"mode":"get","port":"443"}`, "port: must be integer"},
		{`{"mode":"get","port":0}`, "port: must be >= 1"},
		{`{"mode":"get","port":1.5}`, "port: must be integer"},
		{`{"mode":"get","tags":"a"}`, "tags: must be array"},
		{`{"mode":"get","tags":["a","a"]}`, "duplicate items"},
		{`{"mode":"get","tags":[""]}`, "tags.0: must NOT have fewer than 1 characters"},
		{`{"mode":"get","extra":1}`, "must NOT have additional property 'extra'"},
		{`{"mode":"get","body":5}`, "body: must match a schema in anyOf"},
		{`{}`, "root: must have required property 'mode'"},
		{`{"mode":"get"} {}`, "one JSON value"},
		{`{"mode":"get"}]`, "one JSON value"},
		{`{"mode":"get"}}`, "one JSON value"},
	} {
		err := validateArguments("fixture", schema, tc.args, nil)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s rejected: %v", tc.args, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v want %q", tc.args, err, tc.want)
		}
	}
}

func TestValidateArgumentsNilSchemaRequiresObject(t *testing.T) {
	if err := validateArguments("x", nil, `{"any":1}`, nil); err != nil {
		t.Fatal(err)
	}
	if err := validateArguments("x", nil, `"text"`, nil); err == nil {
		t.Fatal("non-object arguments accepted")
	}
}

func TestValidateArgumentsUnresolvedRefRejectsOnlyItsPlace(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"$ref": "#/$defs/missing"}}}
	if err := validateArguments("x", schema, `{}`, nil); err != nil {
		t.Fatalf("absent property rejected: %v", err)
	}
	if err := validateArguments("x", schema, `{"a":1}`, nil); err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("dangling reference accepted: %v", err)
	}
}

func TestValidateArgumentsExactNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, schema, argument, want string
	}{
		{"enum boundary", `{"enum":[9007199254740992]}`, "9007199254740992", ""},
		{"enum adjacent integer", `{"enum":[9007199254740992]}`, "9007199254740993", "must be equal to one of the allowed values"},
		{"enum negative adjacent integer", `{"enum":[-9007199254740992]}`, "-9007199254740993", "must be equal to one of the allowed values"},
		{"enum adjacent fraction", `{"enum":[1]}`, "1.00000000000000000001", "must be equal to one of the allowed values"},
		{"const adjacent integer", `{"const":9007199254740992}`, "9007199254740993", "must be equal to constant"},
		{"const equivalent exponent", `{"const":9007199254740993}`, "9.007199254740993e15", ""},
		{"const equivalent decimal", `{"const":1}`, "1.0", ""},
		{"nested enum integer", `{"enum":[{"count":9007199254740992}]}`, `{"count":9007199254740993}`, "must be equal to one of the allowed values"},
		{"nested const integer", `{"const":[9007199254740992]}`, "[9007199254740993]", "must be equal to constant"},
		{"large integer", `{"type":"integer"}`, "9007199254740993", ""},
		{"large fractional integer", `{"type":"integer"}`, "9007199254740992.5", "must be integer"},
		{"small fractional integer", `{"type":"integer"}`, "1.00000000000000000001", "must be integer"},
		{"integral decimal", `{"type":"integer"}`, "1.000", ""},
		{"integer beyond float range", `{"type":"integer"}`, "1e400", ""},
		{"fraction below float range", `{"type":"integer"}`, "1e-400", "must be integer"},
		{"minimum adjacent integer", `{"minimum":9007199254740993}`, "9007199254740992", "must be >= 9007199254740993"},
		{"maximum adjacent integer", `{"maximum":9007199254740992}`, "9007199254740993", "must be <= 9007199254740992"},
		{"negative maximum", `{"maximum":-9007199254740993}`, "-9007199254740992", "must be <= -9007199254740993"},
		{"exclusive minimum adjacent integer", `{"exclusiveMinimum":9007199254740992}`, "9007199254740993", ""},
		{"exclusive maximum adjacent integer", `{"exclusiveMaximum":9007199254740993}`, "9007199254740992", ""},
		{"exclusive minimum equality", `{"exclusiveMinimum":9007199254740993}`, "9007199254740993", "must be > 9007199254740993"},
		{"exclusive maximum equality", `{"exclusiveMaximum":9007199254740993}`, "9007199254740993", "must be < 9007199254740993"},
		{"fractional minimum", `{"minimum":0.100000000000000000002}`, "0.100000000000000000001", "must be >= 0.100000000000000000002"},
		{"tiny minimum", `{"minimum":1e-400}`, "0", "must be >= 1e-400"},
		{"tiny maximum", `{"maximum":1e-400}`, "1e-399", "must be <= 1e-400"},
		{"huge minimum", `{"minimum":1e400}`, "1e399", "must be >= 1e400"},
		{"unsupported positive exponent", `{"type":"number"}`, "1e999999999999999999999999999999", "exponent exceeds 4096"},
		{"unsupported negative exponent", `{"type":"number"}`, "1e-999999999999999999999999999999", "exponent exceeds 4096"},
		{"unsupported bound exponent", `{"minimum":1e999999999999999999999999999999}`, "0", "exponent exceeds 4096"},
		{"multiple large odd integer", `{"multipleOf":2}`, "9007199254740993", "must be multiple of 2"},
		{"multiple decimal", `{"multipleOf":0.1}`, "0.3", ""},
		{"multiple near decimal", `{"multipleOf":0.1}`, "0.30000000000000000001", "must be multiple of 0.1"},
		{"multiple negative decimal", `{"multipleOf":0.1}`, "-0.3", ""},
		{"multiple below float range", `{"multipleOf":2e-400}`, "1e-400", "must be multiple of 2e-400"},
		{"multiple beyond float range", `{"multipleOf":1e399}`, "1e400", ""},
		{"zero multiple", `{"multipleOf":0.1}`, "0", ""},
		{"invalid zero divisor", `{"multipleOf":0}`, "1", "multipleOf must be positive"},
		{"numeric string", `{"type":"integer"}`, `"1"`, "must be integer"},
		{"unique adjacent integers", `{"uniqueItems":true}`, "[9007199254740992,9007199254740993]", ""},
		{"duplicate numeric representations", `{"uniqueItems":true}`, "[9007199254740993,9.007199254740993e15]", "duplicate items"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := json.RawMessage(`{"properties":{"n":` + tc.schema + `}}`)
			assertArgumentsValidation(t, schema, `{"n":`+tc.argument+`}`, tc.want)
		})
	}
}

func TestValidateArgumentsLocalReferenceCycles(t *testing.T) {
	for _, tc := range []struct {
		name, schema, arguments, want string
	}{
		{"direct", `{"$ref":"#"}`, "{}", "cyclic schema reference"},
		{"indirect", `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`, "{}", "cyclic schema reference"},
		{"allOf", `{"allOf":[{"$ref":"#"}]}`, "{}", "cyclic schema reference"},
		{"anyOf", `{"anyOf":[{"$ref":"#"}]}`, "{}", "cyclic schema reference"},
		{"anyOf after match", `{"anyOf":[true,{"$ref":"#"}]}`, "{}", "cyclic schema reference"},
		{"oneOf", `{"oneOf":[false,{"$ref":"#"}]}`, "{}", "cyclic schema reference"},
		{"not", `{"not":{"$ref":"#"}}`, "{}", "cyclic schema reference"},
		{"if", `{"if":{"$ref":"#"},"else":true}`, "{}", "cyclic schema reference"},
		{"contains probe", `{"$defs":{"loop":{"$ref":"#/$defs/loop"}},"properties":{"n":{"contains":{"$ref":"#/$defs/loop"}}}}`, `{"n":[1]}`, "cyclic schema reference"},
		{"unresolved local anyOf", `{"anyOf":[true,{"$ref":"#/$defs/missing"}]}`, "{}", "unresolved schema reference"},
		{"remote anyOf", `{"anyOf":[true,{"$ref":"https://example.invalid/schema"}]}`, "{}", "unresolved schema reference"},
		{"remote if", `{"if":{"$ref":"https://example.invalid/schema"},"else":true}`, "{}", "unresolved schema reference"},
		{"remote not", `{"not":{"$ref":"https://example.invalid/schema"}}`, "{}", "unresolved schema reference"},
		{"encoded cycle", `{"$defs":{"a b":{"$ref":"#/$defs/a%20b"}},"$ref":"#/$defs/a%20b"}`, "{}", "cyclic schema reference"},
		{"encoded alias cycle", `{"$defs":{"node":{"$ref":"#/$defs/%6eode"}},"$ref":"#/$defs/node"}`, "{}", "cyclic schema reference"},
		{"encoded recursive objects", `{"$defs":{"node type":{"type":"object","properties":{"next":{"$ref":"#/$defs/node%20type"}}}},"$ref":"#/$defs/node%20type"}`, `{"next":{"next":{}}}`, ""},
		{"invalid encoded recursive object", `{"$defs":{"node type":{"type":"object","properties":{"next":{"$ref":"#/$defs/node%20type"}}}},"$ref":"#/$defs/node%20type"}`, `{"next":1}`, "must be object"},
		{"sibling references", `{"$defs":{"n":{"type":"integer"}},"properties":{"a":{"$ref":"#/$defs/n"},"b":{"$ref":"#/$defs/n"}}}`, `{"a":1,"b":2}`, ""},
		{"recursive objects", `{"$defs":{"node":{"anyOf":[{"type":"null"},{"type":"object","required":["value"],"properties":{"value":{"type":"integer"},"next":{"$ref":"#/$defs/node"}}}]}},"properties":{"node":{"$ref":"#/$defs/node"}}}`, `{"node":{"value":1,"next":{"value":2,"next":null}}}`, ""},
		{"invalid recursive object", `{"$defs":{"node":{"anyOf":[{"type":"null"},{"type":"object","required":["value"],"properties":{"value":{"type":"integer"},"next":{"$ref":"#/$defs/node"}}}]}},"properties":{"node":{"$ref":"#/$defs/node"}}}`, `{"node":{"value":1,"next":{"value":1.5}}}`, "must match a schema in anyOf"},
		{"recursive items", `{"$defs":{"node":{"anyOf":[{"type":"integer"},{"type":"array","items":{"$ref":"#/$defs/node"}}]}},"properties":{"node":{"$ref":"#/$defs/node"}}}`, `{"node":[1,[2,[3]]]}`, ""},
		{"recursive contains", `{"$defs":{"node":{"anyOf":[{"type":"integer"},{"type":"array","contains":{"$ref":"#/$defs/node"}}]}},"properties":{"node":{"$ref":"#/$defs/node"}}}`, `{"node":[[1]]}`, ""},
		{"empty property names descend", `{"properties":{"":{"$ref":"#"}}}`, `{"":{"":{}}}`, ""},
		{"escaped property names descend", `{"properties":{"a/b~c":{"$ref":"#"}}}`, `{"a/b~c":{"a/b~c":{}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertArgumentsValidation(t, json.RawMessage(tc.schema), tc.arguments, tc.want)
		})
	}
}

func TestValidateArgumentsLocalReferenceFragments(t *testing.T) {
	for _, tc := range []struct {
		name, schema, want string
	}{
		{"space rejects", `{"type":"object","$defs":{"a b":false,"a%20b":true},"$ref":"#/$defs/a%20b"}`, "must not be present"},
		{"space accepts", `{"$defs":{"a b":true,"a%20b":false},"$ref":"#/$defs/a%20b"}`, ""},
		{"literal percent20 accepts", `{"$defs":{"a b":false,"a%20b":true},"$ref":"#/$defs/a%2520b"}`, ""},
		{"literal percent20 rejects", `{"$defs":{"a b":true,"a%20b":false},"$ref":"#/$defs/a%2520b"}`, "must not be present"},
		{"literal plus rejects", `{"$defs":{"a b":true,"a+b":false},"$ref":"#/$defs/a+b"}`, "must not be present"},
		{"literal plus accepts", `{"$defs":{"a b":false,"a+b":true},"$ref":"#/$defs/a+b"}`, ""},
		{"encoded plus", `{"$defs":{"a b":true,"a+b":false,"a%2Bb":true},"$ref":"#/$defs/a%2Bb"}`, "must not be present"},
		{"encoded separator rejects", `{"$defs":{"nested":{"gate":false},"nested/gate":true},"$ref":"#/$defs/nested%2Fgate"}`, "must not be present"},
		{"encoded separator accepts", `{"$defs":{"nested":{"gate":true},"nested/gate":false},"$ref":"#/$defs/nested%2fgate"}`, ""},
		{"encoded leading separator", `{"$defs":{"gate":true},"$ref":"#%2F$defs%2Fgate"}`, ""},
		{"encoded slash escape", `{"$defs":{"a/b":false,"a%7E1b":true},"$ref":"#/$defs/a%7E1b"}`, "must not be present"},
		{"encoded tilde escape", `{"$defs":{"a~b":false,"a%7e0b":true},"$ref":"#/$defs/a%7e0b"}`, "must not be present"},
		{"tilde escape order", `{"$defs":{"a~1b":false,"a/b":true,"a%7E01b":true},"$ref":"#/$defs/a%7E01b"}`, "must not be present"},
		{"trailing tilde", `{"$defs":{"a~":true},"$ref":"#/$defs/a~"}`, "unresolved schema reference"},
		{"encoded trailing tilde", `{"$defs":{"a~":true,"a%7E":true},"$ref":"#/$defs/a%7E"}`, "unresolved schema reference"},
		{"invalid tilde digit", `{"$defs":{"a~2b":true},"$ref":"#/$defs/a~2b"}`, "unresolved schema reference"},
		{"encoded invalid tilde digit", `{"type":"object","$defs":{"a~2b":true},"$ref":"#/$defs/a%7E2b"}`, "unresolved schema reference"},
		{"repeated tilde", `{"$defs":{"a~~b":true},"$ref":"#/$defs/a~~0b"}`, "unresolved schema reference"},
		{"escaped literal tilde digit", `{"$defs":{"a~2b":true},"$ref":"#/$defs/a~02b"}`, ""},
		{"encoded escaped literal tilde digit", `{"$defs":{"a~2b":true},"$ref":"#/$defs/a%7E02b"}`, ""},
		{"escaped trailing tilde", `{"$defs":{"a~":true},"$ref":"#/$defs/a~0"}`, ""},
		{"adjacent valid escapes", `{"$defs":{"a~/b":true},"$ref":"#/$defs/a~0~1b"}`, ""},
		{"invalid UTF8 byte", `{"$defs":{"\ufffd":true},"$ref":"#/$defs/%FF"}`, "unresolved schema reference"},
		{"truncated UTF8", `{"$defs":{"\ufffd":true},"$ref":"#/$defs/%E4%B8"}`, "unresolved schema reference"},
		{"valid replacement character", `{"$defs":{"\ufffd":true},"$ref":"#/$defs/%EF%BF%BD"}`, ""},
		{"encoded UTF8", `{"$defs":{"a中b":true,"a%E4%B8%ADb":false},"$ref":"#/$defs/a%E4%B8%ADb"}`, ""},
		{"incomplete percent", `{"$defs":{"a%":true},"$ref":"#/$defs/a%"}`, "unresolved schema reference"},
		{"incomplete hex pair", `{"$defs":{"a%2":true},"$ref":"#/$defs/a%2"}`, "unresolved schema reference"},
		{"invalid hex pair", `{"$defs":{"a%GG":true},"$ref":"#/$defs/a%GG"}`, "unresolved schema reference"},
		{"malformed reference after matching branch", `{"$defs":{"a%":true},"anyOf":[true,{"$ref":"#/$defs/a%"}]}`, "unresolved schema reference"},
		{"encoded anchor is not a pointer", `{"$defs":{"gate":true},"$ref":"#%24defs%2Fgate"}`, "unresolved schema reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertArgumentsValidation(t, json.RawMessage(tc.schema), `{}`, tc.want)
		})
	}
}

func TestLocalReferenceFragmentURISyntax(t *testing.T) {
	for _, tc := range []struct {
		name, key, reference string
		valid                bool
	}{
		{"raw hash", "a#b", "a#b", false},
		{"encoded hash", "a#b", "a%23b", true},
		{"raw space", "a b", "a b", false},
		{"encoded space", "a b", "a%20b", true},
		{"raw brackets", "a[b]", "a[b]", false},
		{"encoded brackets", "a[b]", "a%5Bb%5D", true},
		{"raw backslash", `a\b`, `a\b`, false},
		{"encoded backslash", `a\b`, "a%5Cb", true},
		{"raw unicode", "中文", "中文", false},
		{"encoded unicode", "中文", "%E4%B8%AD%E6%96%87", true},
		{"allowed fragment characters", "Az09-._!$&'()*+,;=:@?", "Az09-._!$&'()*+,;=:@?", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := map[string]any{"type": "object", "$defs": map[string]any{tc.key: true}, "$ref": "#/$defs/" + tc.reference}
			want := "unresolved schema reference"
			if tc.valid {
				want = ""
			}
			assertArgumentsValidation(t, schema, `{}`, want)
		})
	}
}

func TestLocalReferenceFragmentsRejectInvalidUTF8(t *testing.T) {
	// Test directly so invalid bytes cannot hide behind a missing JSON key.
	for _, tc := range []struct {
		name, fragment, key string
	}{
		{"continuation byte", "%80", "\x80"},
		{"truncated sequence", "%E4%B8", "\xe4\xb8"},
		{"overlong sequence", "%C0%AF", "\xc0\xaf"},
		{"surrogate", "%ED%A0%80", "\xed\xa0\x80"},
		{"out of range", "%F4%90%80%80", "\xf4\x90\x80\x80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := validator{root: map[string]any{tc.key: true}}
			if _, found := v.resolve("#/" + tc.fragment); found {
				t.Fatal("invalid UTF8 reference accepted")
			}
		})
	}
}

func TestLocalReferenceFragmentsNeverExecute(t *testing.T) {
	for _, tc := range []struct {
		name, schema, want string
	}{
		{"space", `{"type":"object","$defs":{"a b":false,"a%20b":true},"$ref":"#/$defs/a%20b"}`, "must not be present"},
		{"raw hash", `{"type":"object","$defs":{"a#b":true},"$ref":"#/$defs/a#b"}`, "unresolved schema reference"},
		{"raw space", `{"type":"object","$defs":{"a b":true},"$ref":"#/$defs/a b"}`, "unresolved schema reference"},
		{"separator", `{"$defs":{"nested":{"gate":false},"nested/gate":true},"$ref":"#/$defs/nested%2Fgate"}`, "must not be present"},
		{"invalid escape", `{"$defs":{"a%":true},"$ref":"#/$defs/a%"}`, "unresolved schema reference"},
		{"invalid tilde digit", `{"type":"object","$defs":{"a~2b":true},"$ref":"#/$defs/a%7E2b"}`, "unresolved schema reference"},
		{"trailing tilde", `{"$defs":{"a~":true},"$ref":"#/$defs/a%7E"}`, "unresolved schema reference"},
		{"invalid UTF8", `{"$defs":{"\ufffd":true},"$ref":"#/$defs/%FF"}`, "unresolved schema reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{model: func() (Turn, error) {
				return Turn{
					Tools: []Tool{{Name: "fragment", Parameters: json.RawMessage(tc.schema)}},
					Calls: []Call{{ID: "blocked", Name: "fragment", Arguments: `{}`}},
				}, nil
			}}
			executions := 0
			f.execute = func(Call, bool, bool) (Outcome, error) {
				executions++
				return Outcome{}, nil
			}
			if err := Run(testContext(t), f, nil); err != nil {
				t.Fatal(err)
			}
			if executions != 0 {
				t.Fatalf("Execute called %d times for rejected reference", executions)
			}
			if len(f.results) != 1 || f.results[0] != "blocked" {
				t.Fatalf("expected one rejected result, got %v", f.results)
			}
			out := f.outcomes["blocked"]
			if !out.IsError || !out.ValidationError || !strings.Contains(out.Content, tc.want) {
				t.Fatalf("expected validation result containing %q, got %+v", tc.want, out)
			}
		})
	}
}

func TestValidateArgumentsInvalidPatterns(t *testing.T) {
	for _, tc := range []struct {
		name, schema, arguments, want string
	}{
		{"invalid pattern", `{"properties":{"n":{"pattern":"["}}}`, `{"n":"x"}`, "invalid pattern"},
		{"unsupported lookahead", `{"properties":{"n":{"pattern":"(?=x)"}}}`, `{"n":"x"}`, "invalid pattern"},
		{"unsupported backreference", `{"properties":{"n":{"pattern":"(x)\\1"}}}`, `{"n":"xx"}`, "invalid pattern"},
		{"invalid patternProperties", `{"patternProperties":{"[":false}}`, `{"x":1}`, "invalid patternProperties pattern"},
		{"invalid patternProperties empty object", `{"patternProperties":{"[":false}}`, "{}", "invalid patternProperties pattern"},
		{"unsupported patternProperties", `{"patternProperties":{"(?=x)":false}}`, `{"x":1}`, "invalid patternProperties pattern"},
		{"anyOf after match", `{"anyOf":[true,{"properties":{"n":{"pattern":"["}}}]}`, `{"n":"x"}`, "invalid pattern"},
		{"not probe", `{"properties":{"n":{"not":{"pattern":"["}}}}`, `{"n":"x"}`, "invalid pattern"},
		{"matching pattern", `{"properties":{"n":{"pattern":"^x$"}}}`, `{"n":"x"}`, ""},
		{"mismatching pattern", `{"properties":{"n":{"pattern":"^x$"}}}`, `{"n":"y"}`, "must match pattern"},
		{"matching property pattern", `{"patternProperties":{"^x":{"type":"integer"}}}`, `{"x":1}`, ""},
		{"mismatching property value", `{"patternProperties":{"^x":{"type":"integer"}}}`, `{"x":"1"}`, "x: must be integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertArgumentsValidation(t, json.RawMessage(tc.schema), tc.arguments, tc.want)
		})
	}
}

func assertArgumentsValidation(t *testing.T, schema any, arguments, want string) {
	t.Helper()
	err := validateArguments("fixture", schema, arguments, nil)
	if want == "" {
		if err != nil {
			t.Fatalf("%s rejected: %v", arguments, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: got %v, want %q", arguments, err, want)
	}
}

func TestValidationResourceLimits(t *testing.T) {
	unique := map[string]any{"type": "object", "properties": map[string]any{
		"values": map[string]any{"type": "array", "uniqueItems": true},
	}}
	for _, tc := range []struct {
		name  string
		count int
		same  bool
		want  string
	}{
		{"duplicates", 2000, true, "duplicate items"},
		{"distinct bounded", 100, false, ""},
		{"distinct over budget", 1000, false, "validation work budget exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := make([]int, tc.count)
			for i := range values {
				if !tc.same {
					values[i] = i
				}
			}
			raw, err := json.Marshal(map[string]any{"values": values})
			if err != nil {
				t.Fatal(err)
			}
			err = validateArguments("bounded", unique, string(raw), nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if tc.same && strings.Count(err.Error(), "duplicate items") != 1 {
				t.Fatal("duplicate diagnostics were not bounded")
			}
		})
	}
	t.Run("shared reference DAG", func(t *testing.T) {
		defs := map[string]any{"n0": true}
		for i := 1; i <= 20; i++ {
			ref := map[string]any{"$ref": fmt.Sprintf("#/$defs/n%d", i-1)}
			defs[fmt.Sprintf("n%d", i)] = map[string]any{"allOf": []any{ref, ref}}
		}
		schema := map[string]any{"$defs": defs, "$ref": "#/$defs/n20"}
		v := validator{root: schema, references: make(map[referenceVisit]bool)}
		v.check(map[string]any{}, schema, "", "")
		if !v.exhausted || v.steps != maxValidationSteps {
			t.Fatalf("exhausted=%v steps=%d", v.exhausted, v.steps)
		}
		for _, probe := range []string{"anyOf", "oneOf", "not", "if"} {
			var wrapper any = schema
			if probe == "anyOf" || probe == "oneOf" {
				wrapper = []any{schema}
			}
			outer := map[string]any{"$defs": defs, probe: wrapper}
			err := validateArguments("bounded", outer, "{}", nil)
			if err == nil || !strings.Contains(err.Error(), "validation work budget exceeded") {
				t.Fatalf("%s swallowed budget exhaustion: %v", probe, err)
			}
		}
	})
	t.Run("diagnostic cap preserves probe failure", func(t *testing.T) {
		v := validator{references: make(map[referenceVisit]bool)}
		for i := 0; i < maxValidationErrors*2; i++ {
			v.fail("", "invalid")
			v.failSchema("", "invalid schema")
		}
		if len(v.errors) != maxValidationErrors || len(v.schemaErrors) != maxValidationErrors {
			t.Fatalf("diagnostic counts=%d,%d", len(v.errors), len(v.schemaErrors))
		}
		if v.valid("x", map[string]any{"type": "number"}, "", "") {
			t.Fatal("diagnostic cap made invalid probe match")
		}
	})
}

func TestValidationCountsUnmatchedPatternsAndBoundsDiagnostics(t *testing.T) {
	value := map[string]any{}
	patterns := map[string]any{}
	for i := 0; i < 400; i++ {
		value[fmt.Sprintf("key%03d", i)] = i
		patterns[fmt.Sprintf("^pattern%03d$", i)] = true
	}
	schema := map[string]any{"patternProperties": patterns}
	v := validator{root: schema, references: make(map[referenceVisit]bool)}
	v.check(value, schema, "", "")
	// Step or regex byte budget may trip first; either way the scan is bounded.
	if !v.exhausted || v.steps > maxValidationSteps {
		t.Fatalf("unmatched patterns bypassed budget: exhausted=%v steps=%d", v.exhausted, v.steps)
	}
	for _, raw := range []string{
		`{"value":"` + strings.Repeat("x", 100000) + `"}`,
		strings.Repeat(`{"v":`, 500) + `null` + strings.Repeat("}", 500),
	} {
		err := validateArguments("bounded", false, raw, nil)
		if err == nil || len(err.Error()) > maxDiagnosticBytes+1024 {
			t.Fatalf("diagnostic was not bounded: error=%v", err != nil)
		}
	}
}

func TestDuplicateItemsPreserveSchemaErrors(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"values": map[string]any{"not": map[string]any{
			"uniqueItems": true, "items": map[string]any{"$ref": "https://invalid.example/schema"},
		}},
	}}
	err := validateArguments("bounded", schema, `{"values":[1,1]}`, nil)
	if err == nil || !strings.Contains(err.Error(), "unresolved schema reference") {
		t.Fatalf("duplicate hid schema error under not: %v", err)
	}
}

func TestRecursiveValidationMemoryBudgets(t *testing.T) {
	for _, tc := range []struct {
		name, want    string
		prefix, depth int
	}{
		{"bounded", "", 32, 5},
		{"long prefix", "validation path budget exceeded", 50000, 20},
		{"deep recursion", "validation nesting budget exceeded", 1, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := strings.Repeat("p", tc.prefix)
			ref := map[string]any{"$ref": "#/$defs/node"}
			schema := map[string]any{
				"type": "object", "properties": map[string]any{prefix: ref},
				"$defs": map[string]any{"node": map[string]any{
					"type": "object", "properties": map[string]any{"x": ref},
				}},
			}
			value := map[string]any{}
			for i := 0; i < tc.depth; i++ {
				value = map[string]any{"x": value}
			}
			value = map[string]any{prefix: value}
			v := validator{root: schema, references: make(map[referenceVisit]bool)}
			v.check(value, schema, "", "")
			if v.pathBytes > maxValidationPathBytes || v.depth != 0 || len(v.references) != 0 {
				t.Fatalf("bytes=%d depth=%d refs=%d", v.pathBytes, v.depth, len(v.references))
			}
			if (tc.want != "") != v.exhausted || !strings.Contains(v.exhaustion, tc.want) {
				t.Fatalf("exhausted=%v reason=%q want=%q", v.exhausted, v.exhaustion, tc.want)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			err = validateArguments("recursive", schema, string(raw), nil)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			f := &fake{model: func() (Turn, error) {
				return Turn{Calls: []Call{{ID: "recursive", Name: "read", Arguments: string(raw)}},
					Tools: []Tool{{Name: "read", Parameters: schema}}}, nil
			}}
			if err := Run(testContext(t), f, nil); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(f.log, ","); got != "prepare,model,finish" {
				t.Fatalf("recursive budget crossed execution boundary: %s", got)
			}
			if outcome := f.outcomes["recursive"]; !outcome.IsError || !strings.Contains(outcome.Content, tc.want) {
				t.Fatalf("missing recursive budget failure: %+v", outcome)
			}
		})
	}
}

func TestExhaustionReportedOnce(t *testing.T) {
	err := validateArguments("x", map[string]any{"properties": map[string]any{"n": map[string]any{"type": "number"}}}, `{"n":1e999999999999999999999999999999}`, nil)
	if err == nil {
		t.Fatal("number accepted")
	}
	if strings.Count(err.Error(), "exponent exceeds 4096") != 1 {
		t.Fatalf("exhaustion repeated: %v", err)
	}
}

func TestValidateArgumentsSchemaShapeStrictness(t *testing.T) {
	for _, tc := range []struct {
		name, schema, args, want string
	}{
		{"tuple within length", `{"properties":{"a":{"items":[{"type":"integer"}],"additionalItems":false}}}`, `{"a":[1]}`, ""},
		{"tuple extra item rejected", `{"properties":{"a":{"items":[{"type":"integer"}],"additionalItems":false}}}`, `{"a":[1,2]}`, "must NOT have more than 1 items"},
		{"tuple additional schema", `{"properties":{"a":{"items":[{"type":"integer"}],"additionalItems":{"type":"string"}}}}`, `{"a":[1,2]}`, "a.1: must be string"},
		{"unknown type rejected", `{"properties":{"a":{"type":"int"}}}`, `{"a":1}`, "unknown schema type"},
		{"unknown type inside probe", `{"properties":{"a":{"anyOf":[{"type":"int"},{"type":"integer"}]}}}`, `{"a":1}`, "unknown schema type"},
		{"non-string type entry", `{"properties":{"a":{"type":["integer",5]}}}`, `{"a":1}`, "type entries must be strings"},
		{"malformed subschema", `{"properties":{"a":5}}`, `{"a":1}`, "schema must be an object or boolean"},
		{"canonical ref index", `{"$defs":{"l":[{"type":"integer"},{"type":"string"}]},"properties":{"a":{"$ref":"#/$defs/l/1"}}}`, `{"a":1}`, "a: must be string"},
		{"signed ref index", `{"$defs":{"l":[{"type":"string"}]},"properties":{"a":{"$ref":"#/$defs/l/+0"}}}`, `{"a":1}`, "unresolved"},
		{"leading zero ref index", `{"$defs":{"l":[{"type":"string"}]},"properties":{"a":{"$ref":"#/$defs/l/00"}}}`, `{"a":1}`, "unresolved"},
	} {
		var schema any
		if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
			t.Fatal(err)
		}
		err := validateArguments("x", schema, tc.args, nil)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s rejected: %v", tc.name, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v want %q", tc.name, err, tc.want)
		}
	}
}
