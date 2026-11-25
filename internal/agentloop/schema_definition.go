package agentloop

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Schemas may hold typed Go maps; validate their JSON form.
type preparedSchema struct {
	value any
	err   error
}

type validationBudget struct {
	steps      int
	bytes      int
	exhaustion string
	schemas    map[string]preparedSchema
}

func (v *validator) schemaValue(name string, parameters any) (any, error) {
	if v.batch != nil {
		if cached, ok := v.batch.schemas[name]; ok {
			return cached.value, cached.err
		}
	}
	value, err := v.decodeSchema(parameters)
	if err == nil {
		v.root = value
		v.inspectSchema(value, "", 0, true, make(map[string]bool))
		if v.exhausted {
			err = fmt.Errorf("%s", v.exhaustion)
		} else if len(v.schemaErrors) != 0 {
			issue := v.schemaErrors[0]
			err = fmt.Errorf("%s: %s", issue.path, issue.message)
		}
	}
	if v.batch != nil {
		if v.batch.schemas == nil {
			v.batch.schemas = make(map[string]preparedSchema)
		}
		v.batch.schemas[name] = preparedSchema{value, err}
	}
	return value, err
}

func (v *validator) decodeSchema(parameters any) (any, error) {
	if parameters == nil {
		return map[string]any{"type": "object"}, nil
	}
	raw, err := json.Marshal(parameters)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxValidationInputBytes {
		v.exhaust("validation schema size budget exceeded")
	}
	if !v.spendBytes(len(raw)) {
		return nil, fmt.Errorf("%s", v.exhaustion)
	}
	return decode(raw)
}

// A batch replay must charge the same prefix before an error or short circuit.
// The unordered collection therefore charges uniform steps and one byte total.
func (v *validator) sortedKeys(object map[string]any) ([]string, bool) {
	keys := make([]string, 0, min(len(object), maxValidationSteps-v.steps))
	keyBytes := 0
	for key := range object {
		if !v.spend() {
			return nil, false
		}
		keys = append(keys, key)
		keyBytes += len(key)
	}
	if !v.spendBytes(keyBytes) {
		return nil, false
	}
	sort.Strings(keys)
	return keys, true
}

// Inspect assertion shapes once per prepared schema, independent of instance
// types and branch matches. Only schema positions (including resolved pointer
// targets) are schemas: enum/const, annotations and extension data are opaque.
// isRoot tracks document location: references can revisit it at nonzero depth.
func (v *validator) inspectSchema(schema any, path string, depth int, isRoot bool, refs map[string]bool) bool {
	if !v.enter(depth) {
		return false
	}
	s, ok := schema.(map[string]any)
	if !ok {
		_, ok = schema.(bool)
		if !ok {
			v.failSchema(path, "schema must be an object or boolean")
		}
		return ok
	}
	keys, ok := v.sortedKeys(s)
	if !ok {
		return false
	}
	for _, key := range keys {
		raw := s[key]
		switch key {
		case "$id":
			if _, ok := raw.(string); !ok {
				v.failSchema(path, "$id must be a string")
			} else if !isRoot {
				v.failSchema(path, "nested $id is unsupported in a single-resource schema")
			}
		case "$ref":
			ref, ok := raw.(string)
			if !ok {
				v.failSchema(path, "$ref must be a string")
			} else if strings.HasPrefix(ref, "#") {
				if !v.spendBytes(len(ref)) {
					return false
				}
				if refs[ref] {
					break
				}
				refs[ref] = true // Shape is instance-independent; recursion is legal.
				if target, found := v.resolve(ref); found && !v.inspectSchema(target, ref, depth+1, ref == "#", refs) {
					return false
				}
			}
		case "type", "required":
			if name, ok := raw.(string); key == "type" && ok {
				v.knownType(name, path)
				break
			}
			names, ok := raw.([]any)
			if key == "type" && len(names) == 0 {
				v.failSchema(path, "type must be a string or non-empty array")
				break
			}
			if !ok {
				v.failSchema(path, "required must be an array of strings")
				break
			}
			if !v.inspectNames(names, key, path) {
				return false
			}
		case "enum":
			if _, ok := raw.([]any); !ok {
				v.failSchema(path, "enum must be an array")
			}
		case "properties", "patternProperties", "$defs", "definitions":
			children, ok := raw.(map[string]any)
			if !ok {
				v.failSchema(path, "%s must be an object", key)
				break
			}
			names, ok := v.sortedKeys(children)
			if !ok {
				return false
			}
			for _, name := range names {
				child := children[name]
				if _, indexed := child.([]any); indexed && (key == "$defs" || key == "definitions") {
					// Legacy pointer tables are data until a specific element is referenced.
					continue
				}
				if key == "patternProperties" && v.compilePattern(path, "patternProperties pattern", name) == nil {
					return false
				}
				if !v.inspectSchema(child, v.child(path, name), depth+1, false, refs) {
					return false
				}
			}
		case "items", "prefixItems", "allOf", "anyOf", "oneOf":
			children, array := raw.([]any)
			if key == "items" && !array {
				if !v.inspectSchema(raw, v.child(path, key), depth+1, false, refs) {
					return false
				}
				break
			}
			if len(children) == 0 {
				v.failSchema(path, "%s must be a non-empty schema array", key)
				break
			}
			if _, prefix := s["prefixItems"]; key == "items" && prefix {
				v.failSchema(path, "tuple items conflicts with prefixItems")
				break
			}
			for i, child := range children {
				if !v.inspectSchema(child, v.child(path, fmt.Sprintf("%s[%d]", key, i)), depth+1, false, refs) {
					return false
				}
			}
		case "additionalProperties", "additionalItems", "propertyNames", "contains", "not", "if", "then", "else":
			if !v.inspectSchema(raw, v.child(path, key), depth+1, false, refs) {
				return false
			}
		case "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties", "minContains", "maxContains":
			if n, ok := v.number(s, key, path); ok && (!n.IsInt() || n.Sign() < 0) {
				v.failSchema(path, "%s must be a non-negative integer", key)
			}
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf":
			if n, ok := v.number(s, key, path); ok && key == "multipleOf" && n.Sign() <= 0 {
				v.failSchema(path, "multipleOf must be positive")
			}
		case "uniqueItems":
			if _, ok := raw.(bool); !ok {
				v.failSchema(path, "uniqueItems must be a boolean")
			}
		case "pattern", "format":
			if text, ok := raw.(string); !ok {
				v.failSchema(path, "%s must be a string", key)
			} else if key == "pattern" && v.compilePattern(path, key, text) == nil {
				return false
			}
		case "dependentRequired", "dependentSchemas", "dependencies", "unevaluatedProperties", "unevaluatedItems",
			"$dynamicRef", "$recursiveRef", "disallow", "extends", "divisibleBy":
			v.failSchema(path, "unsupported schema keyword %s", key)
		}
		if v.exhausted || v.schemaFailures != 0 {
			return false
		}
	}
	return true
}

func (v *validator) inspectNames(names []any, key, path string) bool {
	seen := make(map[string]bool)
	for _, entry := range names {
		if !v.spend() {
			return false
		}
		name, ok := entry.(string)
		if !ok {
			v.failSchema(path, "%s entries must be strings", key)
			return false
		}
		if !v.spendBytes(len(name)) {
			return false
		}
		if seen[name] {
			v.failSchema(path, "%s entries must be unique", key)
			return false
		}
		seen[name] = true
		if key == "type" && !v.knownType(name, path) {
			return false
		}
	}
	return true
}
