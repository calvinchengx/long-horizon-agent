package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// decodePyJSON is json.loads: numbers keep their int/float distinction (json.Number), and a
// document Python rejects fails with Python's JSONDecodeError text ("Expecting value: line 1
// column 1 (char 0)").
func decodePyJSON(text string) (any, error) {
	if err := pyJSONError(text); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err // NaN / Infinity: Python accepts them, encoding/json does not
	}
	return v, nil
}

// pyJSONMsg is JSONDecodeError.msg (the message without the position).
func pyJSONMsg(text string, fallback error) string {
	err := pyJSONError(text)
	if err == nil {
		err = fallback
	}
	msg, _, _ := strings.Cut(err.Error(), ": line ")
	return msg
}

// pyJSONError validates text the way CPython's json module scans it and returns the
// JSONDecodeError Python would raise (nil when Python accepts the document).
func pyJSONError(text string) error {
	p := &jsonScanner{s: []rune(text)}
	end, err := p.value(p.ws(0))
	if err != nil {
		return err
	}
	if end = p.ws(end); end != len(p.s) {
		return p.fail("Extra data", end)
	}
	return nil
}

type jsonScanner struct{ s []rune }

type jsonDecodeError struct {
	msg       string
	pos       int
	line, col int
}

func (e *jsonDecodeError) Error() string {
	return fmt.Sprintf("%s: line %d column %d (char %d)", e.msg, e.line, e.col, e.pos)
}

func (e *jsonDecodeError) PyTypeName() string { return "JSONDecodeError" }

func (p *jsonScanner) fail(msg string, pos int) error {
	line, last := 1, -1
	for i := 0; i < pos && i < len(p.s); i++ {
		if p.s[i] == '\n' {
			line++
			last = i
		}
	}
	return &jsonDecodeError{msg: msg, pos: pos, line: line, col: pos - last}
}

func (p *jsonScanner) ws(i int) int {
	for i < len(p.s) && (p.s[i] == ' ' || p.s[i] == '\t' || p.s[i] == '\n' || p.s[i] == '\r') {
		i++
	}
	return i
}

func (p *jsonScanner) at(i int) rune {
	if i < len(p.s) {
		return p.s[i]
	}
	return 0
}

func (p *jsonScanner) literal(i int, word string) bool {
	w := []rune(word)
	if i+len(w) > len(p.s) {
		return false
	}
	for j, r := range w {
		if p.s[i+j] != r {
			return false
		}
	}
	return true
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// value is scan_once: it returns the end of the value at i or "Expecting value".
func (p *jsonScanner) value(i int) (int, error) {
	if i >= len(p.s) {
		return 0, p.fail("Expecting value", i)
	}
	switch c := p.s[i]; {
	case c == '"':
		return p.str(i + 1)
	case c == '{':
		return p.object(i + 1)
	case c == '[':
		return p.array(i + 1)
	case c == 'n' && p.literal(i, "null"):
		return i + 4, nil
	case c == 't' && p.literal(i, "true"):
		return i + 4, nil
	case c == 'f' && p.literal(i, "false"):
		return i + 5, nil
	case c == 'N' && p.literal(i, "NaN"):
		return i + 3, nil
	case c == 'I' && p.literal(i, "Infinity"):
		return i + 8, nil
	case c == '-' && p.literal(i, "-Infinity"):
		return i + 9, nil
	}
	// -?(?:0|[1-9]\d*)(\.\d+)?([eE][-+]?\d+)?
	j := i
	if p.at(j) == '-' {
		j++
	}
	switch {
	case p.at(j) == '0':
		j++
	case p.at(j) >= '1' && p.at(j) <= '9':
		for isDigit(p.at(j)) {
			j++
		}
	default:
		return 0, p.fail("Expecting value", i)
	}
	if p.at(j) == '.' && isDigit(p.at(j+1)) {
		j += 2
		for isDigit(p.at(j)) {
			j++
		}
	}
	if e := p.at(j); e == 'e' || e == 'E' {
		k := j + 1
		if p.at(k) == '+' || p.at(k) == '-' {
			k++
		}
		if isDigit(p.at(k)) {
			for isDigit(p.at(k)) {
				k++
			}
			j = k
		}
	}
	return j, nil
}

// str is scanstring: end is the index after the opening quote.
func (p *jsonScanner) str(end int) (int, error) {
	begin := end - 1
	for {
		for end < len(p.s) && p.s[end] != '"' && p.s[end] != '\\' && p.s[end] >= 0x20 {
			end++
		}
		if end >= len(p.s) {
			return 0, p.fail("Unterminated string starting at", begin)
		}
		switch t := p.s[end]; {
		case t == '"':
			return end + 1, nil
		case t != '\\':
			return 0, p.fail("Invalid control character at", end) // the C scanner's text
		}
		if end+1 >= len(p.s) {
			return 0, p.fail("Unterminated string starting at", begin)
		}
		esc := p.s[end+1]
		if esc != 'u' {
			if !strings.ContainsRune(`"\/bfnrt`, esc) {
				return 0, p.fail("Invalid \\escape", end)
			}
			end += 2
			continue
		}
		pos := end + 1
		ok := pos+5 <= len(p.s)
		for k := pos + 1; ok && k < pos+5; k++ {
			r := p.s[k]
			ok = isDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		}
		if !ok {
			return 0, p.fail("Invalid \\uXXXX escape", pos)
		}
		end = pos + 5
	}
}

func (p *jsonScanner) object(end int) (int, error) {
	end = p.ws(end)
	if p.at(end) != '"' {
		if p.at(end) == '}' && end < len(p.s) {
			return end + 1, nil
		}
		return 0, p.fail("Expecting property name enclosed in double quotes", end)
	}
	end++
	for {
		var err error
		if end, err = p.str(end); err != nil {
			return 0, err
		}
		end = p.ws(end)
		if p.at(end) != ':' || end >= len(p.s) {
			return 0, p.fail("Expecting ':' delimiter", end)
		}
		end = p.ws(end + 1)
		if end, err = p.value(end); err != nil {
			return 0, err
		}
		end = p.ws(end)
		next := p.at(end)
		if end >= len(p.s) {
			next = 0
		}
		end++
		if next == '}' {
			return end, nil
		}
		if next != ',' {
			return 0, p.fail("Expecting ',' delimiter", end-1)
		}
		end = p.ws(end)
		next = p.at(end)
		if end >= len(p.s) {
			next = 0
		}
		end++
		if next != '"' {
			return 0, p.fail("Expecting property name enclosed in double quotes", end-1)
		}
	}
}

func (p *jsonScanner) array(end int) (int, error) {
	end = p.ws(end)
	if end < len(p.s) && p.s[end] == ']' {
		return end + 1, nil
	}
	for {
		var err error
		if end, err = p.value(end); err != nil {
			return 0, err
		}
		end = p.ws(end)
		next := p.at(end)
		if end >= len(p.s) {
			next = 0
		}
		end++
		if next == ']' {
			return end, nil
		}
		if next != ',' {
			return 0, p.fail("Expecting ',' delimiter", end-1)
		}
		end = p.ws(end)
	}
}
