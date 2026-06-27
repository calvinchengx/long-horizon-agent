package spec

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func TestCheckNames(t *testing.T) {
	var s struct {
		Derive []struct {
			Command []string `json:"command"`
			Name    string   `json:"name"`
		} `json:"derive_check_name"`
		FromCommands []struct {
			Commands [][]string `json:"commands"`
			Names    []string   `json:"names"`
		} `json:"checks_from_commands"`
	}
	Load(t, "contracts/check_names.json", &s)
	for _, c := range s.Derive {
		if got := contracts.DeriveCheckName(c.Command); got != c.Name {
			t.Errorf("DeriveCheckName(%q) = %q, want %q", c.Command, got, c.Name)
		}
	}
	for _, c := range s.FromCommands {
		got := []string{}
		for _, ch := range contracts.ChecksFromCommands(c.Commands, true) {
			got = append(got, ch.Name)
		}
		if !reflect.DeepEqual(got, c.Names) {
			t.Errorf("ChecksFromCommands(%q) = %q, want %q", c.Commands, got, c.Names)
		}
	}
}

func TestChecklist(t *testing.T) {
	var s struct {
		Scenarios []struct {
			Name             string              `json:"name"`
			Checklist        contracts.Checklist `json:"checklist"`
			NextActionable   *string             `json:"next_actionable"`
			IsComplete       bool                `json:"is_complete"`
			IsDeadlocked     bool                `json:"is_deadlocked"`
			DeadlockReason   string              `json:"deadlock_reason"`
			DependencyErrors []string            `json:"dependency_errors"`
			ItemsDone        int                 `json:"items_done"`
		} `json:"scenarios"`
		Transitions struct {
			Initial contracts.Checklist `json:"initial"`
			Steps   []struct {
				Op   string `json:"op"`
				Args struct {
					ItemID     string   `json:"item_id"`
					Reason     string   `json:"reason"`
					Max        int      `json:"max"`
					VerifiedBy []string `json:"verified_by"`
				} `json:"args"`
				After json.RawMessage `json:"after"`
			} `json:"steps"`
		} `json:"transitions"`
		Split struct {
			Initial contracts.Checklist `json:"initial"`
			ItemID  string              `json:"item_id"`
			Drafts  []struct {
				Description string   `json:"description"`
				Witnesses   []string `json:"witnesses"`
			} `json:"drafts"`
			After          json.RawMessage `json:"after"`
			NextActionable string          `json:"next_actionable"`
			ItemsTotal     int             `json:"items_total"`
			IsComplete     bool            `json:"is_complete"`
		} `json:"split"`
	}
	Load(t, "state/checklist.json", &s)
	for _, c := range s.Scenarios {
		cl := c.Checklist
		var next *string
		if it := cl.NextActionable(); it != nil {
			next = &it.ID
		}
		if !reflect.DeepEqual(next, c.NextActionable) {
			t.Errorf("%s: NextActionable = %v, want %v", c.Name, deref(next), deref(c.NextActionable))
		}
		if cl.IsComplete() != c.IsComplete || cl.IsDeadlocked() != c.IsDeadlocked {
			t.Errorf("%s: complete/deadlocked = %v/%v, want %v/%v", c.Name,
				cl.IsComplete(), cl.IsDeadlocked(), c.IsComplete, c.IsDeadlocked)
		}
		if got := cl.DeadlockReason(); got != c.DeadlockReason {
			t.Errorf("%s: DeadlockReason = %q, want %q", c.Name, got, c.DeadlockReason)
		}
		if got := cl.DependencyErrors(); !reflect.DeepEqual(got, c.DependencyErrors) {
			t.Errorf("%s: DependencyErrors = %q, want %q", c.Name, got, c.DependencyErrors)
		}
		if cl.ItemsDone() != c.ItemsDone {
			t.Errorf("%s: ItemsDone = %d, want %d", c.Name, cl.ItemsDone(), c.ItemsDone)
		}
	}
	cl := s.Transitions.Initial
	for _, step := range s.Transitions.Steps {
		var err error
		switch step.Op {
		case "start":
			_, err = cl.Start(step.Args.ItemID)
		case "record_failure":
			_, err = cl.RecordFailure(step.Args.ItemID, step.Args.Reason, step.Args.Max)
		case "unblock":
			_, err = cl.Unblock(step.Args.ItemID)
		default:
			_, err = cl.RecordSuccess(step.Args.ItemID, step.Args.VerifiedBy)
		}
		if err != nil {
			t.Fatalf("%s: %v", step.Op, err)
		}
		JSONEqual(t, step.Op, cl, step.After)
	}

	sp := s.Split
	split := sp.Initial
	drafts := []contracts.ChecklistItem{}
	for _, d := range sp.Drafts {
		draft := contracts.NewChecklistItem("draft", d.Description)
		draft.Witnesses = d.Witnesses
		drafts = append(drafts, draft)
	}
	if _, err := split.Split(sp.ItemID, drafts); err != nil {
		t.Fatalf("split: %v", err)
	}
	JSONEqual(t, "split", split, sp.After)
	next := ""
	if it := split.NextActionable(); it != nil {
		next = it.ID
	}
	if next != sp.NextActionable || split.ItemsTotal() != sp.ItemsTotal || split.IsComplete() != sp.IsComplete {
		t.Errorf("after split: next=%q total=%d complete=%v, want %q %d %v",
			next, split.ItemsTotal(), split.IsComplete(), sp.NextActionable, sp.ItemsTotal, sp.IsComplete)
	}
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
