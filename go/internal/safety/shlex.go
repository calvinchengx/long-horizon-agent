package safety

import (
	"errors"
	"strings"
)

// shellLex tokenizes script exactly like CPython's
//
//	lexer = shlex.shlex(script, posix=True, punctuation_chars=";&|()\n")
//	lexer.whitespace = " \t\r"
//	lexer.whitespace_split = True
//	tokens = list(lexer)
//
// It is a transliteration of shlex.read_token for that configuration (commenters "#", quotes
// "'\"", escape "\\", escapedquotes "\""), including its quirks: a "#" anywhere outside quotes
// starts a comment that also swallows the terminating newline, runs of punctuation characters form
// one token ("&&", ";;", "|&"...), and an empty quoted string yields an empty token. The two
// ValueError cases ("No closing quotation", "No escaped character") are returned as errors.
func shellLex(script string) ([]string, error) {
	l := &lexer{in: []rune(script), state: ' '}
	var tokens []string
	for {
		tok, ok, err := l.readToken()
		if err != nil {
			return nil, err
		}
		if !ok {
			return tokens, nil
		}
		tokens = append(tokens, tok)
	}
}

var (
	errNoClosingQuotation = errors.New("No closing quotation")
	errNoEscapedCharacter = errors.New("No escaped character")
)

const (
	lexWhitespace  = " \t\r"
	lexPunctuation = ";&|()\n"
	lexQuotes      = "'\""
	stateEOF       = rune(-1)
)

type lexer struct {
	in    []rune
	pos   int
	state rune // ' ', 'a', 'c', a quote character, '\\', or stateEOF
}

func (l *lexer) next() (rune, bool) {
	if l.pos >= len(l.in) {
		return 0, false
	}
	r := l.in[l.pos]
	l.pos++
	return r, true
}

// unread is shlex's _pushback_chars.append(nextchar): the next read returns the same character.
func (l *lexer) unread() { l.pos-- }

// readLine is instream.readline(): consume up to and including the next newline.
func (l *lexer) readLine() {
	for l.pos < len(l.in) {
		r := l.in[l.pos]
		l.pos++
		if r == '\n' {
			return
		}
	}
}

func in(set string, r rune) bool { return strings.ContainsRune(set, r) }

// readToken returns (token, true) or ("", false) at end of input (shlex's eof sentinel None).
func (l *lexer) readToken() (string, bool, error) {
	quoted := false
	escapedState := ' '
	var token strings.Builder
	for {
		ch, have := l.next()
		switch {
		case l.state == stateEOF:
			token.Reset()
		case l.state == ' ':
			switch {
			case !have:
				l.state = stateEOF
			case in(lexWhitespace, ch):
				if token.Len() > 0 || quoted {
					goto emit
				}
				continue
			case ch == '#':
				l.readLine()
				continue
			case ch == '\\':
				escapedState = 'a'
				l.state = ch
				continue
			case in(lexPunctuation, ch):
				token.WriteRune(ch)
				l.state = 'c'
				continue
			case in(lexQuotes, ch):
				l.state = ch
				continue
			default: // wordchars, or anything else under whitespace_split
				token.WriteRune(ch)
				l.state = 'a'
				continue
			}
		case in(lexQuotes, l.state):
			quoted = true
			if !have {
				return "", false, errNoClosingQuotation
			}
			switch {
			case ch == l.state:
				l.state = 'a'
			case ch == '\\' && l.state == '"':
				escapedState = l.state
				l.state = ch
			default:
				token.WriteRune(ch)
			}
			continue
		case l.state == '\\':
			if !have {
				return "", false, errNoEscapedCharacter
			}
			// In posix shells, only the quote itself or the escape character may be escaped
			// within quotes.
			if in(lexQuotes, escapedState) && ch != l.state && ch != escapedState {
				token.WriteRune(l.state)
			}
			token.WriteRune(ch)
			l.state = escapedState
			continue
		default: // 'a' or 'c'
			switch {
			case !have:
				l.state = stateEOF
			case in(lexWhitespace, ch):
				l.state = ' '
				if token.Len() > 0 || quoted {
					goto emit
				}
				continue
			case ch == '#':
				l.readLine()
				l.state = ' '
				if token.Len() > 0 || quoted {
					goto emit
				}
				continue
			case l.state == 'c':
				if in(lexPunctuation, ch) {
					token.WriteRune(ch)
					continue
				}
				if !in(lexWhitespace, ch) {
					l.unread()
				}
				l.state = ' '
			case in(lexQuotes, ch):
				l.state = ch
				continue
			case ch == '\\':
				escapedState = 'a'
				l.state = ch
				continue
			case !in(lexPunctuation, ch): // wordchars, quotes, or whitespace_split
				token.WriteRune(ch)
				continue
			default:
				l.unread()
				l.state = ' '
				if token.Len() > 0 || quoted {
					goto emit
				}
				continue
			}
		}
		break
	}
emit:
	result := token.String()
	if !quoted && result == "" {
		return "", false, nil
	}
	return result, true, nil
}
