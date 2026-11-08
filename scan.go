package cel2yaml

import (
	"fmt"
	"strings"
)

// piece: a literal text, or the source of an expression.
type piece struct {
	text string
	expr bool
}

// scan: the ${{ }} expressions of a string (brace depth and quotes tracked,
// so a CEL map literal may contain "}}").
func scan(s string) ([]piece, error) {
	var out []piece
	var lit strings.Builder
	for i := 0; i < len(s); {
		// $${{ is a literal ${{.
		if strings.HasPrefix(s[i:], "$${{") {
			lit.WriteString("${{")
			i += 4
			continue
		}
		if !strings.HasPrefix(s[i:], "${{") {
			lit.WriteByte(s[i])
			i++
			continue
		}
		if lit.Len() > 0 {
			out = append(out, piece{text: lit.String()})
			lit.Reset()
		}
		j := closing(s, i+3)
		if j < 0 {
			return nil, fmt.Errorf("unterminated ${{ in %q", s)
		}
		out = append(out, piece{text: s[i+3 : j], expr: true})
		i = j + 2
	}
	if lit.Len() > 0 {
		out = append(out, piece{text: lit.String()})
	}
	return out, nil
}

// closing: the index of the }} that closes an expression starting at from,
// or -1. It skips nested braces, and quoted strings ('...', "...", a \
// escaping the next character), a "}}" in them included.
func closing(s string, from int) int {
	depth := 0
	for j := from; j < len(s); j++ {
		switch c := s[j]; {
		case c == '\'' || c == '"':
			for j++; j < len(s) && s[j] != c; j++ {
				if s[j] == '\\' {
					j++
				}
			}
		case c == '{':
			depth++
		case c == '}' && depth == 0 && strings.HasPrefix(s[j:], "}}"):
			return j
		case c == '}':
			depth--
		}
	}
	return -1
}
