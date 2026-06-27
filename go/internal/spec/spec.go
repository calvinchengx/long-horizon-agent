// Package spec loads the language-neutral conformance cases in the repository's spec/ directory.
//
// The Go implementation runs every case in its conformance_*_test.go files; the Python
// implementation runs the same cases in python/tests/unit/test_spec_conformance.py.
package spec

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Dir is the absolute path of the repository's spec/ directory.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "spec")
}

// Load decodes spec/<rel> into v (numbers are kept as json.Number when v is `any`).
func Load(t testing.TB, rel string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(Dir(), rel))
	if err != nil {
		t.Fatalf("read spec %s: %v", rel, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode spec %s: %v", rel, err)
	}
}
