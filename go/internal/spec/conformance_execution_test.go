package spec

import (
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
)

func TestExecutionPaths(t *testing.T) {
	var s struct {
		NormalizeRelpath []struct {
			Path       string  `json:"path"`
			Normalized *string `json:"normalized"`
			Error      *string `json:"error"`
		} `json:"normalize_relpath"`
		IsProtected []struct {
			Path      string  `json:"path"`
			Protected *bool   `json:"protected"`
			Error     *string `json:"error"`
		} `json:"is_protected"`
		ContainedPosix []struct {
			Workdir string  `json:"workdir"`
			Path    string  `json:"path"`
			Result  *string `json:"result"`
			Error   *string `json:"error"`
		} `json:"contained_posix"`
	}
	Load(t, "execution/paths.json", &s)
	if len(s.NormalizeRelpath) == 0 || len(s.IsProtected) == 0 || len(s.ContainedPosix) == 0 {
		t.Fatal("missing path sections")
	}
	check := func(what, path string, got string, err error, want, wantErr *string) {
		t.Helper()
		switch {
		case wantErr != nil && (err == nil || err.Error() != *wantErr):
			t.Errorf("%s(%q) error = %v, want %q", what, path, err, *wantErr)
		case wantErr == nil && (err != nil || got != *want):
			t.Errorf("%s(%q) = %q, %v; want %q", what, path, got, err, *want)
		}
	}
	for _, c := range s.NormalizeRelpath {
		got, err := execution.NormalizeRelpath(c.Path)
		check("NormalizeRelpath", c.Path, got, err, c.Normalized, c.Error)
	}
	for _, c := range s.IsProtected {
		got, err := execution.IsProtected(c.Path)
		switch {
		case c.Error != nil && (err == nil || err.Error() != *c.Error):
			t.Errorf("IsProtected(%q) error = %v, want %q", c.Path, err, *c.Error)
		case c.Error == nil && (err != nil || got != *c.Protected):
			t.Errorf("IsProtected(%q) = %v, %v", c.Path, got, err)
		}
	}
	for _, c := range s.ContainedPosix {
		got, err := execution.ContainedPosix(c.Workdir, c.Path)
		check("ContainedPosix("+c.Workdir+")", c.Path, got, err, c.Result, c.Error)
	}
}

func TestExecutionArguments(t *testing.T) {
	var s struct {
		Validate []struct {
			Schema map[string]any `json:"schema"`
			Value  any            `json:"value"`
			Where  string         `json:"where"`
			Error  *string        `json:"error"`
		} `json:"validate"`
		MissingRequired []struct {
			Required  any            `json:"required"`
			Arguments map[string]any `json:"arguments"`
			Missing   []string       `json:"missing"`
		} `json:"missing_required"`
	}
	Load(t, "execution/arguments.json", &s)
	if len(s.Validate) == 0 || len(s.MissingRequired) == 0 {
		t.Fatal("missing argument sections")
	}
	for _, c := range s.Validate {
		want := ""
		if c.Error != nil {
			want = *c.Error
		}
		if got := execution.ValidateArguments(c.Schema, c.Value, c.Where); got != want {
			t.Errorf("ValidateArguments(%v, %v, %q) = %q, want %q", c.Schema, c.Value, c.Where, got, want)
		}
	}
	for _, c := range s.MissingRequired {
		got := execution.MissingRequired(map[string]any{"required": c.Required}, c.Arguments)
		if len(got) == 0 && len(c.Missing) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.Missing) {
			t.Errorf("MissingRequired(%v, %v) = %v, want %v", c.Required, c.Arguments, got, c.Missing)
		}
	}
}
