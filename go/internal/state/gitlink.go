package state

// gitlink.go mirrors python/src/lha/state/git_link.py: validate a linked work tree's .git FILE
// ("gitdir: <path>") before trusting it.
//
// In a linked git worktree .git is not a directory but a one-line file pointing at the work
// tree's private git dir, <common>/worktrees/<name>, whose commondir file points back at the
// shared repository. Whoever can rewrite that file decides which repository (which config, which
// hooks) the host's next `git add`/`git commit` in that work tree uses. The Docker sandbox mounts
// it read-only, and the host re-validates it here before every harness git invocation, failing
// closed on anything unexpected.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxPointerBytes = 4096

// GitLinkError is a .git pointer that is malformed, dangling, or points somewhere it must not.
type GitLinkError struct{ Message string }

func (e *GitLinkError) Error() string { return e.Message }

// GitLink is a parsed .git file: the work tree's git dir and the repository's common dir.
type GitLink struct {
	GitDir    string // resolved: <common>/worktrees/<name> (or a submodule's modules/<name>)
	CommonDir string // resolved: the shared repository (== GitDir for a submodule)
}

func linkErr(format string, a ...any) error { return &GitLinkError{fmt.Sprintf(format, a...)} }

func resolveRel(base, value string) string {
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	return resolvePath(value)
}

// isWithin reports whether path is root or lies below it (both already resolved).
func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func readTrimmed(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// ReadGitPointer returns the resolved target of <worktree>/.git (a "gitdir:" file).
func ReadGitPointer(worktree string) (string, error) {
	root := resolvePath(worktree)
	dotgit := filepath.Join(root, ".git")
	f, err := os.Open(dotgit)
	if err != nil {
		return "", linkErr("%s: unreadable (%v)", dotgit, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPointerBytes+1))
	if err != nil {
		return "", linkErr("%s: unreadable (%v)", dotgit, err)
	}
	if len(raw) > maxPointerBytes {
		return "", linkErr("%s: not a gitdir pointer (too large)", dotgit)
	}
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "gitdir: ") {
		return "", linkErr("%s: not a single 'gitdir: <path>' line", dotgit)
	}
	value := strings.TrimSpace(strings.TrimPrefix(lines[0], "gitdir: "))
	if value == "" {
		return "", linkErr("%s: empty gitdir pointer", dotgit)
	}
	return resolveRel(root, value), nil
}

// ValidateGitLink checks <worktree>/.git (a FILE) points at a genuine git dir outside the work
// tree. Refused (*GitLinkError): a malformed pointer; a target that is not a directory; a linked
// worktree git dir whose commondir does not lead to a repository containing it under worktrees/,
// or whose gitdir back-link is not this .git; a git dir or common dir INSIDE the work tree (the
// part a sandboxed process can write); and, with expectedCommonDir != "", any other repository.
func ValidateGitLink(worktree, expectedCommonDir string) (GitLink, error) {
	root := resolvePath(worktree)
	dotgit := filepath.Join(root, ".git")
	target, err := ReadGitPointer(root)
	if err != nil {
		return GitLink{}, err
	}
	if !isDir(target) {
		return GitLink{}, linkErr("%s: gitdir %s is not a directory", dotgit, target)
	}
	common := target
	if !isFile(filepath.Join(target, "commondir")) {
		// A submodule-style separate git dir: a whole repository of its own. Git records the
		// member it belongs to as core.worktree; the pointer must lead back to this work tree.
		if setting, found := coreWorktree(target); found && resolveRel(target, setting) != root {
			return GitLink{}, linkErr("%s: gitdir %s does not link back to this work tree", dotgit, target)
		} else if !found && filepath.Base(filepath.Dir(target)) == "modules" {
			return GitLink{}, linkErr("%s: gitdir %s does not link back to this work tree", dotgit, target)
		}
		// The enclosing repository's own .git (a member re-pointed at its workspace): git would
		// treat this directory as that repository's work tree and rewrite its index.
		if filepath.Base(target) == ".git" && isWithin(root, filepath.Dir(target)) && root != filepath.Dir(target) {
			return GitLink{}, linkErr("%s: gitdir %s is the enclosing repository", dotgit, target)
		}
	}
	if isFile(filepath.Join(target, "commondir")) {
		value, err := readTrimmed(filepath.Join(target, "commondir"))
		if err != nil {
			return GitLink{}, linkErr("%s: unreadable commondir (%v)", dotgit, err)
		}
		common = resolveRel(target, value)
		if filepath.Dir(target) != filepath.Join(common, "worktrees") {
			return GitLink{}, linkErr("%s: gitdir %s is not a worktree of %s", dotgit, target, common)
		}
		backlink := ""
		if value, err := readTrimmed(filepath.Join(target, "gitdir")); err == nil {
			backlink = resolveRel(target, value)
		}
		if backlink != dotgit {
			return GitLink{}, linkErr("%s: gitdir %s does not link back to this work tree", dotgit, target)
		}
	}
	if !isFile(filepath.Join(common, "HEAD")) || !isDir(filepath.Join(common, "objects")) {
		return GitLink{}, linkErr("%s: %s is not a git repository", dotgit, common)
	}
	for _, p := range []string{target, common} {
		if isWithin(p, root) {
			return GitLink{}, linkErr("%s: gitdir %s lies inside the work tree", dotgit, p)
		}
	}
	if expectedCommonDir != "" {
		if want := resolvePath(expectedCommonDir); common != want {
			return GitLink{}, linkErr("%s: points at repository %s, expected %s", dotgit, common, want)
		}
	}
	return GitLink{GitDir: target, CommonDir: common}, nil
}

var coreWorktreeRE = regexp.MustCompile(`^\s*worktree\s*=\s*(.*?)\s*$`)

// coreWorktree is core.worktree from <gitDir>/config (git sets it on a submodule's git dir,
// pointing back at the member); found is false when the file has none.
func coreWorktree(gitDir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return "", false
	}
	section := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		if section == "core" {
			if m := coreWorktreeRE.FindStringSubmatch(raw); m != nil {
				return strings.Trim(m[1], "\""), true
			}
		}
	}
	return "", false
}

// CheckGitLinkPath validates <worktree>/.git when it is a file or a symlink; a directory (or
// none) passes. A symlinked .git is refused outright. With expectedCommonDir a .git DIRECTORY
// must also be that repository.
func CheckGitLinkPath(worktree, expectedCommonDir string) error {
	dotgit := filepath.Join(worktree, ".git")
	st, err := os.Lstat(dotgit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return linkErr("%s: %v", dotgit, err)
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		return linkErr("%s: is a symlink", dotgit)
	case st.Mode().IsRegular():
		_, err := ValidateGitLink(worktree, expectedCommonDir)
		return err
	case st.IsDir() && expectedCommonDir != "":
		if got, want := resolvePath(dotgit), resolvePath(expectedCommonDir); got != want {
			return linkErr("%s: expected repository %s", dotgit, want)
		}
	}
	return nil
}

// GitDirsInTree returns the git dir / common dir a .git FILE points at when they lie inside
// worktree (best effort, never fails): the sandbox binds those read-only on top of the writable
// work tree. The host refuses such a layout anyway (ValidateGitLink).
func GitDirsInTree(worktree string) []string {
	root := resolvePath(worktree)
	dotgit := filepath.Join(root, ".git")
	st, err := os.Lstat(dotgit)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	target, err := ReadGitPointer(root)
	if err != nil {
		return nil
	}
	found := []string{target}
	if value, err := readTrimmed(filepath.Join(target, "commondir")); err == nil {
		found = append(found, resolveRel(target, value))
	}
	var inside []string
	for _, p := range found {
		if p != root && isWithin(p, root) && isDir(p) && !containsStr(inside, p) {
			inside = append(inside, p)
		}
	}
	return inside
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
