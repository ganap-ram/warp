package bench

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// fieldNeedsQuotes reports whether our field must be enclosed in quotes.
// Fields with a Comma, fields with a quote or newline, and
// fields which start with a space must be enclosed in quotes.
func fieldNeedsQuotes(field string) bool {
	const comma = '\t'
	if field == "" {
		return false
	}
	if field == `\.` || strings.ContainsRune(field, comma) || strings.ContainsAny(field, "\"\r\n") {
		return true
	}

	r1, _ := utf8.DecodeRuneInString(field)
	return unicode.IsSpace(r1)
}

func csvEscapeString(field string) string {
	if !fieldNeedsQuotes(field) {
		return field
	}
	var w strings.Builder
	w.WriteByte('"')

	for len(field) > 0 {
		// Search for special characters.
		i := strings.IndexAny(field, "\"\r\n")
		if i < 0 {
			i = len(field)
		}

		// Copy verbatim everything before the special character.
		w.WriteString(field[:i])
		field = field[i:]

		// Encode the special character.
		if len(field) > 0 {
			switch field[0] {
			case '"':
				_, _ = w.WriteString(`""`)
			case '\r':
				_ = w.WriteByte('\r')
			case '\n':
				_ = w.WriteByte('\n')
			}
			field = field[1:]
		}
	}
	w.WriteByte('"')
	return w.String()
}
