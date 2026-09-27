// Package coordination is how agents hand off work and avoid stepping on each other (python:
// lha.coordination): the single-writer file-ownership map and its enforcement (a tool-call guard
// plus the git-layer check), lease granting, typed tickets and the blackboard.
//
// Agents coordinate through typed artifacts, not chatter. Work flows down as a TaskContract (a
// Ticket); writes are kept conflict-free by a FileOwnershipMap enforced at the tool call and again
// at the git layer (declare-then-enforce).
package coordination

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"golang.org/x/text/cases"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// File-ownership map — conflict prevention by single-writer-per-file (python:
// lha.coordination.ownership). The Planner assigns each file to one writer; the integration line
// ("lead") owns all unassigned space. Ambiguous/shared files (build manifests, lockfiles,
// __init__.py, conftest, settings, migrations) are ALWAYS the lead's.

// Lead is the writer id of the serial lead engineer (owns all unassigned space).
const Lead = "lead"

var sharedBasenames = map[string]bool{
	// Python
	"pyproject.toml": true, "setup.py": true, "setup.cfg": true, "uv.lock": true,
	"poetry.lock": true, "__init__.py": true, "conftest.py": true, "settings.py": true,
	// JavaScript / TypeScript
	"package.json": true, "package-lock.json": true, "pnpm-lock.yaml": true, "yarn.lock": true,
	// Go / Rust
	"go.mod": true, "go.sum": true, "cargo.toml": true, "cargo.lock": true,
}

// InvalidPathError is a path that is absolute or escapes the repository root.
type InvalidPathError struct{ Message string }

func (e *InvalidPathError) Error() string { return e.Message }

// OwnershipConflictError is Assign being asked to hand a file another writer owns to a new one.
type OwnershipConflictError struct{ Message string }

func (e *OwnershipConflictError) Error() string { return e.Message }

var folder = cases.Fold()

// Casefold is Python's str.casefold.
func Casefold(s string) string { return folder.String(s) }

// NormalizePath normalizes a repo-relative path lexically (PurePosixPath semantics): backslashes
// become "/", "." segments and redundant slashes are dropped, ".." is resolved against the
// preceding segment. Leading dots in names are kept. Absolute paths and paths that climb above
// the root are an *InvalidPathError.
func NormalizePath(p string) (string, error) {
	posix := strings.ReplaceAll(p, `\`, "/")
	if strings.HasPrefix(posix, "/") {
		return "", &InvalidPathError{fmt.Sprintf("path must be repo-relative, got %s", contracts.PyRepr(p))}
	}
	parts := []string{}
	for _, part := range strings.Split(posix, "/") {
		switch part {
		case "", ".":
		case "..":
			if len(parts) == 0 {
				return "", &InvalidPathError{fmt.Sprintf("path escapes the repository root: %s", contracts.PyRepr(p))}
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "", &InvalidPathError{fmt.Sprintf("path names no file: %s", contracts.PyRepr(p))}
	}
	return strings.Join(parts, "/"), nil
}

// IsShared reports whether p is a shared/ambiguous file that must stay on the serial lead thread
// (case-insensitive). An invalid path is an error.
func IsShared(p string) (bool, error) {
	norm, err := NormalizePath(p)
	if err != nil {
		return false, err
	}
	return isSharedNorm(norm), nil
}

func isSharedNorm(norm string) bool {
	norm = Casefold(norm)
	base := norm
	if i := strings.LastIndex(norm, "/"); i >= 0 {
		base = norm[i+1:]
	}
	return sharedBasenames[base] || strings.HasSuffix(base, ".lock") ||
		(strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")) ||
		strings.Contains("/"+norm, "/migrations/")
}

// OwnershipViolation is a write to a path the writer does not own. Owner is "" when the path is
// unassigned (python: None).
type OwnershipViolation struct {
	Path   string
	Writer string
	Owner  string
}

// LeaseRequest is a writer's request to (temporarily) own a file outside its write-set.
type LeaseRequest struct {
	Writer string `json:"writer"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// FileOwnershipMap maps normalized, case-folded file paths to their single permitted writer. Its
// JSON form is the pydantic model's: {"owners": {path: writer}}, keys in assignment order (a
// Python dict's insertion order, so .lha/ownership.json is byte-identical). It is safe for
// concurrent use (a wave's implementers share one live map: a lease granted to one is seen by
// every guard); use it by pointer, and read Owners directly only when nothing else holds the map.
type FileOwnershipMap struct {
	mu     sync.RWMutex
	Owners map[string]string `json:"owners"`
	// order is the keys in insertion order; keys set on Owners directly (not through Assign /
	// Reassign) are written after them, sorted.
	order []string
}

// NewFileOwnershipMap returns an empty map.
func NewFileOwnershipMap() *FileOwnershipMap { return &FileOwnershipMap{Owners: map[string]string{}} }

// Snapshot is a copy of the owners (path key -> writer).
func (m *FileOwnershipMap) Snapshot() map[string]string {
	out := map[string]string{}
	if m == nil {
		return out
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for k, v := range m.Owners {
		out[k] = v
	}
	return out
}

// Keys is the owned path keys in insertion order (Python's dict order).
func (m *FileOwnershipMap) Keys() []string {
	if m == nil {
		return []string{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.keysLocked()
}

func (m *FileOwnershipMap) keysLocked() []string {
	out := make([]string, 0, len(m.Owners))
	seen := make(map[string]bool, len(m.Owners))
	for _, k := range m.order {
		if _, ok := m.Owners[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	rest := []string{}
	for k := range m.Owners {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// setLocked stores key -> writer; a new key goes last (Python: owners[key] = writer).
func (m *FileOwnershipMap) setLocked(key, writer string) {
	if m.Owners == nil {
		m.Owners = map[string]string{}
	}
	if _, ok := m.Owners[key]; !ok {
		m.order = append(m.order, key)
	}
	m.Owners[key] = writer
}

// MarshalJSON never emits null owners, and writes them in insertion order.
func (m *FileOwnershipMap) MarshalJSON() ([]byte, error) {
	owners := contracts.NewOrderedMap()
	if m != nil {
		m.mu.RLock()
		for _, k := range m.keysLocked() {
			owners.Set(k, m.Owners[k])
		}
		m.mu.RUnlock()
	}
	return json.Marshal(contracts.NewOrderedMap("owners", owners))
}

// UnmarshalJSON accepts a missing owners key (pydantic default) and keeps the file's key order.
func (m *FileOwnershipMap) UnmarshalJSON(data []byte) error {
	var raw struct {
		Owners *contracts.OrderedMap `json:"owners"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	owners := map[string]string{}
	order := []string{}
	var bad error
	raw.Owners.Range(func(k string, v any) bool {
		s, ok := v.(string)
		if !ok {
			bad = fmt.Errorf("owners[%s]: not a string", contracts.PyRepr(k))
			return false
		}
		owners[k] = s
		order = append(order, k)
		return true
	})
	if bad != nil {
		return bad
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Owners = owners
	m.order = order
	return nil
}

// Clone is a deep copy (python: model_copy(deep=True)).
func (m *FileOwnershipMap) Clone() *FileOwnershipMap {
	out := NewFileOwnershipMap()
	if m == nil {
		return out
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, k := range m.keysLocked() {
		out.setLocked(k, m.Owners[k])
	}
	return out
}

// Equal reports whether two maps have the same owners.
func (m *FileOwnershipMap) Equal(other *FileOwnershipMap) bool {
	a, b := m.Snapshot(), other.Snapshot()
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func checkedKey(p, writer string) (string, error) {
	norm, err := NormalizePath(p)
	if err != nil {
		return "", err
	}
	if isSharedNorm(norm) && writer != Lead {
		return "", fmt.Errorf("shared file %s can only be owned by the lead", contracts.PyRepr(norm))
	}
	return Casefold(norm), nil
}

// Assign gives p to writer. Shared files can only belong to the lead; re-assigning a file to its
// current owner is a no-op, and a file another writer owns is never silently stolen
// (*OwnershipConflictError; use Reassign to transfer it explicitly).
func (m *FileOwnershipMap) Assign(p, writer string) error {
	key, err := checkedKey(p, writer)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.Owners[key]; ok && current != writer {
		norm, _ := NormalizePath(p)
		return &OwnershipConflictError{fmt.Sprintf("%s is already owned by %s; refusing to hand it to %s (use reassign)",
			contracts.PyRepr(norm), contracts.PyRepr(current), contracts.PyRepr(writer))}
	}
	m.setLocked(key, writer)
	return nil
}

// Reassign explicitly transfers p to writer and returns the previous owner ("" if none).
func (m *FileOwnershipMap) Reassign(p, writer string) (string, error) {
	key, err := checkedKey(p, writer)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous := m.Owners[key]
	m.setLocked(key, writer)
	return previous, nil
}

func (m *FileOwnershipMap) lookup(key string) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	owner, ok := m.Owners[key]
	return owner, ok
}

// OwnerOf is p's owner ("" when unassigned or invalid; python: None).
func (m *FileOwnershipMap) OwnerOf(p string) string {
	norm, err := NormalizePath(p)
	if err != nil {
		return ""
	}
	owner, _ := m.lookup(Casefold(norm))
	return owner
}

// Permits reports whether writer may write p under the single-writer-per-file invariant. Paths
// outside the repository (absolute, or escaping via "..") are never permitted.
func (m *FileOwnershipMap) Permits(writer, p string) bool {
	norm, err := NormalizePath(p)
	if err != nil {
		return false
	}
	if isSharedNorm(norm) {
		return writer == Lead
	}
	owner, ok := m.lookup(Casefold(norm))
	if !ok {
		return writer == Lead // unassigned space belongs to the serial lead
	}
	return writer == owner
}

// PermitsAny reports whether any of writers (one agent acting under several identities) may
// write p.
func (m *FileOwnershipMap) PermitsAny(writers []string, p string) bool {
	for _, w := range writers {
		if m.Permits(w, p) {
			return true
		}
	}
	return false
}

// Violations are the ownership violations for paths changed by writer.
func (m *FileOwnershipMap) Violations(writer string, paths []string) []OwnershipViolation {
	return m.ViolationsAny([]string{writer}, paths)
}

// ViolationsAny are the violations for paths changed by an agent acting as any of writers.
func (m *FileOwnershipMap) ViolationsAny(writers []string, paths []string) []OwnershipViolation {
	out := []OwnershipViolation{}
	for _, p := range paths {
		if m.PermitsAny(writers, p) {
			continue
		}
		shown, err := NormalizePath(p)
		if err != nil {
			shown = p
		}
		out = append(out, OwnershipViolation{Path: shown, Writer: strings.Join(writers, "+"), Owner: m.OwnerOf(p)})
	}
	return out
}

// WriteSet is the (normalized, case-folded) paths writer owns, sorted.
func (m *FileOwnershipMap) WriteSet(writer string) []string {
	out := []string{}
	for key, owner := range m.Snapshot() {
		if owner == writer {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Release explicitly returns every file writer owns to the lead's unassigned space (its slice is
// finished) and returns the released keys.
func (m *FileOwnershipMap) Release(writer string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	released := []string{}
	for key, owner := range m.Owners {
		if owner == writer {
			released = append(released, key)
		}
	}
	sort.Strings(released)
	for _, key := range released {
		delete(m.Owners, key)
	}
	kept := m.order[:0]
	for _, key := range m.order {
		if _, ok := m.Owners[key]; ok {
			kept = append(kept, key)
		}
	}
	m.order = kept
	return released
}

// WriterForItem is the writer id of the implementer that owns checklist item itemID's write-set.
func WriterForItem(itemID string) string { return "implementer-" + itemID }

// OwnershipJSON is the map as .lha/ownership.json holds it (pydantic model_dump_json(indent=2)).
func OwnershipJSON(m *FileOwnershipMap) ([]byte, error) {
	return state.PydanticJSON(m.Clone(), true)
}

// ReadOwnership is the committed file-ownership map (empty if the mission never declared one).
func ReadOwnership(ctx context.Context, anchor *state.GitMissionAnchor) (*FileOwnershipMap, error) {
	raw, ok, err := anchor.ReadOwnershipJSON(ctx)
	if err != nil {
		return nil, err
	}
	out := NewFileOwnershipMap()
	if !ok {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return nil, fmt.Errorf("%s/%s: %w", state.AnchorDir, state.OwnershipFile, err)
	}
	return out, nil
}

// StageOwnership writes m to .lha/ownership.json with the anchor's next checkpoint.
func StageOwnership(anchor *state.GitMissionAnchor, m *FileOwnershipMap) error {
	data, err := OwnershipJSON(m)
	if err != nil {
		return err
	}
	anchor.StageOwnershipJSON(data)
	return nil
}
