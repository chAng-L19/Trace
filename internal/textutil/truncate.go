package textutil

import "unicode/utf8"

// TruncateUTF8 returns a prefix of at most limit bytes without splitting a
// UTF-8 encoding. Callers supply a nonnegative limit; no suffix is added.
func TruncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
