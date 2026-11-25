package agentloop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// validateArguments checks raw call arguments against Trace's fixed JSON Schema subset.
// Trace executes the journaled original arguments, so values are never
// coerced: an argument that would need coercion is a validation failure.
// Error text follows pi-ai validateToolArguments so the model sees the same
// repair guidance. format is annotation-only. References use root-local JSON
// Pointers; remote and dangling references fail where evaluated, never fetched.
func validateArguments(name string, parameters any, arguments string, budget *validationBudget) error {
	v := validator{references: make(map[referenceVisit]bool), batch: budget}
	if len(arguments) > maxValidationInputBytes {
		v.exhaust("validation input size budget exceeded")
	}
	if !v.spendBytes(len(arguments)) {
		return fmt.Errorf("Validation failed for tool %q: %s", diagnosticPreview(name), v.exhaustion)
	}
	value, err := decode([]byte(arguments))
	if err == nil {
		if _, object := value.(map[string]any); !object {
			err = fmt.Errorf("tool arguments must be a JSON object")
		}
	}
	if err != nil {
		return fmt.Errorf("Invalid arguments for tool %q: %s\n\nReceived arguments:\n%s", name, err, diagnosticPreview(arguments))
	}
	schema, err := v.schemaValue(name, parameters)
	if err != nil {
		return fmt.Errorf("Validation failed for tool %q:\n  - root: unusable parameter schema: %s", name, err)
	}
	v.root = schema
	v.check(value, schema, "", "")
	v.errors = append(v.errors, v.schemaErrors...)
	if v.exhausted {
		v.errors = append(v.errors, schemaError{"root", v.exhaustion})
	}
	if len(v.errors) == 0 {
		return nil
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Validation failed for tool %q:\n", name)
	for _, e := range v.errors {
		fmt.Fprintf(&text, "  - %s: %s\n", e.path, e.message)
	}
	fmt.Fprintf(&text, "\nReceived arguments:\n%s", diagnosticPreview(arguments))
	return fmt.Errorf("%s", text.String())
}

func decode(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("arguments must contain one JSON value")
	}
	return value, nil
}

const (
	maxValidationInputBytes = 1 << 20
	maxValidationWorkBytes  = 8 << 20
	maxBatchValidationSteps = 200_000
	maxBatchValidationBytes = 16 << 20
	maxValidationSteps      = 100_000
	maxValidationErrors     = 32
	maxDiagnosticBytes      = 4096
	maxValidationDepth      = 128
	maxValidationPathBytes  = 1 << 20
)

func diagnosticPreview(arguments string) string {
	if len(arguments) <= maxDiagnosticBytes {
		return arguments
	}
	return strings.ToValidUTF8(arguments[:maxDiagnosticBytes], "�") + "\n[truncated]"
}

type schemaError struct{ path, message string }

type referenceVisit struct{ ref, instance string }

type validator struct {
	root           any
	errors         []schemaError
	schemaErrors   []schemaError
	references     map[referenceVisit]bool
	steps          int
	failures       int
	schemaFailures int
	exhausted      bool
	exhaustion     string
	depth          int
	pathBytes      int
	workBytes      int
	batch          *validationBudget
}

func (v *validator) spend() bool {
	if v.batch != nil {
		if v.batch.exhaustion != "" {
			v.exhaust(v.batch.exhaustion)
		}
		if v.batch.steps >= maxBatchValidationSteps {
			v.exhaust("batch validation work budget exceeded")
		}
	}
	if v.exhausted {
		return false
	}
	if v.steps >= maxValidationSteps {
		v.exhaust(fmt.Sprintf("validation work budget exceeded (%d steps)", maxValidationSteps))
		return false
	}
	v.steps++
	if v.batch != nil {
		v.batch.steps++
	}
	return true
}

func (v *validator) spendBytes(size int) bool {
	if v.batch != nil && v.batch.exhaustion != "" {
		v.exhaust(v.batch.exhaustion)
	}
	if v.exhausted {
		return false
	}
	if size > maxValidationWorkBytes-v.workBytes {
		v.exhaust("validation byte work budget exceeded")
		return false
	}
	if v.batch != nil && size > maxBatchValidationBytes-v.batch.bytes {
		v.exhaust("batch validation byte work budget exceeded")
		return false
	}
	v.workBytes += size
	if v.batch != nil {
		v.batch.bytes += size
	}
	return true
}

func (v *validator) diagnostic(path, format string, args []any) schemaError {
	for i, arg := range args {
		switch value := arg.(type) {
		case string:
			if !v.spendBytes(len(value)) {
				return schemaError{"root", v.exhaustion}
			}
			args[i] = diagnosticPreview(value)
		case json.Number:
			if !v.spendBytes(len(value)) {
				return schemaError{"root", v.exhaustion}
			}
			args[i] = diagnosticPreview(string(value))
		case nil, bool, int, int64, float64:
		default:
			args[i] = "invalid constraint"
		}
	}
	return schemaError{diagnosticPreview(path), diagnosticPreview(fmt.Sprintf(format, args...))}
}

func (v *validator) fail(path, format string, args ...any) {
	v.failures++
	// After exhaustion the single budget diagnostic is appended once at the end.
	if v.exhausted || len(v.errors) >= maxValidationErrors {
		return
	}
	if path == "" {
		path = "root"
	}
	v.errors = append(v.errors, v.diagnostic(path, format, args))
}

func (v *validator) failSchema(path, format string, args ...any) {
	v.schemaFailures++
	// After exhaustion the single budget diagnostic is appended once at the end.
	if v.exhausted || len(v.schemaErrors) >= maxValidationErrors {
		return
	}
	if path == "" {
		path = "root"
	}
	v.schemaErrors = append(v.schemaErrors, v.diagnostic(path, format, args))
}

func (v *validator) exhaust(reason string) {
	if !v.exhausted {
		v.exhaustion = reason
	}
	v.exhausted = true
	if v.batch != nil && v.batch.exhaustion == "" {
		v.batch.exhaustion = reason
	}
}

func (v *validator) pathBudget(size int) bool {
	if v.exhausted {
		return false
	}
	if size > maxValidationPathBytes-v.pathBytes {
		v.exhaust(fmt.Sprintf("validation path budget exceeded (%d bytes)", maxValidationPathBytes))
		return false
	}
	v.pathBytes += size
	return true
}

func (v *validator) child(path, key string) string {
	separator := 0
	if path != "" {
		separator = 1
	}
	if !v.pathBudget(len(path) + separator + len(key)) {
		return ""
	}
	if path == "" {
		return key
	}
	return path + "." + key
}

func (v *validator) instanceChild(instance, key string) string {
	size := len(instance) + 1 + len(key) + strings.Count(key, "~") + strings.Count(key, "/")
	if !v.pathBudget(size) {
		return ""
	}
	return instance + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func (v *validator) valid(value, schema any, path, instance string) bool {
	errors, failures, schemaFailures := len(v.errors), v.failures, v.schemaFailures
	v.check(value, schema, path, instance)
	matched := !v.exhausted && v.failures == failures && v.schemaFailures == schemaFailures
	// Probe mismatches are local; malformed schemas must still reject the call.
	v.errors = v.errors[:errors]
	v.failures = failures
	return matched
}

func (v *validator) enter(depth int) bool {
	if !v.spend() {
		return false
	}
	if depth >= maxValidationDepth {
		v.exhaust(fmt.Sprintf("validation nesting budget exceeded (%d levels)", maxValidationDepth))
		return false
	}
	return true
}

func (v *validator) check(value, schema any, path, instance string) {
	if !v.enter(v.depth) {
		return
	}
	v.depth++
	defer func() { v.depth-- }()
	switch s := schema.(type) {
	case bool:
		if !s {
			v.fail(path, "must not be present")
		}
		return
	case map[string]any:
		v.object(value, s, path, instance)
	default:
		v.failSchema(path, "schema must be an object or boolean")
	}
}

func (v *validator) object(value any, s map[string]any, path, instance string) {
	if ref, ok := s["$ref"].(string); ok {
		target, found := v.resolve(ref)
		if !found {
			v.failSchema(path, "unresolved schema reference %s", ref)
			return
		}
		visit := referenceVisit{ref, instance}
		if v.references[visit] {
			v.failSchema(path, "cyclic schema reference %s", ref)
			return
		}
		v.references[visit] = true
		v.check(value, target, path, instance)
		delete(v.references, visit)
	}
	if v.exhausted {
		return
	}
	if t, ok := s["type"]; ok && !v.matchesType(value, t) {
		if v.exhausted {
			return
		}
		v.fail(path, "must be %s", v.typeNames(t))
		return
	}
	if allowed, ok := s["enum"].([]any); ok {
		found := false
		for _, candidate := range allowed {
			if !v.spend() {
				return
			}
			found = found || v.equal(value, candidate)
		}
		if !found {
			v.fail(path, "must be equal to one of the allowed values")
		}
	}
	if constant, ok := s["const"]; ok && !v.equal(value, constant) {
		v.fail(path, "must be equal to constant")
	}
	for _, sub := range list(s["allOf"]) {
		if v.exhausted {
			return
		}
		v.check(value, sub, path, instance)
	}
	if subs := list(s["anyOf"]); len(subs) > 0 {
		matched := false
		for _, sub := range subs {
			if v.exhausted {
				return
			}
			if v.valid(value, sub, path, instance) {
				matched = true
			}
		}
		if !matched {
			v.fail(path, "must match a schema in anyOf")
		}
	}
	if subs := list(s["oneOf"]); len(subs) > 0 {
		matched := 0
		for _, sub := range subs {
			if v.exhausted {
				return
			}
			if v.valid(value, sub, path, instance) {
				matched++
			}
		}
		if matched != 1 {
			v.fail(path, "must match exactly one schema in oneOf")
		}
	}
	if not, ok := s["not"]; ok && v.valid(value, not, path, instance) {
		v.fail(path, "must not be valid")
	}
	if condition, ok := s["if"]; ok {
		if v.valid(value, condition, path, instance) {
			if then, ok := s["then"]; ok {
				v.check(value, then, path, instance)
			}
		} else if otherwise, ok := s["else"]; ok {
			v.check(value, otherwise, path, instance)
		}
	}
	if v.exhausted {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		v.properties(typed, s, path, instance)
	case []any:
		v.items(typed, s, path, instance)
	case string:
		length := new(big.Rat)
		_, minimum := s["minLength"]
		_, maximum := s["maxLength"]
		if minimum || maximum {
			if !v.spendBytes(len(typed)) {
				return
			}
			length.SetInt64(int64(utf8.RuneCountInString(typed)))
		}
		if limit, ok := v.number(s, "minLength", path); ok && length.Cmp(limit) < 0 {
			v.fail(path, "must NOT have fewer than %v characters", s["minLength"])
		}
		if limit, ok := v.number(s, "maxLength", path); ok && length.Cmp(limit) > 0 {
			v.fail(path, "must NOT have more than %v characters", s["maxLength"])
		}
		if pattern, ok := s["pattern"].(string); ok {
			re := v.compilePattern(path, "pattern", pattern)
			if re == nil || !v.matchCost(pattern, typed) {
				return
			} else if !re.MatchString(typed) {
				v.fail(path, "must match pattern %q", pattern)
			}
		}
	case json.Number:
		n, ok := v.parseNumber(typed)
		if !ok {
			v.fail(path, "unsupported JSON number %s", typed)
			return
		}
		if limit, ok := v.number(s, "minimum", path); ok && n.Cmp(limit) < 0 {
			v.fail(path, "must be >= %v", s["minimum"])
		}
		if limit, ok := v.number(s, "maximum", path); ok && n.Cmp(limit) > 0 {
			v.fail(path, "must be <= %v", s["maximum"])
		}
		if limit, ok := v.number(s, "exclusiveMinimum", path); ok && n.Cmp(limit) <= 0 {
			v.fail(path, "must be > %v", s["exclusiveMinimum"])
		}
		if limit, ok := v.number(s, "exclusiveMaximum", path); ok && n.Cmp(limit) >= 0 {
			v.fail(path, "must be < %v", s["exclusiveMaximum"])
		}
		if step, ok := v.number(s, "multipleOf", path); ok {
			if step.Sign() <= 0 {
				v.failSchema(path, "multipleOf must be positive")
			} else if !new(big.Rat).Quo(n, step).IsInt() {
				v.fail(path, "must be multiple of %v", s["multipleOf"])
			}
		}
	}
}

func (v *validator) compilePattern(path, kind, pattern string) *regexp.Regexp {
	if !v.spendBytes(len(pattern)) {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		v.failSchema(path, "invalid %s %q: %s", kind, pattern, err.Error())
	}
	return re
}

func (v *validator) matchCost(pattern, value string) bool {
	if len(pattern)+1 > maxValidationWorkBytes/(len(value)+1) {
		v.exhaust("validation regex byte work budget exceeded")
		return false
	}
	return v.spendBytes((len(pattern) + 1) * (len(value) + 1))
}

func (v *validator) properties(value map[string]any, s map[string]any, path, instance string) {
	for _, name := range list(s["required"]) {
		if !v.spend() {
			return
		}
		if key, ok := name.(string); ok {
			if _, present := value[key]; !present {
				v.fail(path, "must have required property '%s'", key)
			}
		}
	}
	if limit, ok := v.number(s, "minProperties", path); ok && new(big.Rat).SetInt64(int64(len(value))).Cmp(limit) < 0 {
		v.fail(path, "must NOT have fewer than %v properties", s["minProperties"])
	}
	if limit, ok := v.number(s, "maxProperties", path); ok && new(big.Rat).SetInt64(int64(len(value))).Cmp(limit) > 0 {
		v.fail(path, "must NOT have more than %v properties", s["maxProperties"])
	}
	properties, _ := s["properties"].(map[string]any)
	patterns, _ := s["patternProperties"].(map[string]any)
	patternNames, ok := v.sortedKeys(patterns)
	if !ok {
		return
	}
	compiledPatterns := make([]*regexp.Regexp, len(patternNames))
	for i, pattern := range patternNames {
		compiledPatterns[i] = v.compilePattern(path, "patternProperties pattern", pattern)
		if compiledPatterns[i] == nil {
			return
		}
	}
	keys, ok := v.sortedKeys(value)
	if !ok {
		return
	}
	for _, key := range keys {
		if !v.spend() {
			return
		}
		if names, ok := s["propertyNames"]; ok {
			v.check(key, names, v.child(path, key), v.instanceChild(instance, key))
		}
		matched := false
		if property, ok := properties[key]; ok {
			matched = true
			v.check(value[key], property, v.child(path, key), v.instanceChild(instance, key))
		}
		for i, pattern := range patternNames {
			if !v.spend() {
				return
			}
			if !v.matchCost(pattern, key) {
				return
			}
			if compiledPatterns[i].MatchString(key) {
				matched = true
				v.check(value[key], patterns[pattern], v.child(path, key), v.instanceChild(instance, key))
			}
		}
		if matched {
			continue
		}
		if additional, ok := s["additionalProperties"]; ok {
			if allowed, isBool := additional.(bool); isBool && !allowed {
				v.fail(path, "must NOT have additional property '%s'", key)
			} else {
				v.check(value[key], additional, v.child(path, key), v.instanceChild(instance, key))
			}
		}
	}
}

func (v *validator) items(value []any, s map[string]any, path, instance string) {
	if limit, ok := v.number(s, "minItems", path); ok && new(big.Rat).SetInt64(int64(len(value))).Cmp(limit) < 0 {
		v.fail(path, "must NOT have fewer than %v items", s["minItems"])
	}
	if limit, ok := v.number(s, "maxItems", path); ok && new(big.Rat).SetInt64(int64(len(value))).Cmp(limit) > 0 {
		v.fail(path, "must NOT have more than %v items", s["maxItems"])
	}
	if unique, _ := s["uniqueItems"].(bool); unique {
	unique:
		for i := range value {
			for j := i + 1; j < len(value); j++ {
				if v.equal(value[i], value[j]) {
					v.fail(path, "must NOT have duplicate items (items ## %d and %d are identical)", j, i)
					break unique
				}
				if v.exhausted {
					return
				}
			}
		}
	}
	prefix := list(s["prefixItems"])
	if tuple, ok := s["items"].([]any); ok {
		prefix = tuple
	}
	for i, item := range value {
		if !v.spend() {
			return
		}
		if i < len(prefix) {
			v.check(item, prefix[i], v.child(path, fmt.Sprint(i)), v.instanceChild(instance, fmt.Sprint(i)))
		} else if items, ok := s["items"]; ok {
			if _, tuple := items.([]any); !tuple {
				v.check(item, items, v.child(path, fmt.Sprint(i)), v.instanceChild(instance, fmt.Sprint(i)))
			} else if additional, ok := s["additionalItems"]; ok {
				// Draft-07 tuple form: additionalItems governs elements past the tuple.
				if allowed, isBool := additional.(bool); isBool && !allowed {
					v.fail(path, "must NOT have more than %d items", len(prefix))
					return
				}
				v.check(item, additional, v.child(path, fmt.Sprint(i)), v.instanceChild(instance, fmt.Sprint(i)))
			}
		}
	}
	if contains, ok := s["contains"]; ok {
		matches := 0
		for i, item := range value {
			if !v.spend() {
				return
			}
			if v.valid(item, contains, path, v.instanceChild(instance, fmt.Sprint(i))) {
				matches++
			}
		}
		count := new(big.Rat).SetInt64(int64(matches))
		if limit, ok := v.number(s, "minContains", path); ok {
			if count.Cmp(limit) < 0 {
				v.fail(path, "must contain at least %v valid items", s["minContains"])
			}
		} else if matches == 0 {
			v.fail(path, "must contain at least 1 valid item")
		}
		if limit, ok := v.number(s, "maxContains", path); ok && count.Cmp(limit) > 0 {
			v.fail(path, "must contain at most %v valid items", s["maxContains"])
		}
	}
}

// resolve supports local JSON Pointer URI fragments ("#", "#/$defs/x").
func (v *validator) resolve(ref string) (any, bool) {
	if !v.spendBytes(len(ref)) {
		return nil, false
	}
	if !strings.HasPrefix(ref, "#") {
		return nil, false
	}
	fragment := strings.TrimPrefix(ref, "#")
	// RFC 3986 fragment syntax is checked before decoding; escaped delimiters are data.
	for i := 0; i < len(fragment); i++ {
		c := fragment[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~!$&'()*+,;=:@/?%", rune(c)) {
			continue
		}
		return nil, false
	}
	pointer, err := url.PathUnescape(fragment)
	if err != nil || !utf8.ValidString(pointer) || pointer != "" && !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	current := v.root
	if pointer == "" {
		return current, true
	}
	for _, segment := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		if !v.spend() {
			return nil, false
		}
		for i := 0; i < len(segment); i++ {
			if segment[i] == '~' {
				i++
				if i == len(segment) || segment[i] != '0' && segment[i] != '1' {
					return nil, false
				}
			}
		}
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[segment]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			// RFC 6901: "0" or digits without a leading zero; no sign or spaces.
			if segment == "" || len(segment) > 1 && segment[0] == '0' || strings.Trim(segment, "0123456789") != "" {
				return nil, false
			}
			index, err := strconv.Atoi(segment)
			if err != nil || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func (v *validator) matchesType(value, t any) bool {
	if name, ok := t.(string); ok {
		return v.isType(value, name)
	}
	for _, entry := range list(t) {
		if !v.spend() {
			return false
		}
		if name, ok := entry.(string); ok && v.isType(value, name) {
			return true
		}
	}
	return false
}

// knownType rejects misspelled types (e.g. "int") instead of allowing everything.
func (v *validator) knownType(name, path string) bool {
	switch name {
	case "object", "array", "string", "boolean", "null", "number", "integer":
		return true
	}
	v.failSchema(path, "unknown schema type %q", name)
	return false
}

func (v *validator) isType(value any, name string) bool {
	switch name {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		n, ok := v.parseNumber(value)
		return ok && n.IsInt()
	}
	return false
}

func (v *validator) typeNames(t any) string {
	if name, ok := t.(string); ok {
		if !v.spendBytes(len(name)) {
			return v.exhaustion
		}
		return diagnosticPreview(name)
	}
	if types, ok := t.([]any); ok {
		names := make([]string, 0, min(len(types), maxValidationErrors))
		for _, entry := range types {
			if len(names) >= maxValidationErrors {
				break
			}
			name, ok := entry.(string)
			if !ok {
				return "valid schema type"
			}
			if !v.spendBytes(len(name)) {
				return v.exhaustion
			}
			names = append(names, diagnosticPreview(name))
		}
		return strings.Join(names, ",")
	}
	return "valid schema type"
}

func list(value any) []any {
	items, _ := value.([]any)
	return items
}

func (v *validator) number(s map[string]any, keyword, path string) (*big.Rat, bool) {
	raw, present := s[keyword]
	if !present {
		return nil, false
	}
	n, ok := v.parseNumber(raw)
	if !ok {
		v.failSchema(path, "unusable %s constraint", keyword)
	}
	return n, ok
}

// equal compares decoded JSON values; numbers compare by value (1 == 1.0).
func (v *validator) equal(a, b any) bool {
	if !v.spend() {
		return false
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && len(x) == len(y) && v.spendBytes(len(x)) && x == y
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		nx, okX := v.parseNumber(x)
		ny, okY := v.parseNumber(y)
		return okX && okY && nx.Cmp(ny) == 0
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		keys, ok := v.sortedKeys(x)
		if !ok {
			return false
		}
		for _, key := range keys {
			other, present := y[key]
			if !present || !v.equal(x[key], other) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !v.equal(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}
