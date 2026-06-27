package verify

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func write(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHarnessFilesProtectedAnywhere(t *testing.T) {
	for _, rel := range []string{
		"tests/test_a.py", "src/pkg/tests/test_a.py", "src/pkg/test/helpers.py", "test_app.py",
		"pkg/test_models.py", "pkg/models_test.py", "pkg/conftest.py", "noxfile.py", "sub/tox.ini",
		"pytest.ini", "pyproject.toml", "setup.cfg",
	} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			path := write(t, root, rel, "def test_x():\n    assert True\n")
			before := SnapshotHarness(root)
			if _, ok := before[rel]; !ok {
				t.Fatalf("%s not in snapshot %v", rel, before)
			}
			if err := os.WriteFile(path, []byte("def test_x():\n    pass\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			got := HarnessViolations(before, SnapshotHarness(root))
			if !slices.Equal(got, []string{"modified: " + rel}) {
				t.Fatal(got)
			}
		})
	}
}

func TestNonHarnessFilesNotProtected(t *testing.T) {
	for _, rel := range []string{"src/pkg/core.py", "src/pkg/testing_utils.py", "README.md"} {
		root := t.TempDir()
		write(t, root, rel, "x = 1\n")
		if _, ok := SnapshotHarness(root)[rel]; ok {
			t.Fatalf("%s protected", rel)
		}
	}
}

func TestSnapshotSkipsToolDirsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".venv/lib/tests/test_x.py", "x")
	write(t, root, "node_modules/p/test_x.py", "x")
	write(t, root, "a/build/test_x.py", "x")
	real := write(t, root, "tests/test_real.py", "x")
	if err := os.Symlink(real, filepath.Join(root, "tests", "test_link.py")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "tests"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	snap := SnapshotHarness(root)
	keys := []string{}
	for k := range snap {
		keys = append(keys, k)
	}
	if !slices.Equal(keys, []string{"tests/test_real.py"}) {
		t.Fatal(keys)
	}
	if len(snap["tests/test_real.py"]) != 64 {
		t.Fatal(snap)
	}
	if len(SnapshotHarness(filepath.Join(root, "missing"))) != 0 {
		t.Fatal("missing root")
	}
}

func TestViolationsDeletedSortedAndNewAllowed(t *testing.T) {
	before := HarnessSnapshot{"tests/b.py": "1", "tests/a.py": "1", "conftest.py": "1"}
	after := HarnessSnapshot{"tests/b.py": "2", "tests/new.py": "9", "conftest.py": "1"}
	got := HarnessViolations(before, after)
	if !slices.Equal(got, []string{"deleted: tests/a.py", "modified: tests/b.py"}) {
		t.Fatal(got)
	}
	if paths := ViolatedPaths(append(got, "garbage")); !slices.Equal(paths, []string{"tests/a.py", "tests/b.py"}) {
		t.Fatal(paths)
	}
	r := IntegrityResult(got)
	if r.Name != contracts.HarnessIntegrityCheck || r.Passed || r.ExitCode != 1 || !r.Gating ||
		!strings.HasSuffix(r.OutputTail, "(the changes were reverted):\ndeleted: tests/a.py\nmodified: tests/b.py") {
		t.Fatalf("%+v", r)
	}
}

func TestIsHarnessFileTrailingNewlineLikePythonDollar(t *testing.T) {
	if !IsHarnessFile("test_a.py\n") || IsHarnessFile("test_a.py\n\n") || IsHarnessFile("test\n_a.py") {
		t.Fatal("regex anchoring differs from Python re.match(...$)")
	}
}
