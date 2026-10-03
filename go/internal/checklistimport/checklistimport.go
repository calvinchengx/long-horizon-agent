// Package checklistimport seeds a mission with the operator's own checklist instead of the
// Planner's (python: lha.state.checklist_import).
//
// Two formats, chosen by file extension:
//
//   - .json — either a Checklist dump ({"items": [...]} with ids) or a mission file:
//     {"title": "...", "description": "...", "references": ["docs/api.md"],
//     "items": [{"description": "...", "id": "01", "depends_on": [], "witnesses": ["go:TestX"],
//     "allow_harness_edits": false}]}.
//     Items without an "id" get zero-padded sequential ids (01, 02, ... by position).
//
//   - .md — a Markdown roadmap: the first "# " heading is the title; paragraph text before the
//     first "## " is the description; each "## " section is a phase; each "- [ ] text" line is
//     an item ("- [x]" too, only with includeDone, imported as todo so the harness re-verifies
//     it). Lines indented under a checkbox continue its description; nested bullets that are not
//     checkboxes are ignored. Witnesses are declared inline — "(witness: go:TestX)" or
//     "(witnesses: go:TestX, ci:e2e)" — and removed from the description. Items in a phase
//     depend on EVERY item of the nearest earlier phase that has items; items within one phase
//     are independent.
//
// Everything is validated up front (ids, dependencies, witness syntax) and reported as an
// *ImportError naming the file and, for Markdown, the line. Messages match the Python text.
package checklistimport

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// ImportError is an operator checklist file that is unreadable, malformed or structurally
// invalid (python: ChecklistImportError).
type ImportError struct{ Message string }

func (e *ImportError) Error() string { return e.Message }

func importErr(format string, args ...any) error {
	return &ImportError{Message: fmt.Sprintf(format, args...)}
}

// ImportedChecklist is a parsed checklist file: the items plus the mission metadata it declared.
type ImportedChecklist struct {
	Checklist   contracts.Checklist
	Title       string
	Description string
	References  []string
}

// Load loads and validates an operator checklist (.json or .md / .markdown).
//
// Errors are *ImportError, except the ones Python does not wrap in ChecklistImportError: a file
// that is not valid UTF-8 (*DecodeError, python UnicodeDecodeError) and a path with a NUL byte.
func Load(path string, includeDone bool) (ImportedChecklist, error) {
	p := pyPath(path)
	text, osErr, err := readText(p)
	if err != nil {
		return ImportedChecklist{}, err
	}
	if osErr != "" {
		return ImportedChecklist{}, importErr("%s: cannot read checklist: %s", p, osErr)
	}
	var imported ImportedChecklist
	lines := map[string]int{}
	switch suffix := pyLower(pySuffix(p)); suffix {
	case ".json":
		imported, err = fromJSON(p, text)
	case ".md", ".markdown":
		imported, lines, err = fromMarkdown(p, text, includeDone)
	default:
		err = importErr("%s: unsupported checklist format %s (use .json or .md)", p, contracts.PyRepr(suffix))
	}
	if err != nil {
		return ImportedChecklist{}, err
	}
	if err := validate(p, &imported.Checklist, lines); err != nil {
		return ImportedChecklist{}, err
	}
	return imported, nil
}

func validate(path string, checklist *contracts.Checklist, lines map[string]int) error {
	where := func(id string) string {
		if n, ok := lines[id]; ok {
			return fmt.Sprintf("%s:%d", path, n)
		}
		return path
	}
	if len(checklist.Items) == 0 {
		return importErr("%s: checklist has no items", path)
	}
	if errs := checklist.DependencyErrors(); len(errs) > 0 {
		return importErr("%s: invalid checklist: %s", path, strings.Join(errs, "; "))
	}
	for _, item := range checklist.Items {
		for _, w := range item.Witnesses {
			if err := verify.ValidateWitness(w); err != nil {
				return importErr("%s: item %s: %s", where(item.ID), contracts.PyRepr(item.ID), err.Error())
			}
		}
	}
	return nil
}

// --- JSON -----------------------------------------------------------------------------------

func strList(path string, value any, what string) ([]string, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, importErr("%s: %s must be a list of strings", path, what)
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return nil, importErr("%s: %s must be a list of strings", path, what)
		}
		out = append(out, s)
	}
	return out, nil
}

func pyListRepr(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = contracts.PyRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func fromJSON(path, text string) (ImportedChecklist, error) {
	decoded, err := pyJSONLoads(text)
	if err != nil {
		return ImportedChecklist{}, importErr("%s: invalid JSON: %s", path, err.Error())
	}
	if list, ok := decoded.([]any); ok {
		d := newPyDict()
		d.set("items", list)
		decoded = d
	}
	data, ok := decoded.(*pyDict)
	var rawItems []any
	if ok {
		v, _ := data.get("items")
		rawItems, ok = v.([]any)
	}
	if !ok {
		return ImportedChecklist{}, importErr(`%s: expected an object with an "items" list`, path)
	}
	title, description := any(""), any("")
	if v, ok := data.get("title"); ok {
		title = v
	}
	if v, ok := data.get("description"); ok {
		description = v
	}
	titleStr, ok1 := title.(string)
	descStr, ok2 := description.(string)
	if !ok1 || !ok2 {
		return ImportedChecklist{}, importErr(`%s: "title" and "description" must be strings`, path)
	}
	var refsRaw any = []any{}
	if v, ok := data.get("references"); ok {
		refsRaw = v
	}
	references, err := strList(path, refsRaw, `"references"`)
	if err != nil {
		return ImportedChecklist{}, err
	}
	width := max(2, len(strconv.Itoa(len(rawItems))))
	items := []contracts.ChecklistItem{}
	for idx, raw := range rawItems {
		label := fmt.Sprintf("items[%d]", idx)
		obj, ok := raw.(*pyDict)
		if !ok {
			return ImportedChecklist{}, importErr("%s: %s must be an object", path, label)
		}
		unknown := []string{}
		for _, k := range obj.keys {
			if !isItemField(k) {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown) // UTF-8 byte order == Python's code-point order
			return ImportedChecklist{}, importErr("%s: %s has unknown field(s): %s", path, label, pyListRepr(unknown))
		}
		fields := newPyDict()
		for _, k := range obj.keys {
			fields.set(k, obj.vals[k])
		}
		if _, ok := fields.get("id"); !ok {
			fields.set("id", fmt.Sprintf("%0*d", width, idx+1))
		}
		item, verr := validateItem(fields)
		if verr != "" {
			return ImportedChecklist{}, importErr("%s: %s is invalid: %s", path, label, verr)
		}
		if pyStrip(item.ID) == "" || pyStrip(item.Description) == "" {
			return ImportedChecklist{}, importErr("%s: %s needs a non-empty id and description", path, label)
		}
		items = append(items, item)
	}
	return ImportedChecklist{
		Checklist:   contracts.Checklist{Items: items, SchemaVersion: 1},
		Title:       pyStrip(titleStr),
		Description: pyStrip(descStr),
		References:  references,
	}, nil
}

// --- Markdown -------------------------------------------------------------------------------

// sp is Python's Unicode \s (the characters for which str.isspace() is true); Go's \s is ASCII.
const sp = `[\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

// witnessWord is "witness(?:es)?" under Python's re.IGNORECASE, which (unlike Go's (?i)) also
// matches U+0130 and U+0131 for "i" (and, like Go, U+017F for "s").
const witnessWord = `[wW][iIİı][tT][nN][eE][sSſ][sSſ](?:[eE][sSſ])?`

var (
	titleRE        = regexp.MustCompile(`^#` + sp + `+(.+?)` + sp + `*#*` + sp + `*$`)
	phaseRE        = regexp.MustCompile(`^##` + sp + `+`)
	headingRE      = regexp.MustCompile(`^#{1,6}` + sp)
	checkboxRE     = regexp.MustCompile(`^(` + sp + `*)[-*+]` + sp + `+\[([ xX])\]` + sp + `+(.*)$`)
	bulletRE       = regexp.MustCompile(`^(` + sp + `*)(?:[-*+]|\p{Nd}+[.)])` + sp)
	witnessPattern = `\(` + sp + `*` + witnessWord + sp + `*:` + sp + `*([^)]*)\)`
	witnessRE      = regexp.MustCompile(witnessPattern)
	spacesRE       = regexp.MustCompile(sp + `+`)
	witnessStripRE = regexp.MustCompile(sp + `*` + witnessPattern)
)

type draft struct {
	line, indent, phase int
	parts               []string
	include             bool
}

// indentOf is len(line) - len(line.lstrip(" \t")) in code points.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

func fromMarkdown(path, text string, includeDone bool) (ImportedChecklist, map[string]int, error) {
	title := ""
	intro := []string{}
	phase := 0
	seenPhase := false
	var drafts []*draft
	var current *draft
	skipIndent := -1 // >= 0: inside an ignored nested (non-checkbox) bullet
	inFence := false

	for i, raw := range pySplitlines(text) {
		lineno := i + 1
		line := pyRstrip(raw)
		stripped := pyStrip(line)
		indent := indentOf(line)
		if strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if skipIndent >= 0 {
			if stripped == "" || indent > skipIndent {
				continue
			}
			skipIndent = -1
		}
		if stripped == "" {
			if !seenPhase && current == nil {
				intro = append(intro, "")
			}
			continue
		}
		if phaseRE.MatchString(line) {
			phase, seenPhase, current = phase+1, true, nil
			continue
		}
		if title == "" && !seenPhase {
			if m := titleRE.FindStringSubmatch(line); m != nil {
				title = m[1]
				continue
			}
		}
		if headingRE.MatchString(line) {
			current = nil
			continue
		}
		if box := checkboxRE.FindStringSubmatch(line); box != nil {
			current = &draft{
				line: lineno, indent: runeLen(box[1]), phase: phase,
				parts: []string{box[3]}, include: box[2] == " " || includeDone,
			}
			drafts = append(drafts, current)
			continue
		}
		if current != nil && indent > current.indent {
			if bulletRE.MatchString(line) {
				skipIndent = indent
			} else {
				current.parts = append(current.parts, stripped)
			}
			continue
		}
		current = nil
		if bulletRE.MatchString(line) {
			skipIndent = indent
		} else if !seenPhase {
			intro = append(intro, stripped)
		}
	}

	included := []*draft{}
	for _, d := range drafts {
		if d.include {
			included = append(included, d)
		}
	}
	checklist, lines, err := buildItems(path, included)
	if err != nil {
		return ImportedChecklist{}, nil, err
	}
	paras := []string{}
	for _, p := range paragraphs(intro) {
		if len(p) > 0 {
			paras = append(paras, strings.Join(p, " "))
		}
	}
	if title == "" {
		title = pyStem(path)
	}
	return ImportedChecklist{
		Checklist:   checklist,
		Title:       title,
		Description: strings.Join(paras, "\n\n"),
		References:  []string{},
	}, lines, nil
}

func paragraphs(lines []string) [][]string {
	out := [][]string{{}}
	for _, line := range lines {
		last := len(out) - 1
		if line != "" {
			out[last] = append(out[last], line)
		} else if len(out[last]) > 0 {
			out = append(out, []string{})
		}
	}
	return out
}

// SplitWitnesses is python's split_witnesses: "Do X (witness: cmd:true, go:TestX)" ->
// ("Do X", ["cmd:true", "go:TestX"]). The description is the text with every (witness: ...) /
// (witnesses: ...) group removed and whitespace collapsed; the witnesses are the groups'
// comma-separated entries in order (the Markdown roadmap syntax, also used by lha mission-edit).
func SplitWitnesses(text string) (string, []string) {
	witnesses := []string{}
	for _, m := range witnessRE.FindAllStringSubmatch(text, -1) {
		for _, w := range strings.Split(m[1], ",") {
			if w = pyStrip(w); w != "" {
				witnesses = append(witnesses, w)
			}
		}
	}
	description := pyStrip(spacesRE.ReplaceAllLiteralString(witnessStripRE.ReplaceAllLiteralString(text, ""), " "))
	return description, witnesses
}

func buildItems(path string, drafts []*draft) (contracts.Checklist, map[string]int, error) {
	width := max(2, len(strconv.Itoa(len(drafts))))
	items := []contracts.ChecklistItem{}
	lines := map[string]int{}
	idsByPhase := map[int][]string{}
	previous := []string{} // ids of the nearest earlier phase that has items
	currentPhase, havePhase := 0, false
	for n, d := range drafts {
		if !havePhase || d.phase != currentPhase {
			if havePhase {
				previous = idsByPhase[currentPhase]
			}
			currentPhase, havePhase = d.phase, true
		}
		description, witnesses := SplitWitnesses(strings.Join(d.parts, " "))
		if description == "" {
			return contracts.Checklist{}, nil, importErr("%s:%d: checklist item has no description", path, d.line)
		}
		id := fmt.Sprintf("%0*d", width, n+1)
		idsByPhase[d.phase] = append(idsByPhase[d.phase], id)
		lines[id] = d.line
		item := contracts.NewChecklistItem(id, description, previous...)
		item.Witnesses = witnesses
		items = append(items, item)
	}
	return contracts.Checklist{Items: items, SchemaVersion: 1}, lines, nil
}

// --- fabric-emulator style witness manifests ------------------------------------------------

// WitnessesFromManifest reads a witnesses.json manifest ({claim_id: {"witnesses": [...]}})
// (python: witnesses_from_manifest). Keys starting with "_" (e.g. "_gated": witnesses that only
// pass in CI) are skipped. Entries are checked in file order, so the first bad entry reported
// matches Python.
func WitnessesFromManifest(path string) (map[string][]string, error) {
	p := pyPath(path)
	// Python catches (OSError, ValueError) here, which includes decode errors and a NUL path.
	text, osErr, err := readText(p)
	switch {
	case err != nil:
		return nil, importErr("%s: cannot read witness manifest: %s", p, err.Error())
	case osErr != "":
		return nil, importErr("%s: cannot read witness manifest: %s", p, osErr)
	}
	data, err := pyJSONLoads(text)
	if err != nil {
		return nil, importErr("%s: cannot read witness manifest: %s", p, err.Error())
	}
	obj, ok := data.(*pyDict)
	if !ok {
		return nil, importErr("%s: witness manifest must be a JSON object", p)
	}
	out := map[string][]string{}
	for _, key := range obj.keys {
		if strings.HasPrefix(key, "_") {
			continue
		}
		var witnesses any
		if entry, ok := obj.vals[key].(*pyDict); ok {
			witnesses, _ = entry.get("witnesses")
		}
		list, err := strList(p, witnesses, contracts.PyRepr(key)+".witnesses")
		if err != nil {
			return nil, err
		}
		out[key] = list
	}
	return out, nil
}
