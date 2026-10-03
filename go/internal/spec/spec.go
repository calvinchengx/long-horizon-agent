// Package spec loads the language-neutral conformance cases in the repository's spec/ directory.
//
// The Go implementation runs every case in its conformance_*_test.go files; the Python
// implementation runs the same cases in python/tests/unit/test_spec_conformance.py.
//
// Coverage is enforced, not hoped for: Load decodes only the top-level case groups the target
// type maps, strictly (an unmapped nested field fails the test), and records which groups were
// read. After a full run of the package, Uncovered must be empty: every spec file loaded and every
// top-level group read by some test (docs/27-mission-ui.md#contract-coverage).
package spec

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Dir is the absolute path of the repository's spec/ directory.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "spec")
}

var (
	coverageMu sync.Mutex
	read       = map[string]map[string]bool{} // spec file -> top-level groups some test read
)

// Load decodes spec/<rel> into v (numbers are kept as json.Number when v is `any`).
//
// When v is a struct, only the top-level groups it maps are decoded, and strictly: a nested field
// the struct does not map fails the test. Those groups count as covered.
func Load(t testing.TB, rel string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(Dir(), rel))
	if err != nil {
		t.Fatalf("read spec %s: %v", rel, err)
	}
	var top map[string]json.RawMessage
	keys := mappedKeys(v)
	if keys != nil && json.Unmarshal(data, &top) == nil {
		picked := map[string]json.RawMessage{}
		for k, raw := range top {
			if keys[k] {
				picked[k] = raw
			}
		}
		if data, err = json.Marshal(picked); err != nil {
			t.Fatalf("re-encode spec %s: %v", rel, err)
		}
		record(rel, picked)
	} else if json.Unmarshal(data, &top) == nil {
		record(rel, top) // decoded whole (a map or `any`): every group counts as read
	} else {
		record(rel, map[string]json.RawMessage{"": nil}) // not an object: the file counts as read
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode spec %s: %v", rel, err)
	}
}

func record[V any](rel string, groups map[string]V) {
	coverageMu.Lock()
	defer coverageMu.Unlock()
	if read[rel] == nil {
		read[rel] = map[string]bool{}
	}
	for k := range groups {
		read[rel][k] = true
	}
}

// mappedKeys are the JSON names of v's struct fields, or nil when v is not a pointer to a struct.
func mappedKeys(v any) map[string]bool {
	t := reflect.TypeOf(v)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil
	}
	keys := map[string]bool{}
	st := t.Elem()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
		case name != "":
			keys[name] = true
		default:
			keys[f.Name] = true
		}
	}
	return keys
}

// Uncovered lists every spec file no test loaded and every top-level group no test read.
func Uncovered() ([]string, error) {
	var gaps []string
	err := filepath.WalkDir(Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return err
		}
		rel, _ := filepath.Rel(Dir(), path)
		rel = filepath.ToSlash(rel)
		coverageMu.Lock()
		got := read[rel]
		coverageMu.Unlock()
		if got == nil {
			gaps = append(gaps, rel+": never loaded")
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var top map[string]json.RawMessage
		if json.Unmarshal(data, &top) != nil {
			return nil // not an object: loading it was enough
		}
		for k := range top {
			if !got[k] && !got[""] {
				gaps = append(gaps, rel+": group "+k+" never read")
			}
		}
		return nil
	})
	sort.Strings(gaps)
	return gaps, err
}
