package checklistimport

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// ChecklistItem.model_validate(dict) in pydantic's lax (python) mode, for values decoded from
// JSON, with pydantic's error rendering (pydantic 2.13 / pydantic-core, as pinned in
// python/uv.lock). Coercions reproduced: int fields take bools, integral finite floats inside
// the i64 range and numeric strings ("  12 ", "1_000", "+5", "1.00"); the bool field takes
// 0/1, 0.0/1.0 and the words true/false/yes/no/on/off/t/f/y/n/1/0 in any case; str and
// list[str] fields take only str / list.
//
// Known gap: Python ints are unbounded, Go's ChecklistItem ints are 64-bit. An int beyond the
// int64 range (a JSON literal or a numeric string) is reported here as int_parsing_size, where
// pydantic would have accepted it.

// pydanticErrorsURL is the "For further information" link prefix pydantic prints.
const pydanticErrorsURL = "https://errors.pydantic.dev/2.13/v/"

// itemFields is ChecklistItem.model_fields, in declaration (= validation and error) order.
var itemFields = []string{
	"id", "description", "status", "verified_by", "depends_on", "attempts",
	"consecutive_failures", "last_failure", "allow_harness_edits", "witnesses", "notes",
	"schema_version",
}

func isItemField(k string) bool {
	for _, f := range itemFields {
		if f == k {
			return true
		}
	}
	return false
}

type pdError struct {
	loc, msg, typ string
	input         any
}

// truncateRepr is pydantic-core's input_value truncation: a repr longer than 50 BYTES keeps
// its first 25 bytes (backing off to a char boundary) and its last 24 (moving forward to one).
func truncateRepr(s string) string {
	if len(s) <= 50 {
		return s
	}
	head := 25
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - 24
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + "..." + s[tail:]
}

func renderValidationError(title string, errs []pdError) string {
	var b strings.Builder
	plural := "s"
	if len(errs) == 1 {
		plural = ""
	}
	fmt.Fprintf(&b, "%d validation error%s for %s", len(errs), plural, title)
	for _, e := range errs {
		fmt.Fprintf(&b, "\n%s\n  %s [type=%s, input_value=%s, input_type=%s]\n    For further information visit %s%s",
			e.loc, e.msg, e.typ, truncateRepr(pyValueRepr(e.input)), pyTypeName(e.input), pydanticErrorsURL, e.typ)
	}
	return b.String()
}

var intStrRE = regexp.MustCompile(`^[+-]?[0-9]+(?:_[0-9]+)*$`)

const (
	msgIntType    = "Input should be a valid integer"
	msgIntParsing = "Input should be a valid integer, unable to parse string as an integer"
	msgIntSize    = "Unable to parse input string as an integer, exceeded maximum size"
)

func pdInt(v any) (int, *pdError) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case *big.Int:
		if !x.IsInt64() {
			return 0, &pdError{msg: msgIntSize, typ: "int_parsing_size", input: v}
		}
		return int(x.Int64()), nil
	case float64:
		switch {
		case math.IsNaN(x) || math.IsInf(x, 0):
			return 0, &pdError{msg: "Input should be a finite number", typ: "finite_number", input: v}
		case x != math.Trunc(x):
			return 0, &pdError{msg: "Input should be a valid integer, got a number with a fractional part",
				typ: "int_from_float", input: v}
		case x <= -9223372036854775808.0 || x >= 9223372036854775808.0:
			return 0, &pdError{msg: msgIntSize, typ: "int_parsing_size", input: v}
		}
		return int(x), nil
	case string:
		s := strings.TrimSpace(x) // Rust str::trim: Unicode White_Space
		if len(s) > 4300 {
			return 0, &pdError{msg: msgIntSize, typ: "int_parsing_size", input: v}
		}
		if before, after, ok := strings.Cut(s, "."); ok && after != "" && strings.Trim(after, "0") == "" {
			s = before
		}
		if !intStrRE.MatchString(s) {
			return 0, &pdError{msg: msgIntParsing, typ: "int_parsing", input: v}
		}
		n, ok := new(big.Int).SetString(strings.ReplaceAll(strings.TrimPrefix(s, "+"), "_", ""), 10)
		if !ok {
			return 0, &pdError{msg: msgIntParsing, typ: "int_parsing", input: v}
		}
		if !n.IsInt64() {
			return 0, &pdError{msg: msgIntSize, typ: "int_parsing_size", input: v}
		}
		return int(n.Int64()), nil
	}
	return 0, &pdError{msg: msgIntType, typ: "int_type", input: v}
}

func pdBool(v any) (bool, *pdError) {
	typeErr := &pdError{msg: "Input should be a valid boolean", typ: "bool_type", input: v}
	parseErr := &pdError{msg: "Input should be a valid boolean, unable to interpret input", typ: "bool_parsing", input: v}
	switch x := v.(type) {
	case bool:
		return x, nil
	case *big.Int:
		if !x.IsInt64() {
			return false, typeErr
		}
		switch x.Int64() {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
		return false, parseErr
	case float64:
		switch x {
		case 0:
			return false, nil
		case 1:
			return true, nil
		}
		return false, typeErr
	case string:
		switch strings.ToLower(x) {
		case "0", "off", "f", "false", "n", "no":
			return false, nil
		case "1", "on", "t", "true", "y", "yes":
			return true, nil
		}
		return false, parseErr
	}
	return false, typeErr
}

func pdStr(v any) (string, *pdError) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", &pdError{msg: "Input should be a valid string", typ: "string_type", input: v}
}

// validateItem is ChecklistItem.model_validate(fields); the error string is str(ValidationError).
func validateItem(fields *pyDict) (contracts.ChecklistItem, string) {
	item := contracts.NewChecklistItem("", "")
	var errs []pdError
	add := func(loc string, e *pdError) {
		e.loc = loc
		errs = append(errs, *e)
	}
	str := func(name string, dst *string) {
		if v, ok := fields.get(name); ok {
			s, e := pdStr(v)
			if e != nil {
				add(name, e)
				return
			}
			*dst = s
		}
	}
	strList := func(name string, dst *[]string) {
		v, ok := fields.get(name)
		if !ok {
			return
		}
		list, isList := v.([]any)
		if !isList {
			add(name, &pdError{msg: "Input should be a valid list", typ: "list_type", input: v})
			return
		}
		out := make([]string, 0, len(list))
		for i, e := range list {
			s, perr := pdStr(e)
			if perr != nil {
				add(fmt.Sprintf("%s.%d", name, i), perr)
				continue
			}
			out = append(out, s)
		}
		*dst = out
	}
	integer := func(name string, dst *int) {
		if v, ok := fields.get(name); ok {
			n, e := pdInt(v)
			if e != nil {
				add(name, e)
				return
			}
			*dst = n
		}
	}
	for _, name := range itemFields {
		switch name {
		case "id":
			str(name, &item.ID)
		case "description":
			if _, ok := fields.get(name); !ok {
				add(name, &pdError{msg: "Field required", typ: "missing", input: fields})
				continue
			}
			str(name, &item.Description)
		case "status":
			if v, ok := fields.get(name); ok {
				s, isStr := v.(string)
				switch {
				case isStr && (s == contracts.StatusTodo || s == contracts.StatusInProgress ||
					s == contracts.StatusBlocked || s == contracts.StatusDone || s == contracts.StatusSplit):
					item.Status = s
				default:
					add(name, &pdError{msg: "Input should be 'todo', 'in_progress', 'blocked', 'done' or 'split'",
						typ: "literal_error", input: v})
				}
			}
		case "verified_by":
			strList(name, &item.VerifiedBy)
		case "depends_on":
			strList(name, &item.DependsOn)
		case "witnesses":
			strList(name, &item.Witnesses)
		case "attempts":
			integer(name, &item.Attempts)
		case "consecutive_failures":
			integer(name, &item.ConsecutiveFailures)
		case "schema_version":
			integer(name, &item.SchemaVersion)
		case "last_failure":
			str(name, &item.LastFailure)
		case "notes":
			str(name, &item.Notes)
		case "allow_harness_edits":
			if v, ok := fields.get(name); ok {
				b, e := pdBool(v)
				if e != nil {
					add(name, e)
					continue
				}
				item.AllowHarnessEdits = b
			}
		}
	}
	if len(errs) > 0 {
		return contracts.ChecklistItem{}, renderValidationError("ChecklistItem", errs)
	}
	return item, ""
}
