package main

import (
	"errors"
	"strings"
)

// shlexSplit is Python's shlex.split(s) (POSIX mode, no comments): the --check values are parsed
// exactly as the Python CLI parses them, including its error messages.
func shlexSplit(s string) ([]string, error) {
	const whitespace = " \t\r\n"
	var (
		tokens   []string
		token    strings.Builder
		state    = ' ' // ' ' between tokens, 'a' in a word, '\'' or '"' in quotes, '\\' escaping
		escaped  rune  // the state to return to after an escape
		quoted   bool
		inWord   bool
		runes    = []rune(s)
		finished = func() {
			if inWord && (quoted || token.Len() > 0) {
				tokens = append(tokens, token.String())
			}
			token.Reset()
			quoted, inWord = false, false
		}
	)
	for _, c := range runes {
		switch state {
		case ' ':
			switch {
			case strings.ContainsRune(whitespace, c):
			case c == '\\':
				inWord, escaped, state = true, 'a', '\\'
			case c == '\'' || c == '"':
				inWord, quoted, state = true, true, c
			default:
				inWord, state = true, 'a'
				token.WriteRune(c)
			}
		case 'a':
			switch {
			case strings.ContainsRune(whitespace, c):
				finished()
				state = ' '
			case c == '\'' || c == '"':
				quoted, state = true, c
			case c == '\\':
				escaped, state = 'a', '\\'
			default:
				token.WriteRune(c)
			}
		case '\'', '"':
			switch {
			case c == state:
				state = 'a'
			case c == '\\' && state == '"':
				escaped, state = state, '\\'
			default:
				token.WriteRune(c)
			}
		case '\\':
			if (escaped == '\'' || escaped == '"') && c != escaped && c != '\\' {
				token.WriteRune('\\')
			}
			token.WriteRune(c)
			state = escaped
		}
	}
	switch state {
	case '\'', '"':
		return nil, errors.New("No closing quotation")
	case '\\':
		return nil, errors.New("No escaped character")
	}
	finished()
	if tokens == nil {
		tokens = []string{}
	}
	return tokens, nil
}
