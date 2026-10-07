package store

import (
	"strings"
	"unicode"
)

// stopwords are language keywords and English filler that match everything.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		a an and are as at be by do does for from how i in is it of on or the to we what where which who why with
		func function fn def return if else elif for while let const var nil null none true false self this
		import package from export default new class struct type interface pub impl mut async await err
		int string bool str void public private static`) {
		stopwords[w] = true
	}
}

// Tokenize splits text into lowercase search terms. Identifiers are kept
// whole and also split on camelCase, snake_case and digits, so
// "requeueWithDelay" yields requeuewithdelay, requeue, delay.
func Tokenize(s string) []string {
	var out []string
	emit := func(t string) {
		t = strings.ToLower(t)
		if len(t) < 2 || len(t) > 64 || stopwords[t] {
			return
		}
		out = append(out, t)
	}
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	for _, word := range strings.FieldsFunc(s, func(r rune) bool { return !isWord(r) }) {
		parts := splitIdent(word)
		if len(parts) > 1 {
			emit(strings.ReplaceAll(word, "_", ""))
		}
		for _, p := range parts {
			emit(p)
		}
	}
	return out
}

func splitIdent(w string) []string {
	var parts []string
	rs := []rune(w)
	start := 0
	flush := func(end int) {
		if end > start {
			parts = append(parts, string(rs[start:end]))
		}
		start = end
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '_' {
			flush(i)
			start = i + 1
			continue
		}
		if i == start {
			continue
		}
		prev := rs[i-1]
		switch {
		case unicode.IsUpper(r) && unicode.IsLower(prev): // fooBar
			flush(i)
		case unicode.IsUpper(r) && i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(prev): // HTTPServer
			flush(i)
		case unicode.IsDigit(r) != unicode.IsDigit(prev) && prev != '_': // sha256 -> sha 256
			flush(i)
		}
	}
	flush(len(rs))
	return parts
}
