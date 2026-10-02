// Package agents holds the role "brains" around the lead loop (python: lha.agents): the Planner
// (mission intake: task -> verifiable checklist + file ownership) and the Replanner (split a
// blocked item into smaller ones).
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// PlannerSystemPrompt is ROLES["planner"].system_prompt.
const PlannerSystemPrompt = "You are the Planner/Architect. Decompose the mission into an ordered checklist of " +
	"small, independently-verifiable items, and assign single-writer-per-file ownership. " +
	"Mark items that touch shared files/types as serial."

// PlannerInstructions is the planning request appended to the mission text.
const PlannerInstructions = "Decompose the mission into 3-15 small, ordered, independently-verifiable steps.\n" +
	"Reply with ONLY a JSON array; each element: " +
	`{"description": "<imperative step>", "depends_on": [<1-based numbers of EARLIER steps>], ` +
	`"files": [<repo-relative paths this step creates or modifies>], ` +
	`"allow_harness_edits": <true only if the step must modify EXISTING tests or test config>, ` +
	`"witnesses": [<optional: checks proving THIS step is delivered, each "pytest:<node id>", ` +
	`"go:TestName" or "cmd:<shell command that exits 0>"; they gate the step on top of the ` +
	`mission checks>]}.` + "\n" +
	"Steps whose files do not overlap can be built in parallel, each by its own writer; list " +
	"every file a step writes, and keep shared files (pyproject.toml, lockfiles, __init__.py, " +
	"conftest.py, migrations) in as few steps as possible.\n" +
	"No prose, no code fences."

// Harness-owned directories: never part of a plan's write-set.
var harnessDirs = []string{".lha", ".git"}

var digitsRE = regexp.MustCompile(`[0-9]+`)

// MissionPlan is what the Planner produced: the checklist, the ownership map, and each item's
// declared files.
type MissionPlan struct {
	Checklist contracts.Checklist
	Ownership *FileOwnershipMap
	Files     map[string][]string // item id -> declared files
}

// normalizeDep maps a model-written dependency ("1", "01", 1, "step 1", "#1") to a 1-based index.
func normalizeDep(raw any) (int, bool) {
	switch x := raw.(type) {
	case json.Number:
		s := string(x)
		if !strings.ContainsAny(s, ".eE") {
			n, ok := new(big.Int).SetString(s, 10)
			if !ok || !n.IsInt64() {
				return 0, false // an index this large can never name a step
			}
			return int(n.Int64()), true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != math.Trunc(f) || math.IsInf(f, 0) || math.Abs(f) > 1e15 {
			return 0, false
		}
		return int(f), true
	case string:
		found := digitsRE.FindAllString(x, -1)
		if len(found) == 1 {
			n, err := strconv.Atoi(found[0])
			if err != nil {
				return 0, false // too large to name a step
			}
			return n, true
		}
	}
	return 0, false
}

func filesOf(entry *pyfmt.OrderedMap) []string {
	raw, ok := entry.Values["files"]
	if !ok {
		return []string{}
	}
	values, isList := raw.([]any)
	if !isList {
		values = []any{raw}
	}
	out := []string{}
	for _, v := range values {
		if s, ok := v.(string); ok {
			s = pyfmt.PyStrip(s)
			if s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// pyTruthy is Python's bool() for a JSON value.
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case *pyfmt.OrderedMap:
		return len(x.Keys) > 0
	}
	return true
}

type planEntry struct {
	position     int
	description  string
	deps         []any
	allow        bool
	files        []string
	witnesses    []string
	badWitnesses []string
}

// witnessesOf is the entry's valid witnesses and the ones dropped (as "<witness>: <why>").
// A Planner witness may only ADD a gate the sandbox can run (pytest:, go:, cmd:): a trusted:
// check runs outside the sandbox and stays the operator's to name (python: _witnesses_of).
func witnessesOf(entry *pyfmt.OrderedMap) (kept, dropped []string) {
	kept, dropped = []string{}, []string{}
	raw, ok := entry.Values["witnesses"]
	if !ok {
		return kept, dropped
	}
	values, isList := raw.([]any)
	if !isList {
		values = []any{raw}
	}
	for _, v := range values {
		s, ok := v.(string)
		if !ok || pyfmt.PyStrip(s) == "" {
			continue
		}
		witness := pyfmt.PyStrip(s)
		scheme, _, _ := strings.Cut(witness, ":")
		if scheme == "trusted" || scheme == "ci" {
			dropped = append(dropped, witness+": only the operator may name a trusted check")
			continue
		}
		if err := verify.ValidateWitness(witness); err != nil {
			dropped = append(dropped, witness+": "+err.Error())
			continue
		}
		if !slices.Contains(kept, witness) {
			kept = append(kept, witness)
		}
	}
	return kept, dropped
}

// ParsePlan extracts checklist items and each item's declared files from model output. Ids are
// zero-padded; dependencies are normalized to the assigned ids. Only references to EARLIER steps
// are kept (so cycles are impossible); unknown / forward / self references are dropped and
// recorded in the item's notes. Files are returned as written (AssignOwnership validates them).
func ParsePlan(text string) ([]contracts.ChecklistItem, map[string][]string) {
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start == -1 || end == -1 || end <= start {
		return []contracts.ChecklistItem{}, map[string][]string{}
	}
	parsed, err := pyfmt.DecodeOrdered([]byte(text[start : end+1]))
	if err != nil {
		return []contracts.ChecklistItem{}, map[string][]string{}
	}
	list, ok := parsed.([]any)
	if !ok {
		return []contracts.ChecklistItem{}, map[string][]string{}
	}
	entries := []planEntry{}
	for i, raw := range list {
		position := i + 1
		var e planEntry
		if entry, ok := raw.(*pyfmt.OrderedMap); ok {
			desc := entry.Values["description"]
			if !pyTruthy(desc) {
				desc = entry.Values["step"]
			}
			if !pyTruthy(desc) {
				desc = ""
			}
			e.description = pyfmt.PyStrip(pyfmt.PyStr(desc))
			rawDeps, has := entry.Values["depends_on"]
			if !has {
				rawDeps = []any{}
			}
			if l, ok := rawDeps.([]any); ok {
				e.deps = l
			} else {
				e.deps = []any{rawDeps}
			}
			e.allow = entry.Values["allow_harness_edits"] == true
			e.files = filesOf(entry)
			e.witnesses, e.badWitnesses = witnessesOf(entry)
		} else {
			e.description = pyfmt.PyStrip(pyfmt.PyStr(raw))
			e.deps = []any{}
			e.files = []string{}
			e.witnesses, e.badWitnesses = []string{}, []string{}
		}
		if e.description != "" {
			e.position = position
			entries = append(entries, e)
		}
	}
	width := max(2, len(strconv.Itoa(len(entries))))
	idFor := map[int]string{}
	for n, e := range entries {
		idFor[e.position] = fmt.Sprintf("%0*d", width, n+1)
	}
	items := []contracts.ChecklistItem{}
	filesByItem := map[string][]string{}
	for _, e := range entries {
		kept, dropped := []string{}, []string{}
		for _, raw := range e.deps {
			index, ok := normalizeDep(raw)
			if ok && index < e.position {
				if id, known := idFor[index]; known {
					if !slices.Contains(kept, id) {
						kept = append(kept, id)
					}
					continue
				}
			}
			if raw != nil && raw != "" {
				dropped = append(dropped, pyfmt.PyStr(raw))
			}
		}
		item := contracts.NewChecklistItem(idFor[e.position], e.description, kept...)
		item.AllowHarnessEdits = e.allow
		item.Witnesses = e.witnesses
		notes := []string{}
		if len(dropped) > 0 {
			notes = append(notes, "planner dropped invalid depends_on: "+pyfmt.PyReprValue(dropped))
		}
		if len(e.badWitnesses) > 0 {
			notes = append(notes, "planner dropped invalid witnesses: "+pyfmt.PyReprValue(e.badWitnesses))
		}
		item.Notes = strings.Join(notes, "; ")
		items = append(items, item)
		if len(e.files) > 0 {
			filesByItem[idFor[e.position]] = e.files
		}
	}
	return items, filesByItem
}

// ParseChecklist extracts a checklist from model output (ParsePlan without the files).
func ParseChecklist(text string) []contracts.ChecklistItem {
	items, _ := ParsePlan(text)
	return items
}

func addNote(item *contracts.ChecklistItem, note string) {
	if item.Notes != "" {
		item.Notes = item.Notes + "; " + note
	} else {
		item.Notes = note
	}
}

// AssignOwnership gives each item's declared files to its implementer, or keeps the item serial.
// It mutates items in place: serial items get a notes entry saying why, and an item that overlaps
// an earlier item's files gains a depends_on edge to it (earlier items only, so no cycle).
func AssignOwnership(items []contracts.ChecklistItem, filesByItem map[string][]string) *FileOwnershipMap {
	ownership := NewFileOwnershipMap()
	claimed := map[string]string{} // case-folded path -> id of the first item that declared it
	for idx := range items {
		item := &items[idx]
		declared := filesByItem[item.ID]
		if len(declared) == 0 {
			continue
		}
		keys, invalid := []string{}, []string{}
		for _, p := range declared {
			norm, err := NormalizePath(p)
			if err != nil {
				invalid = append(invalid, p)
				continue
			}
			first, _, _ := strings.Cut(norm, "/")
			if slices.Contains(harnessDirs, first) {
				invalid = append(invalid, p)
				continue
			}
			if key := casefold(norm); !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		shared := []string{}
		overlapSet := map[string]bool{}
		for _, k := range keys {
			if s, _ := IsShared(k); s {
				shared = append(shared, k)
			}
			if owner, ok := claimed[k]; ok {
				overlapSet[owner] = true
			}
		}
		overlaps := make([]string, 0, len(overlapSet))
		for o := range overlapSet {
			overlaps = append(overlaps, o)
		}
		sort.Strings(overlaps)
		for _, k := range keys {
			if _, ok := claimed[k]; !ok {
				claimed[k] = item.ID
			}
		}
		switch {
		case len(invalid) > 0:
			addNote(item, "serial: invalid file paths "+pyfmt.PyReprValue(invalid))
		case len(shared) > 0:
			addNote(item, "serial: touches shared files "+pyfmt.PyReprValue(shared))
		case len(overlaps) > 0:
			for _, other := range overlaps {
				if !slices.Contains(item.DependsOn, other) {
					item.DependsOn = append(item.DependsOn, other)
				}
			}
			addNote(item, "serial after "+strings.Join(overlaps, ", ")+": overlapping files")
		default:
			writer := WriterForItem(item.ID)
			for _, k := range keys {
				_ = ownership.Assign(k, writer) // non-shared, unclaimed: cannot conflict
			}
		}
	}
	return ownership
}

// Planner turns a mission into a checklist (and file ownership) using the model, with a
// fallback: if the model returns nothing usable, the plan is a single item built from the
// description, so a mission can always start.
type Planner struct {
	model contracts.ModelProvider
}

// NewPlanner returns a Planner using model (pass a metered provider to budget it).
func NewPlanner(model contracts.ModelProvider) *Planner { return &Planner{model: model} }

// PlannerMessages are the messages the Planner sends (exported for parity tests).
func PlannerMessages(title, description, acceptance string) []contracts.ModelMessage {
	if acceptance == "" {
		acceptance = "(use your judgment)"
	}
	return []contracts.ModelMessage{
		{Role: "system", Content: PlannerSystemPrompt},
		{Role: "user", Content: "Mission: " + title + "\n\n" + description + "\n\n" +
			"Definition of done: " + acceptance + "\n\n" + PlannerInstructions},
	}
}

// PlanMission plans the checklist and assigns single-writer file ownership from declared files.
// A model error (e.g. a *governor.BudgetExceeded) is returned unchanged.
func (p *Planner) PlanMission(ctx context.Context, title, description, acceptance string) (MissionPlan, error) {
	result, err := p.model.Complete(ctx, PlannerMessages(title, description, acceptance), nil, 0)
	if err != nil {
		return MissionPlan{}, err
	}
	items, files := ParsePlan(result.Text)
	if len(items) == 0 {
		desc := pyfmt.PyStrip(description)
		if desc == "" {
			desc = pyfmt.PyStrip(title)
		}
		items = []contracts.ChecklistItem{contracts.NewChecklistItem("01", desc)}
		files = map[string][]string{}
	}
	ownership := AssignOwnership(items, files)
	return MissionPlan{Checklist: contracts.Checklist{Items: items, SchemaVersion: 1}, Ownership: ownership, Files: files}, nil
}

// Plan returns the checklist alone.
func (p *Planner) Plan(ctx context.Context, title, description, acceptance string) (contracts.Checklist, error) {
	plan, err := p.PlanMission(ctx, title, description, acceptance)
	return plan.Checklist, err
}
