package state

import (
	"encoding/json"
	"testing"
)

func TestDocSchemaMigrators(t *testing.T) {
	Register("test_artifact", 1, func(data map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range data {
			out[k] = v
		}
		out["schema_version"] = 2
		out["new_field"] = true
		return out
	})
	out, err := Migrate("test_artifact", map[string]any{"schema_version": 1}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if out["schema_version"] != 2 || out["new_field"] != true {
		t.Fatal(out)
	}
	// Missing schema_version means v1; already at target is a no-op.
	if _, err := Migrate("test_artifact", map[string]any{}, 2); err != nil {
		t.Fatal(err)
	}
	same, err := Migrate("nothing", map[string]any{"schema_version": json.Number("3")}, 3)
	if err != nil || same["schema_version"] != json.Number("3") {
		t.Fatal(same, err)
	}
}

func TestMigrateErrors(t *testing.T) {
	_, err := Migrate("unknown_art", map[string]any{"schema_version": "1"}, 2)
	if err == nil || err.Error() != "no migrator registered for 'unknown_art' v1" {
		t.Fatalf("err = %v", err)
	}
	Register("stuck", 1, func(d map[string]any) map[string]any { return map[string]any{"schema_version": 1} })
	_, err = Migrate("stuck", map[string]any{}, 2)
	if err == nil || err.Error() != "migrator for 'stuck' v1 did not advance the version" {
		t.Fatalf("err = %v", err)
	}
	// A migrator that drops schema_version advances by one.
	Register("drop", 1, func(d map[string]any) map[string]any { return map[string]any{} })
	if _, err := Migrate("drop", map[string]any{}, 2); err != nil {
		t.Fatal(err)
	}
}

func TestAsInt(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{true, 9}, {0, 9}, {3, 3}, {int64(4), 4}, {int32(5), 5}, {float64(6), 6}, {6.5, 9},
		{json.Number("7"), 7}, {json.Number("7.0"), 9}, {json.Number("x"), 9},
		{" 8 ", 8}, {"0", 9}, {"", 9}, {"-1", 9}, {"1a", 9}, {nil, 9}, {[]any{}, 9},
	}
	for _, c := range cases {
		if got := asInt(c.in, 9); got != c.want {
			t.Errorf("asInt(%#v) = %d, want %d", c.in, got, c.want)
		}
	}
}
