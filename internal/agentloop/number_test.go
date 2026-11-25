package agentloop

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseNumberExactAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name, token, want string
	}{
		{"zero", "-0", "0"},
		{"decimal integer", "1.0", "1"},
		{"decimal fraction", "0.125", "1/8"},
		{"small fraction", "1.00000000000000000001", "100000000000000000001/100000000000000000000"},
		{"adjacent even", "9007199254740992", "9007199254740992"},
		{"adjacent odd", "9007199254740993", "9007199254740993"},
		{"negative odd", "-9007199254740993", "-9007199254740993"},
		{"positive exponent", "1e+400", "1" + strings.Repeat("0", 400)},
		{"negative exponent", "1e-400", "1/1" + strings.Repeat("0", 400)},
		{"positive exponent boundary", "1e4096", "1" + strings.Repeat("0", 4096)},
		{"negative exponent boundary", "1e-4096", "1/1" + strings.Repeat("0", 4096)},
		{"leading exponent zeros", "1E+0004096", "1" + strings.Repeat("0", 4096)},
		{"token boundary", "1" + strings.Repeat("0", 4095), "1" + strings.Repeat("0", 4095)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := validator{}
			n, ok := v.parseNumber(json.Number(tc.token))
			if !ok || n == nil || v.exhausted || n.RatString() != tc.want {
				t.Fatalf("parse %s: n=%v ok=%v exhausted=%v", tc.token, n, ok, v.exhausted)
			}
		})
	}
	for _, value := range []any{nil, "1", 1, 1.0, true} {
		v := validator{}
		if n, ok := v.parseNumber(value); n != nil || ok || v.exhausted {
			t.Fatalf("non-JSON number %v: n=%v ok=%v exhausted=%v", value, n, ok, v.exhausted)
		}
	}
}

func TestParseNumberRejectsExtremeOrInvalidTokens(t *testing.T) {
	for _, token := range []string{
		"1e4097", "1e-4097", "1e999999999999999999999999999999", "1e-999999999999999999999999999999",
		"1" + strings.Repeat("0", 4096), "", "1e", "1e+", "1e1x", "1/2", "0x1p2", "01", "true",
	} {
		t.Run(token, func(t *testing.T) {
			v := validator{}
			if n, ok := v.parseNumber(json.Number(token)); n != nil || ok || !v.exhausted || !strings.Contains(v.exhaustion, "validation numeric budget exceeded") {
				t.Fatalf("invalid number accepted: n=%v ok=%v exhausted=%v reason=%q", n, ok, v.exhausted, v.exhaustion)
			}
			if n, ok := v.parseNumber(json.Number("1")); n != nil || ok {
				t.Fatal("numeric exhaustion was cleared by a later valid number")
			}
		})
	}
}

func TestParseNumberChargesRepeatedExpansion(t *testing.T) {
	v := validator{}
	// Each parse expands 4096 decimal places, even when its token is reused.
	for i := 0; i <= maxValidationWorkBytes/4096; i++ {
		if n, ok := v.parseNumber(json.Number("1e4096")); !ok {
			if i == 0 || n != nil || !v.exhausted || !strings.Contains(v.exhaustion, "budget exceeded") {
				t.Fatalf("wrong cumulative failure: parse=%d n=%v exhausted=%v reason=%q", i, n, v.exhausted, v.exhaustion)
			}
			return
		}
	}
	t.Fatal("repeated numeric expansion did not consume the byte budget")
}

func TestParseNumberChargesBeforeAllocation(t *testing.T) {
	v := validator{}
	if !v.spendBytes(maxValidationWorkBytes) {
		t.Fatal("could not fill validation byte budget")
	}
	if n, ok := v.parseNumber(json.Number("1e4096")); n != nil || ok || !v.exhausted {
		t.Fatalf("number ignored exhausted byte budget: n=%v ok=%v exhausted=%v", n, ok, v.exhausted)
	}
}

func TestBoundedNumbersCannotBeHiddenBySchemaProbes(t *testing.T) {
	for _, tc := range []struct {
		name, schema, arguments string
	}{
		{"unique without items", `{"properties":{"n":{"uniqueItems":true}}}`, `{"n":[1e999999999999999999999999999999,1e999999999999999999999999999999]}`},
		{"negative exponent unique without items", `{"properties":{"n":{"uniqueItems":true}}}`, `{"n":[1e-4097,1e-4097]}`},
		{"nested not", `{"not":{"not":{"properties":{"n":{"uniqueItems":true}}}}}`, `{"n":[1e4097,1e4097]}`},
		{"single not", `{"not":{"properties":{"n":{"uniqueItems":true}}}}`, `{"n":[1e4097,1e4097]}`},
		{"anyOf after match", `{"anyOf":[true,{"properties":{"n":{"uniqueItems":true}}}]}`, `{"n":[1e4097,1e4097]}`},
		{"not oversized token", `{"not":{"properties":{"n":{"uniqueItems":true}}}}`, `{"n":[` + strings.Repeat("9", 4097) + "," + strings.Repeat("9", 4097) + "]}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := json.RawMessage(tc.schema)
			err := validateArguments("numeric", schema, tc.arguments, nil)
			if err == nil || !strings.Contains(err.Error(), "validation numeric budget exceeded") {
				t.Fatalf("numeric budget swallowed: %v", err)
			}
			f := &fake{model: func() (Turn, error) {
				return Turn{Calls: []Call{{ID: "numeric", Name: "numeric", Arguments: tc.arguments}},
					Tools: []Tool{{Name: "numeric", Parameters: schema}}}, nil
			}}
			f.execute = func(Call, bool, bool) (Outcome, error) {
				t.Error("numeric budget exhaustion crossed the execution boundary")
				return Outcome{Content: "unexpected execution"}, nil
			}
			if err := Run(testContext(t), f, nil); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(f.log, ","); got != "prepare,model,finish" {
				t.Fatalf("numeric budget crossed execution boundary: %s", got)
			}
			if outcome := f.outcomes["numeric"]; !outcome.IsError || !strings.Contains(outcome.Content, "validation numeric budget exceeded") {
				t.Fatalf("missing numeric budget failure: %+v", outcome)
			}
		})
	}
}

func TestBoundedNumbersKeepAdjacentIntegersDistinct(t *testing.T) {
	v := validator{}
	even, evenOK := v.parseNumber(json.Number("9007199254740992"))
	odd, oddOK := v.parseNumber(json.Number("9007199254740993"))
	integer, integerOK := v.parseNumber(json.Number("1"))
	decimal, decimalOK := v.parseNumber(json.Number("1.0"))
	if !evenOK || !oddOK || !integerOK || !decimalOK || even.Cmp(odd) >= 0 || integer.Cmp(decimal) != 0 {
		t.Fatal("bounded parsing lost exact number equality")
	}
}
