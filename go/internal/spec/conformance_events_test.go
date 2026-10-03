package spec

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// eventContract is spec/obs/mission_events.json: every trace event kind and its payload schema.
type eventContract struct {
	SchemaVersion json.Number               `json:"schema_version"`
	Kinds         map[string]map[string]any `json:"kinds"`
	Cases         []struct {
		Kind  string `json:"kind"`
		Data  any    `json:"data"`
		Valid bool   `json:"valid"`
	} `json:"cases"`
}

func TestEventSchemasJudgeEveryCaseLikeTheReference(t *testing.T) {
	var c eventContract
	Load(t, "obs/mission_events.json", &c)
	if c.SchemaVersion.String() != "1" || len(c.Kinds) == 0 || len(c.Cases) == 0 {
		t.Fatalf("contract: version %s, %d kinds, %d cases", c.SchemaVersion, len(c.Kinds), len(c.Cases))
	}
	for i, tc := range c.Cases {
		errs := obs.EventErrors(c.Kinds, tc.Kind, tc.Data)
		if (len(errs) == 0) != tc.Valid {
			t.Errorf("case %d (%s): valid=%v, errors %v", i, tc.Kind, tc.Valid, errs)
		}
	}
}

// TestTraceAudit checks the events a test run recorded (LHA_TRACE_AUDIT_DIR) against the
// contract: every event matches its kind's schema and every kind was recorded. CI runs the whole
// suite with LHA_TRACE_AUDIT_DIR set, then this test with LHA_TRACE_AUDIT_CHECK naming that
// directory.
func TestTraceAudit(t *testing.T) {
	dir := os.Getenv("LHA_TRACE_AUDIT_CHECK")
	if dir == "" {
		t.Skip("LHA_TRACE_AUDIT_CHECK is not set")
	}
	data, err := os.ReadFile(Dir() + "/obs/mission_events.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var c eventContract
	if err := dec.Decode(&c); err != nil {
		t.Fatal(err)
	}
	report, err := obs.Audit(dir, c.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	if report.Events == 0 {
		t.Fatalf("no events in %s", dir)
	}
	var violations []string
	for v := range report.Violations {
		violations = append(violations, v)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("event %s (x%d)", v, report.Violations[v])
	}
	for _, kind := range report.Unseen(c.Kinds) {
		t.Errorf("event kind %q was never recorded", kind)
	}
}
