package agents

import (
	"fmt"
	"path"
	"strings"

	"golang.org/x/text/cases"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// The part of python/src/lha/coordination/ownership.py the Planner needs: path normalization,
// the shared-file rule and the single-writer ownership map.

// Lead is the writer id of the serial lead engineer (owns all unassigned space).
const Lead = "lead"

var sharedBasenames = map[string]bool{
	"pyproject.toml": true, "setup.py": true, "setup.cfg": true, "uv.lock": true,
	"poetry.lock": true, "__init__.py": true, "conftest.py": true, "settings.py": true,
	"package.json": true, "package-lock.json": true, "pnpm-lock.yaml": true, "yarn.lock": true,
	"go.mod": true, "go.sum": true, "cargo.toml": true, "cargo.lock": true,
}

// InvalidPathError is a path that is absolute or escapes the repository root.
type InvalidPathError struct{ Message string }

func (e *InvalidPathError) Error() string { return e.Message }

var folder = cases.Fold()

// casefold is Python's str.casefold.
func casefold(s string) string { return folder.String(s) }

// NormalizePath normalizes a repo-relative path lexically (PurePosixPath semantics): backslashes
// become "/", "." segments and redundant slashes are dropped, ".." is resolved against the
// preceding segment. Absolute paths and paths that climb above the root are errors.
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
	norm = casefold(norm)
	base := path.Base(norm)
	if i := strings.LastIndex(norm, "/"); i >= 0 {
		base = norm[i+1:]
	}
	return sharedBasenames[base] || strings.HasSuffix(base, ".lock") ||
		(strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")) ||
		strings.Contains("/"+norm, "/migrations/"), nil
}

// FileOwnershipMap maps normalized, case-folded file paths to their single permitted writer.
type FileOwnershipMap struct {
	Owners map[string]string `json:"owners"`
}

// NewFileOwnershipMap returns an empty map.
func NewFileOwnershipMap() *FileOwnershipMap { return &FileOwnershipMap{Owners: map[string]string{}} }

// Assign gives p to writer. Shared files can only belong to the lead, and a file another writer
// owns is never silently stolen.
func (m *FileOwnershipMap) Assign(p, writer string) error {
	norm, err := NormalizePath(p)
	if err != nil {
		return err
	}
	if shared, _ := IsShared(norm); shared && writer != Lead {
		return fmt.Errorf("shared file %s can only be owned by the lead", contracts.PyRepr(norm))
	}
	key := casefold(norm)
	if current, ok := m.Owners[key]; ok && current != writer {
		return fmt.Errorf("%s is already owned by %s; refusing to hand it to %s (use reassign)",
			contracts.PyRepr(norm), contracts.PyRepr(current), contracts.PyRepr(writer))
	}
	m.Owners[key] = writer
	return nil
}

// WriterForItem is the writer id of the implementer that owns checklist item itemID's write-set.
func WriterForItem(itemID string) string { return "implementer-" + itemID }
