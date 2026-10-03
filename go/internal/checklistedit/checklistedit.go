// Package checklistedit applies an operator's edits to a mission checklist (python:
// lha.state.checklist_edit): add, remove, edit, reopen, block and unblock items, as one atomic
// batch. The batch is a list of objects, each with an "op":
//
//	{"op": "add", "description": "...", "id"?, "witnesses"?, "depends_on"?, "after"?,
//	 "allow_harness_edits"?, "notes"?}
//	{"op": "remove", "id"}             an open item (never a done or split one)
//	{"op": "edit", "id", "description"?, "witnesses"?, "depends_on"?, "notes"?, "allow_harness_edits"?}
//	{"op": "reopen", "id"}             done -> todo (its verification is forgotten)
//	{"op": "block", "id"}              an open item -> blocked (a human parked it)
//	{"op": "unblock", "id"}            blocked -> todo
//
// A description may carry witnesses the Markdown roadmap way ("Do X (witness: cmd:true)"). The
// batch is validated as a whole (ids, dependencies, witness syntax, at least one item left); one
// bad edit refuses the whole batch and the checklist is unchanged. Every message is Python's,
// byte for byte (spec/state/checklist_edit.json).
package checklistedit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/checklistimport"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// MaxEditOps is the edits per batch (one signal, one lha mission-edit call).
const MaxEditOps = 50

// Error is python's ChecklistEditError: the batch is malformed or would leave the checklist
// invalid; nothing was changed.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func errorf(format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

var fields = map[string][]string{
	"add":     {"op", "id", "description", "witnesses", "depends_on", "after", "allow_harness_edits", "notes"},
	"remove":  {"op", "id"},
	"edit":    {"op", "id", "description", "witnesses", "depends_on", "allow_harness_edits", "notes"},
	"reopen":  {"op", "id"},
	"block":   {"op", "id"},
	"unblock": {"op", "id"},
}

var editOrder = []string{"description", "witnesses", "depends_on", "notes", "allow_harness_edits"}

// Apply applies edits to cl in place and returns one summary line per edit; on an *Error the
// checklist is untouched. reserved ids (every item a cycle ever worked on, from the anchor's
// events) and the ids the checklist starts with are never handed to an added item (python:
// apply_edits).
func Apply(cl *contracts.Checklist, edits []any, by string, reserved []string) ([]string, error) {
	if len(edits) == 0 {
		return nil, errorf("edits must be a non-empty list of objects")
	}
	if len(edits) > MaxEditOps {
		return nil, errorf("too many edits (%d > %d)", len(edits), MaxEditOps)
	}
	draft := clone(*cl)
	taken := map[string]bool{}
	for _, id := range reserved {
		taken[id] = true
	}
	for _, it := range cl.Items {
		taken[it.ID] = true
	}
	lines := make([]string, 0, len(edits))
	for i, edit := range edits {
		line, err := applyOne(&draft, i+1, edit, by, taken)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	if len(draft.Items) == 0 {
		return nil, errorf("the checklist would have no items")
	}
	if errs := draft.DependencyErrors(); len(errs) > 0 {
		return nil, errorf("invalid checklist: %s", strings.Join(errs, "; "))
	}
	for _, it := range draft.Items {
		for _, w := range it.Witnesses {
			if err := verify.ValidateWitness(w); err != nil {
				return nil, errorf("item %s: %s", contracts.PyRepr(it.ID), err.Error())
			}
		}
	}
	cl.Items = draft.Items
	return lines, nil
}

// WorkedItemIDs is every item id the anchor's events name (item_id): reserved for NextItemID
// (python: worked_item_ids), sorted.
func WorkedItemIDs(events []contracts.EventRecord) []string {
	seen := map[string]bool{}
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		if id, ok := e.Payload.String("item_id"); ok && id != "" {
			seen[id] = true
		}
	}
	return keys(seen)
}

func clone(cl contracts.Checklist) contracts.Checklist {
	items := make([]contracts.ChecklistItem, len(cl.Items))
	for i, it := range cl.Items {
		it.VerifiedBy = append([]string{}, it.VerifiedBy...)
		it.DependsOn = append([]string{}, it.DependsOn...)
		it.Witnesses = append([]string{}, it.Witnesses...)
		items[i] = it
	}
	cl.Items = items
	return cl
}

func applyOne(cl *contracts.Checklist, n int, edit any, by string, taken map[string]bool) (string, error) {
	m, ok := edit.(map[string]any)
	if !ok {
		return "", errorf("edit #%d: must be an object", n)
	}
	opValue, present := m["op"]
	if !present || opValue == nil {
		return "", errorf("edit #%d: missing 'op'", n)
	}
	op, isStr := opValue.(string)
	allowed, known := fields[op]
	if !isStr || !known {
		return "", errorf("edit #%d: unknown op %s", n, pyRepr(opValue))
	}
	var unknown []string
	for k := range m {
		if !contains(allowed, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", errorf("edit #%d: unknown field(s) %s", n, pyList(unknown))
	}
	if op == "add" {
		return add(cl, n, m, taken)
	}
	id, err := str(n, m, "id")
	if err != nil {
		return "", err
	}
	if id == nil || pyfmt.PyStrip(*id) == "" {
		return "", errorf("edit #%d: 'id' is required", n)
	}
	item := cl.Get(*id)
	if item == nil {
		return "", errorf("edit #%d: unknown item %s", n, contracts.PyRepr(*id))
	}
	if item.Status == contracts.StatusSplit {
		return "", errorf("edit #%d: item %s was split", n, contracts.PyRepr(*id))
	}
	switch op {
	case "remove":
		if item.Status == contracts.StatusDone {
			return "", errorf("edit #%d: cannot remove done item %s", n, contracts.PyRepr(*id))
		}
		kept := []contracts.ChecklistItem{}
		for _, it := range cl.Items {
			if it.ID != *id {
				kept = append(kept, it)
			}
		}
		cl.Items = kept
		return "removed " + *id, nil
	case "edit":
		return editItem(n, m, item)
	case "reopen":
		if item.Status != contracts.StatusDone {
			return "", errorf("edit #%d: item %s is not done", n, contracts.PyRepr(*id))
		}
		item.Status = contracts.StatusTodo
		item.VerifiedBy = []string{}
		item.ConsecutiveFailures = 0
		item.LastFailure = ""
		return "reopened " + *id, nil
	case "block":
		if item.Status == contracts.StatusDone {
			return "", errorf("edit #%d: item %s is done", n, contracts.PyRepr(*id))
		}
		if item.Status == contracts.StatusBlocked {
			return "", errorf("edit #%d: item %s is already blocked", n, contracts.PyRepr(*id))
		}
		item.Status = contracts.StatusBlocked
		item.LastFailure = "blocked by an operator"
		if by != "" {
			item.LastFailure = "blocked by " + by
		}
		return "blocked " + *id, nil
	}
	if item.Status != contracts.StatusBlocked {
		return "", errorf("edit #%d: item %s is not blocked", n, contracts.PyRepr(*id))
	}
	if _, err := cl.Unblock(*id); err != nil {
		return "", err
	}
	return "unblocked " + *id, nil
}

func add(cl *contracts.Checklist, n int, m map[string]any, taken map[string]bool) (string, error) {
	text, err := str(n, m, "description")
	if err != nil {
		return "", err
	}
	description, witnesses := checklistimport.SplitWitnesses(deref(text))
	if description == "" {
		return "", errorf("edit #%d: description must not be empty", n)
	}
	extra, err := strList(n, m, "witnesses")
	if err != nil {
		return "", err
	}
	witnesses = append(witnesses, extra...)
	idp, err := str(n, m, "id")
	if err != nil {
		return "", err
	}
	var id string
	if idp != nil {
		id = pyfmt.PyStrip(*idp)
		if id == "" {
			return "", errorf("edit #%d: 'id' must not be empty", n)
		}
		if cl.Get(id) != nil {
			return "", errorf("edit #%d: item %s already exists", n, contracts.PyRepr(id))
		}
	} else {
		id = NextItemID(cl, keys(taken))
	}
	deps, err := strList(n, m, "depends_on")
	if err != nil {
		return "", err
	}
	harness, err := boolean(n, m, "allow_harness_edits")
	if err != nil {
		return "", err
	}
	notes, err := str(n, m, "notes")
	if err != nil {
		return "", err
	}
	item := contracts.NewChecklistItem(id, description, deps...)
	item.Witnesses = witnesses
	item.AllowHarnessEdits = harness
	item.Notes = deref(notes)
	after, err := str(n, m, "after")
	if err != nil {
		return "", err
	}
	if after != nil && *after != "" {
		at := -1
		for i, it := range cl.Items {
			if it.ID == *after {
				at = i + 1
				break
			}
		}
		if at < 0 {
			return "", errorf("edit #%d: unknown item %s in 'after'", n, contracts.PyRepr(*after))
		}
		items := append([]contracts.ChecklistItem{}, cl.Items[:at]...)
		items = append(items, item)
		cl.Items = append(items, cl.Items[at:]...)
	} else {
		cl.Items = append(cl.Items, item)
	}
	return "added " + id, nil
}

func editItem(n int, m map[string]any, item *contracts.ChecklistItem) (string, error) {
	if item.Status == contracts.StatusDone {
		return "", errorf("edit #%d: item %s is done; reopen it first", n, contracts.PyRepr(item.ID))
	}
	changed := []string{}
	for _, f := range editOrder {
		if _, ok := m[f]; ok {
			changed = append(changed, f)
		}
	}
	if len(changed) == 0 {
		return "", errorf("edit #%d: nothing to change on %s", n, contracts.PyRepr(item.ID))
	}
	if _, ok := m["description"]; ok {
		text, err := str(n, m, "description")
		if err != nil {
			return "", err
		}
		description, extra := checklistimport.SplitWitnesses(deref(text))
		if description == "" {
			return "", errorf("edit #%d: description must not be empty", n)
		}
		item.Description = description
		if len(extra) > 0 {
			item.Witnesses = append(append([]string{}, item.Witnesses...), extra...)
		}
	}
	if _, ok := m["witnesses"]; ok {
		w, err := strList(n, m, "witnesses")
		if err != nil {
			return "", err
		}
		item.Witnesses = w
	}
	if _, ok := m["depends_on"]; ok {
		d, err := strList(n, m, "depends_on")
		if err != nil {
			return "", err
		}
		item.DependsOn = d
	}
	if _, ok := m["notes"]; ok {
		notes, err := str(n, m, "notes")
		if err != nil {
			return "", err
		}
		item.Notes = deref(notes)
	}
	if _, ok := m["allow_harness_edits"]; ok {
		b, err := boolean(n, m, "allow_harness_edits")
		if err != nil {
			return "", err
		}
		item.AllowHarnessEdits = b
	}
	return fmt.Sprintf("edited %s: %s", item.ID, strings.Join(changed, ", ")), nil
}

// NextItemID is one past the largest numeric id among the items and reserved, zero-padded to
// the import's width: "04" after "01".."03", and still "04" when "03" was removed (its id stays
// reserved), so the anchor's history never names two items the same. Ids that are not plain
// digits ("02.1", "02a") do not count (python: next_item_id).
func NextItemID(cl *contracts.Checklist, reserved []string) string {
	ids := make([]string, 0, len(cl.Items)+len(reserved))
	for _, it := range cl.Items {
		ids = append(ids, it.ID)
	}
	ids = append(ids, reserved...)
	n, width := 1, 2
	for _, id := range ids {
		if !isDigits(id) {
			continue
		}
		v, err := strconv.Atoi(id)
		if err != nil {
			continue
		}
		n = max(n, v+1)
		width = max(width, len(id))
	}
	width = max(width, len(strconv.Itoa(n)))
	for {
		id := fmt.Sprintf("%0*d", width, n)
		if cl.Get(id) == nil && !contains(reserved, id) {
			return id
		}
		n++
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isDigits is python's str.isdigit() for ASCII ids (non-ASCII digits never appear in ids the
// import or the Planner produce).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func str(n int, m map[string]any, key string) (*string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	s, isStr := v.(string)
	if !isStr {
		return nil, errorf("edit #%d: %s must be a string", n, contracts.PyRepr(key))
	}
	return &s, nil
}

func strList(n int, m map[string]any, key string) ([]string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return []string{}, nil
	}
	raw, isList := v.([]any)
	if !isList {
		return nil, errorf("edit #%d: %s must be a list of strings", n, contracts.PyRepr(key))
	}
	out := []string{}
	for _, e := range raw {
		s, isStr := e.(string)
		if !isStr {
			return nil, errorf("edit #%d: %s must be a list of strings", n, contracts.PyRepr(key))
		}
		if t := pyfmt.PyStrip(s); t != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

func boolean(n int, m map[string]any, key string) (bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, errorf("edit #%d: %s must be a boolean", n, contracts.PyRepr(key))
	}
	return b, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// pyRepr is python's repr() of a JSON scalar (strings quoted; other values as JSON prints them).
func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return contracts.PyRepr(s)
	}
	return fmt.Sprint(v)
}

func pyList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = contracts.PyRepr(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
