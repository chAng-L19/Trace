package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// JSON.parse drops duplicate fields; encoding/json can merge them into Go
// structs. Reject that ambiguity before executing the journaled original.
func strictPiArguments(raw string) (any, error) {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	var read func() (any, error)
	read = func() (any, error) {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch token {
		case json.Delim('{'):
			object := map[string]any{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return nil, err
				}
				name := key.(string)
				if _, exists := object[name]; exists {
					return nil, fmt.Errorf("duplicate argument field %q", name)
				}
				value, err := read()
				if err != nil {
					return nil, err
				}
				object[name] = value
			}
			_, err = d.Token()
			return object, err
		case json.Delim('['):
			array := []any{}
			for d.More() {
				value, err := read()
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			_, err = d.Token()
			return array, err
		default:
			return token, nil
		}
	}
	value, err := read()
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, fmt.Errorf("arguments must contain one JSON value")
	}
	return value, nil
}

// Builtins decode schema fields into Go structs (case-insensitive). MCP/map
// payload keys stay case-sensitive; do not normalize or rewrite either form.
func checkPiFieldAliases(value, schema any) error {
	s, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	switch v := value.(type) {
	case map[string]any:
		properties, _ := s["properties"].(map[string]any)
		for name, child := range v {
			property, exact := properties[name]
			if !exact {
				for field := range properties {
					if strings.EqualFold(field, name) {
						return fmt.Errorf("argument field %q must use schema spelling %q", name, field)
					}
				}
			}
			if err := checkPiFieldAliases(child, property); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := checkPiFieldAliases(child, s["items"]); err != nil {
				return err
			}
		}
	}
	return nil
}
