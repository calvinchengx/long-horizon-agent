package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Test-harness integrity: the agent must not pass the gate by editing the gate. The harness
// snapshots hashes of test/config files at cycle start; any modified or deleted file yields a
// FAILING gating harness_integrity check (unless the item explicitly allows harness edits). New
// test files are always fine.

// HarnessDirs: any file under a directory with one of these names, at ANY depth.
var HarnessDirs = []string{"tests", "test"}

// HarnessRootFiles is repo-level config that decides what the test run means.
var HarnessRootFiles = []string{"pyproject.toml", "setup.cfg", ".coveragerc"}

// HarnessAnywhereFiles are test runner config / fixtures, protected wherever they live.
var HarnessAnywhereFiles = []string{"conftest.py", "tox.ini", "pytest.ini", "noxfile.py"}

// pytest's default discovery patterns, protected anywhere. Python's re.match with "$" also
// matches before a trailing newline; "\n?\z" reproduces that.
var testFileRE = regexp.MustCompile(`^(test_.*|.*_test)\.py\n?\z`)

var skipDirs = map[string]bool{
	".git": true, ".lha": true, ".venv": true, "venv": true, "node_modules": true,
	"__pycache__": true, ".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
	".tox": true, ".nox": true, "build": true, "dist": true,
}

// HarnessSnapshot maps each harness file (posix relpath) to its sha256 hex digest.
type HarnessSnapshot map[string]string

// IsHarnessFile reports whether the posix relpath rel is a protected test-harness file.
func IsHarnessFile(rel string) bool {
	parts := strings.Split(rel, "/")
	last := parts[len(parts)-1]
	for _, p := range parts[:len(parts)-1] {
		if slices.Contains(HarnessDirs, p) {
			return true
		}
	}
	return slices.Contains(HarnessRootFiles, rel) ||
		slices.Contains(HarnessAnywhereFiles, last) ||
		testFileRE.MatchString(last)
}

func sha256File(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SnapshotHarness maps each existing harness file under workdir to its sha256. Unreadable
// entries are skipped (like os.walk without onerror); symlinks are never followed or hashed.
func SnapshotHarness(workdir string) HarnessSnapshot {
	snapshot := HarnessSnapshot{}
	_ = filepath.WalkDir(workdir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != workdir {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != workdir && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() { // symlinks, sockets, ...
			return nil
		}
		rel, err := filepath.Rel(workdir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !IsHarnessFile(rel) {
			return nil
		}
		if digest, err := sha256File(path); err == nil {
			snapshot[rel] = digest
		}
		return nil
	})
	return snapshot
}

// HarnessViolations lists pre-existing harness files that were modified or deleted (sorted by
// path; new files are allowed).
func HarnessViolations(before, after HarnessSnapshot) []string {
	keys := make([]string, 0, len(before))
	for k := range before {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	violations := []string{}
	for _, rel := range keys {
		now, ok := after[rel]
		switch {
		case !ok:
			violations = append(violations, "deleted: "+rel)
		case now != before[rel]:
			violations = append(violations, "modified: "+rel)
		}
	}
	return violations
}

// ViolatedPaths returns the relpaths named in HarnessViolations output.
func ViolatedPaths(violations []string) []string {
	paths := []string{}
	for _, v := range violations {
		if _, rel, ok := strings.Cut(v, ": "); ok {
			paths = append(paths, rel)
		}
	}
	return paths
}

// IntegrityResult is a failing gating harness_integrity result describing violations.
func IntegrityResult(violations []string) contracts.CheckResult {
	return contracts.CheckResult{
		Name:     contracts.HarnessIntegrityCheck,
		Passed:   false,
		ExitCode: 1,
		Gating:   true,
		OutputTail: "Pre-existing test-harness files were changed; this is not allowed for this item " +
			"(the changes were reverted):\n" + strings.Join(violations, "\n"),
	}
}
