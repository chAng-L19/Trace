package agentloop

import (
	"encoding/json"
	"math/big"
	"strings"
)

const (
	maxNumberTokenBytes = 4096
	maxNumberExponent   = 4096
)

func (v *validator) parseNumber(value any) (*big.Rat, bool) {
	token, ok := value.(json.Number)
	if !ok || v.exhausted {
		return nil, false
	}
	if len(token) > maxNumberTokenBytes {
		v.exhaust("validation numeric budget exceeded (token exceeds 4096 bytes)")
		return nil, false
	}
	text := string(token)
	exponent := 0
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		digits := text[index+1:]
		if len(digits) > 0 && (digits[0] == '+' || digits[0] == '-') {
			digits = digits[1:]
		}
		for i := range len(digits) {
			if digits[i] < '0' || digits[i] > '9' {
				v.exhaust("validation numeric budget exceeded (invalid decimal exponent)")
				return nil, false
			}
			exponent = exponent*10 + int(digits[i]-'0')
			if exponent > maxNumberExponent {
				v.exhaust("validation numeric budget exceeded (exponent exceeds 4096)")
				return nil, false
			}
		}
	}
	// Charge every parse, including the decimal expansion, before allocating big integers.
	if !v.spendBytes(len(text) + exponent) {
		return nil, false
	}
	if !json.Valid([]byte(text)) {
		v.exhaust("validation numeric budget exceeded (invalid JSON number)")
		return nil, false
	}
	n, ok := new(big.Rat).SetString(text)
	if !ok {
		v.exhaust("validation numeric budget exceeded (number is not representable)")
		return nil, false
	}
	return n, true
}
