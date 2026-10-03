package obs

// The contract for trace events (python: lha.obs.event_schema): every kind a run records and the
// fields its payload carries, as JSON Schemas in spec/obs/mission_events.json.
//
// LHA_TRACE_AUDIT_DIR makes every TraceRecorder also append each event it records to
// <dir>/go-<pid>.ndjson; Audit then checks a directory of such files against the schemas. The
// test suites use it to prove that every event they record matches its kind's schema and that
// every kind is recorded at least once. Kinds starting with "test_" are test fixtures.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// AuditDirEnv names a directory to append every recorded event to.
const AuditDirEnv = "LHA_TRACE_AUDIT_DIR"

// TestKindPrefix marks test-fixture kinds, which Audit skips.
const TestKindPrefix = "test_"

var (
	auditOnce sync.Once
	auditPath string
	auditMu   sync.Mutex
)

// appendAudit appends event to this process's audit file when LHA_TRACE_AUDIT_DIR is set.
func appendAudit(event TraceEvent, logger interface{ Warn(string, ...any) }) {
	auditOnce.Do(func() {
		if dir := os.Getenv(AuditDirEnv); dir != "" {
			auditPath = filepath.Join(dir, fmt.Sprintf("go-%d.ndjson", os.Getpid()))
		}
	})
	if auditPath == "" {
		return
	}
	line, err := json.Marshal(event)
	if err == nil {
		auditMu.Lock()
		var f *os.File
		if f, err = os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, err = f.Write(append(line, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		auditMu.Unlock()
	}
	if err != nil {
		logger.Warn("trace_audit_failed", "error", err.Error())
	}
}

// SchemaErrors reports where value (decoded JSON, numbers as json.Number) breaks schema; empty
// when it conforms. The schema subset: type, enum, properties, required, additionalProperties,
// items. A JSON integer is a number written without a fraction or exponent.
func SchemaErrors(schema map[string]any, value any, path string) []string {
	if kinds, ok := schema["type"]; ok {
		allowed := stringList(kinds)
		match := false
		for _, k := range allowed {
			if typeOK(k, value) {
				match = true
				break
			}
		}
		if !match {
			return []string{fmt.Sprintf("%s: expected %s, got %s", path, strings.Join(allowed, " or "), jsonType(value))}
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(value) && jsonType(e) == jsonType(value) {
				found = true
				break
			}
		}
		if !found {
			return []string{fmt.Sprintf("%s: %s is not one of %v", path, quote(value), enum)}
		}
	}
	var errs []string
	switch v := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for _, name := range stringList(schema["required"]) {
			if _, ok := v[name]; !ok {
				errs = append(errs, fmt.Sprintf("%s: missing %q", path, name))
			}
		}
		names := make([]string, 0, len(v))
		for name := range v {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if sub, ok := props[name].(map[string]any); ok {
				errs = append(errs, SchemaErrors(sub, v[name], path+"."+name)...)
				continue
			}
			switch extra := schema["additionalProperties"].(type) {
			case bool:
				if !extra {
					errs = append(errs, fmt.Sprintf("%s: unexpected %q", path, name))
				}
			case map[string]any:
				errs = append(errs, SchemaErrors(extra, v[name], path+"."+name)...)
			}
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range v {
				errs = append(errs, SchemaErrors(items, item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}
	return errs
}

// EventErrors reports where a recorded event's payload breaks its kind's schema (an unknown kind
// is one error).
func EventErrors(kinds map[string]map[string]any, kind string, data any) []string {
	schema, ok := kinds[kind]
	if !ok {
		return []string{fmt.Sprintf("unknown event kind %q", kind)}
	}
	return SchemaErrors(schema, data, "$")
}

// AuditReport is what Audit found: events checked, kinds seen, and each violation (deduplicated,
// with a count).
type AuditReport struct {
	Events     int
	Seen       map[string]bool
	Violations map[string]int
}

// Unseen lists the kinds no audited event had.
func (r AuditReport) Unseen(kinds map[string]map[string]any) []string {
	var out []string
	for k := range kinds {
		if !r.Seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Audit checks every event in dir's *.ndjson files against kinds.
func Audit(dir string, kinds map[string]map[string]any) (AuditReport, error) {
	report := AuditReport{Seen: map[string]bool{}, Violations: map[string]int{}}
	paths, err := filepath.Glob(filepath.Join(dir, "*.ndjson"))
	if err != nil {
		return report, err
	}
	sort.Strings(paths)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return report, err
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) == 0 {
				continue
			}
			dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
			dec.UseNumber()
			var event struct {
				Kind string `json:"kind"`
				Data any    `json:"data"`
			}
			if err := dec.Decode(&event); err != nil {
				return report, fmt.Errorf("%s: %w", p, err)
			}
			if strings.HasPrefix(event.Kind, TestKindPrefix) {
				continue
			}
			report.Events++
			report.Seen[event.Kind] = true
			for _, e := range EventErrors(kinds, event.Kind, event.Data) {
				report.Violations[event.Kind+": "+e]++
			}
		}
		if err := sc.Err(); err != nil {
			return report, fmt.Errorf("%s: %w", p, err)
		}
	}
	return report, nil
}

func stringList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	case []string:
		return x
	}
	return nil
}

func typeOK(kind string, value any) bool {
	switch kind {
	case "null":
		return value == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		return ok && !strings.ContainsAny(n.String(), ".eE")
	case "number":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	}
	panic(fmt.Sprintf("unsupported schema type %q", kind))
}

func jsonType(value any) string {
	for _, k := range []string{"null", "boolean", "integer", "number", "string", "array", "object"} {
		if typeOK(k, value) {
			return k
		}
	}
	return fmt.Sprintf("%T", value)
}

func quote(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprint(v)
}
