package execution

import (
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// Workspace path containment — the one place every sandbox and fs tool resolves agent paths
// (python/src/lha/execution/paths.py).
//
// Agent-supplied paths are untrusted. Every path the model names must stay inside the session's
// workspace root, so this file:
//
//   - rejects empty-anchor tricks: absolute paths, drive letters, NUL bytes;
//   - rejects lexical escapes (../x, a/../../x) before touching the filesystem;
//   - resolves symlinks on the host (Realpath, os.path.realpath semantics) and re-checks
//     containment, so a symlink planted in the workspace cannot point reads/writes outside it;
//   - re-checks the opened file descriptor against the path after open (O_NOFOLLOW + inode
//     comparison), narrowing the resolve->open race window.
//
// NormalizeRelpath is purely lexical (usable for container/VM paths the host cannot resolve);
// ResolveWithin / ReadTextWithin / WriteTextWithin are for host-local workspaces.

// ProtectedDirs are the top-level workspace dirs owned by the harness: the agent must never
// mutate them directly.
var ProtectedDirs = []string{".git", ".lha"}

// PathEscapeError is returned when an agent-supplied path would leave the workspace root. It is
// Python's PathEscapeError (a PermissionError): Error() is the exact str(exc).
type PathEscapeError struct{ Msg string }

func (e *PathEscapeError) Error() string { return e.Msg }

// PyTypeName names the Python exception type.
func (e *PathEscapeError) PyTypeName() string { return "PathEscapeError" }

func escapeErr(msg string) error { return &PathEscapeError{Msg: msg} }

func isADirectory(msg string) error { return pyval.NewError("IsADirectoryError", msg) }

func hasWindowsDrive(relpath string) bool {
	// PureWindowsPath(relpath).drive: a second character ':' ("C:x", "1:x") or a UNC prefix
	// (which also starts with a separator and is caught by the caller).
	_, size := utf8.DecodeRuneInString(relpath)
	return size < len(relpath) && relpath[size] == ':'
}

// NormalizeRelpath lexically validates relpath and returns it normalized ("." for the root).
// It returns a *PathEscapeError for absolute paths, drive-qualified paths, NUL bytes, or any path
// whose normalized form climbs above the root.
func NormalizeRelpath(relpath string) (string, error) {
	if strings.Contains(relpath, "\x00") {
		return "", escapeErr("path contains a NUL byte")
	}
	unified := strings.ReplaceAll(relpath, "\\", "/")
	if strings.HasPrefix(unified, "/") || hasWindowsDrive(relpath) {
		return "", escapeErr("absolute paths are not allowed: " + contracts.PyRepr(relpath))
	}
	normalized := "."
	if unified != "" {
		normalized = path.Clean(unified) // posixpath.normpath for a relative path
	}
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", escapeErr("path escapes the workspace: " + contracts.PyRepr(relpath))
	}
	return normalized, nil
}

func firstPart(normalized string) string {
	if normalized == "." {
		return ""
	}
	first, _, _ := strings.Cut(normalized, "/")
	return first
}

func isProtectedPart(part string) bool {
	folded := pystr.Casefold(part)
	for _, d := range ProtectedDirs {
		if folded == d {
			return true
		}
	}
	return false
}

// IsProtected reports whether relpath (lexically) targets a harness-owned dir such as .lha/ or
// .git/ (case-insensitive, because the default macOS/Windows filesystems are).
func IsProtected(relpath string) (bool, error) {
	normalized, err := NormalizeRelpath(relpath)
	if err != nil {
		return false, err
	}
	return isProtectedPart(firstPart(normalized)), nil
}

// ContainedPosix joins a validated relpath onto a container/VM workdir (lexical containment
// only).
func ContainedPosix(workdir, relpath string) (string, error) {
	rel, err := NormalizeRelpath(relpath)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return workdir, nil
	}
	return posixJoin(pureWorkdir(workdir), rel), nil
}

// pureWorkdir is str(PurePosixPath(workdir)): collapsed slashes, no trailing slash, "." parts
// dropped ("..": kept).
func pureWorkdir(workdir string) string {
	if workdir == "" {
		return "."
	}
	abs := strings.HasPrefix(workdir, "/")
	var parts []string
	for _, p := range strings.Split(workdir, "/") {
		if p != "" && p != "." {
			parts = append(parts, p)
		}
	}
	out := strings.Join(parts, "/")
	if abs {
		if strings.HasPrefix(workdir, "//") && !strings.HasPrefix(workdir, "///") {
			return "//" + out
		}
		return "/" + out
	}
	if out == "" {
		return "."
	}
	return out
}

// posixJoin is posixpath.join(a, b).
func posixJoin(a, b string) string {
	switch {
	case strings.HasPrefix(b, "/"):
		return b
	case a == "" || strings.HasSuffix(a, "/"):
		return a + b
	}
	return a + "/" + b
}

// posixSplit is posixpath.split(p).
func posixSplit(p string) (head, tail string) {
	i := strings.LastIndex(p, "/") + 1
	head, tail = p[:i], p[i:]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head, tail
}

// posixAbspath is posixpath.abspath(p).
func posixAbspath(p string) string {
	if !strings.HasPrefix(p, "/") {
		cwd, err := os.Getwd()
		if err == nil {
			p = posixJoin(cwd, p)
		}
	}
	return posixNormpath(p)
}

// posixNormpath is posixpath.normpath(p).
func posixNormpath(p string) string {
	if p == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(p, "/") {
		initial = 1
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if c != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, c)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	out := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if out == "" {
		return "."
	}
	return out
}

// Realpath is os.path.realpath(p) (non-strict): symlinks are resolved component by component,
// missing components are appended lexically, and a symlink loop leaves the rest unresolved.
func Realpath(p string) string {
	resolved, _ := joinRealpath("", p, map[string]*string{})
	return posixAbspath(resolved)
}

func joinRealpath(base, rest string, seen map[string]*string) (string, bool) {
	p := base
	if strings.HasPrefix(rest, "/") {
		rest = rest[1:]
		p = "/"
	}
	for rest != "" {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			if p != "" {
				p, name = posixSplit(p)
				if name == ".." {
					p = posixJoin(posixJoin(p, ".."), "..")
				}
			} else {
				p = ".."
			}
			continue
		}
		newpath := posixJoin(p, name)
		st, err := os.Lstat(newpath)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			p = newpath
			continue
		}
		if cached, ok := seen[newpath]; ok {
			if cached != nil {
				p = *cached
				continue
			}
			return posixJoin(newpath, rest), false // a symlink loop
		}
		seen[newpath] = nil
		target, err := os.Readlink(newpath)
		if err != nil {
			p = newpath
			continue
		}
		var ok bool
		p, ok = joinRealpath(p, target, seen)
		if !ok {
			return posixJoin(p, rest), false
		}
		resolved := p
		seen[newpath] = &resolved
	}
	return p, true
}

// resolvePath is Path(p).resolve(): Realpath, then a symlink loop is an error (RuntimeError).
func resolvePath(p string) (string, error) {
	resolved := Realpath(p)
	if _, err := os.Stat(resolved); err != nil && errors.Is(err, syscall.ELOOP) {
		return "", pyval.NewError("RuntimeError", "Symlink loop from "+contracts.PyRepr(resolved))
	}
	return resolved, nil
}

// IsRelativeTo is PurePath.is_relative_to for two resolved absolute paths.
func IsRelativeTo(p, root string) bool {
	if p == root {
		return true
	}
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, root+"/")
}

// relParts are the components of p relative to root (IsRelativeTo must hold).
func relFirstPart(p, root string) string {
	rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
	if p == root {
		return ""
	}
	first, _, _ := strings.Cut(rel, "/")
	return first
}

// ResolveWithin resolves relpath under root following symlinks; it returns a *PathEscapeError
// if the result escapes root.
func ResolveWithin(root, relpath string) (string, error) {
	rel, err := NormalizeRelpath(relpath)
	if err != nil {
		return "", err
	}
	rootResolved, err := resolvePath(root)
	if err != nil {
		return "", err
	}
	joined := rootResolved
	if rel != "." {
		joined = posixJoin(rootResolved, rel)
	}
	candidate, err := resolvePath(joined)
	if err != nil {
		return "", err
	}
	if !IsRelativeTo(candidate, rootResolved) {
		return "", escapeErr("path escapes the workspace: " + contracts.PyRepr(relpath))
	}
	return candidate, nil
}

// IsProtectedResolved is IsProtected after symlink resolution (link -> .git is also protected).
func IsProtectedResolved(root, relpath string) (bool, error) {
	protected, err := IsProtected(relpath)
	if err != nil || protected {
		return protected, err
	}
	rootResolved, err := resolvePath(root)
	if err != nil {
		return false, err
	}
	resolved, err := ResolveWithin(root, relpath)
	if err != nil {
		return false, err
	}
	return isProtectedPart(relFirstPart(resolved, rootResolved)), nil
}

// verifyFD checks, after open, that f is a regular file that is still the contained path we
// resolved; then it restores blocking mode.
func verifyFD(root, relpath string, f *os.File, expected string) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return isADirectory("not a regular file: " + contracts.PyRepr(relpath))
	}
	again, err := ResolveWithin(root, relpath)
	if err != nil {
		return err
	}
	current, err := os.Stat(again)
	if err != nil {
		return escapeErr("path changed during open: " + contracts.PyRepr(relpath))
	}
	if again != expected || !os.SameFile(current, st) {
		return escapeErr("path changed during open: " + contracts.PyRepr(relpath))
	}
	return setBlocking(f)
}

// ReadTextWithin reads the UTF-8 file at relpath inside root (containment checked before and
// after open). Invalid UTF-8 is a *pyval.UnicodeDecodeError-typed error.
func ReadTextWithin(root, relpath string) (string, error) {
	target, err := ResolveWithin(root, relpath)
	if err != nil {
		return "", err
	}
	f, err := openNoFollow(target, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	rootResolved, err := resolvePath(root)
	if err != nil {
		return "", err
	}
	if err := verifyFD(rootResolved, relpath, f, target); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return pyval.DecodeUTF8(data)
}

// mkdirParents is Path.mkdir(parents=True, exist_ok=True).
func mkdirParents(dir string) error {
	err := os.Mkdir(dir, 0o777)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENOENT) {
		parent, _ := posixSplit(dir)
		if parent == dir || parent == "" {
			return err
		}
		if err := mkdirParents(parent); err != nil {
			return err
		}
		err = os.Mkdir(dir, 0o777)
		if err == nil {
			return nil
		}
	}
	if st, statErr := os.Stat(dir); statErr == nil && st.IsDir() {
		return nil
	}
	return err
}

// WriteTextWithin writes UTF-8 content to relpath inside root, creating contained parents, and
// returns the written path.
func WriteTextWithin(root, relpath, content string) (string, error) {
	rootResolved, err := resolvePath(root)
	if err != nil {
		return "", err
	}
	target, err := ResolveWithin(root, relpath)
	if err != nil {
		return "", err
	}
	if target == rootResolved {
		return "", isADirectory("cannot write to the workspace root: " + contracts.PyRepr(relpath))
	}
	parent, _ := posixSplit(target)
	if err := mkdirParents(parent); err != nil {
		return "", err
	}
	// Re-resolve after mkdir: a racing symlink swap of a parent must not redirect the write.
	target, err = ResolveWithin(root, relpath)
	if err != nil {
		return "", err
	}
	f, err := openNoFollow(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) { // a FIFO with no reader (O_NONBLOCK)
			return "", isADirectory("not a regular file: " + contracts.PyRepr(relpath))
		}
		return "", err
	}
	defer f.Close()
	if err := verifyFD(rootResolved, relpath, f, target); err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		return "", err
	}
	return target, nil
}
