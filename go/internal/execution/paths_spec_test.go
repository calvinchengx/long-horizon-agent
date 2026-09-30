package execution

import "testing"

// The cases of spec/execution/paths.json, repeated here so the package's own tests pin them (the mutation
// audit runs only these; internal/spec runs the JSON itself).

func TestSpecPaths(t *testing.T) {
	// want is the result, or the error message when fails is set.
	check := func(what string, got string, err error, want string, fails bool) {
		t.Helper()
		switch {
		case fails && (err == nil || err.Error() != want):
			t.Errorf("%s error = %v, want %q", what, err, want)
		case !fails && (err != nil || got != want):
			t.Errorf("%s = %q, %v; want %q", what, got, err, want)
		}
	}
	for _, c := range []struct {
		path, want string
		fails      bool
	}{
		{"", ".", false},
		{".", ".", false},
		{"./", ".", false},
		{"a", "a", false},
		{"a/./b/../c.txt", "a/c.txt", false},
		{"a//b/", "a/b", false},
		{"src\\pkg\\x.py", "src/pkg/x.py", false},
		{"../x", "path escapes the workspace: '../x'", true},
		{"..", "path escapes the workspace: '..'", true},
		{"a/../../x", "path escapes the workspace: 'a/../../x'", true},
		{"a/b/../../..", "path escapes the workspace: 'a/b/../../..'", true},
		{"a/..", ".", false},
		{"/etc/passwd", "absolute paths are not allowed: '/etc/passwd'", true},
		{"\\\\srv\\share", "absolute paths are not allowed: '\\\\\\\\srv\\\\share'", true},
		{"C:\\x", "absolute paths are not allowed: 'C:\\\\x'", true},
		{"C:x", "absolute paths are not allowed: 'C:x'", true},
		{"1:x", "absolute paths are not allowed: '1:x'", true},
		{"\u00e9:x", "absolute paths are not allowed: '\u00e9:x'", true},
		{"a:b", "absolute paths are not allowed: 'a:b'", true},
		{"a\u0000b", "path contains a NUL byte", true},
		{".lha/checklist.json", ".lha/checklist.json", false},
		{"./.git/hooks/pre-commit", ".git/hooks/pre-commit", false},
		{".GIT/config", ".GIT/config", false},
		{".Lha", ".Lha", false},
		{"a/../.git/config", ".git/config", false},
		{"src/.git_notes", "src/.git_notes", false},
		{".github/workflows/ci.yml", ".github/workflows/ci.yml", false},
		{".gitignore", ".gitignore", false},
		{"sub/.git/x", "sub/.git/x", false},
		{"'quoted'", "'quoted'", false},
		{"mixed\"'", "mixed\"'", false},
		{"tab\there", "tab\there", false},
		{"\u00fcn\u00efc\u00f6d\u00e9/\u0444\u0430\u0439\u043b.txt", "\u00fcn\u00efc\u00f6d\u00e9/\u0444\u0430\u0439\u043b.txt", false},
	} {
		got, err := NormalizeRelpath(c.path)
		check("NormalizeRelpath("+c.path+")", got, err, c.want, c.fails)
	}
	for _, c := range []struct {
		path      string
		protected bool
		err       string
	}{
		{"", false, ""},
		{".", false, ""},
		{"./", false, ""},
		{"a", false, ""},
		{"a/./b/../c.txt", false, ""},
		{"a//b/", false, ""},
		{"src\\pkg\\x.py", false, ""},
		{"../x", false, "path escapes the workspace: '../x'"},
		{"..", false, "path escapes the workspace: '..'"},
		{"a/../../x", false, "path escapes the workspace: 'a/../../x'"},
		{"a/b/../../..", false, "path escapes the workspace: 'a/b/../../..'"},
		{"a/..", false, ""},
		{"/etc/passwd", false, "absolute paths are not allowed: '/etc/passwd'"},
		{"\\\\srv\\share", false, "absolute paths are not allowed: '\\\\\\\\srv\\\\share'"},
		{"C:\\x", false, "absolute paths are not allowed: 'C:\\\\x'"},
		{"C:x", false, "absolute paths are not allowed: 'C:x'"},
		{"1:x", false, "absolute paths are not allowed: '1:x'"},
		{"\u00e9:x", false, "absolute paths are not allowed: '\u00e9:x'"},
		{"a:b", false, "absolute paths are not allowed: 'a:b'"},
		{"a\u0000b", false, "path contains a NUL byte"},
		{".lha/checklist.json", true, ""},
		{"./.git/hooks/pre-commit", true, ""},
		{".GIT/config", true, ""},
		{".Lha", true, ""},
		{"a/../.git/config", true, ""},
		{"src/.git_notes", false, ""},
		{".github/workflows/ci.yml", false, ""},
		{".gitignore", false, ""},
		{"sub/.git/x", false, ""},
		{"'quoted'", false, ""},
		{"mixed\"'", false, ""},
		{"tab\there", false, ""},
		{"\u00fcn\u00efc\u00f6d\u00e9/\u0444\u0430\u0439\u043b.txt", false, ""},
	} {
		got, err := IsProtected(c.path)
		switch {
		case c.err != "" && (err == nil || err.Error() != c.err):
			t.Errorf("IsProtected(%q) error = %v, want %q", c.path, err, c.err)
		case c.err == "" && (err != nil || got != c.protected):
			t.Errorf("IsProtected(%q) = %v, %v; want %v", c.path, got, err, c.protected)
		}
	}
	for _, c := range []struct {
		workdir, path, want string
		fails               bool
	}{
		{"/workspace", "", "/workspace", false},
		{"/home/user/workspace/", "", "/home/user/workspace/", false},
		{"/workspace", ".", "/workspace", false},
		{"/home/user/workspace/", ".", "/home/user/workspace/", false},
		{"/workspace", "./", "/workspace", false},
		{"/home/user/workspace/", "./", "/home/user/workspace/", false},
		{"/workspace", "a", "/workspace/a", false},
		{"/home/user/workspace/", "a", "/home/user/workspace/a", false},
		{"/workspace", "a/./b/../c.txt", "/workspace/a/c.txt", false},
		{"/home/user/workspace/", "a/./b/../c.txt", "/home/user/workspace/a/c.txt", false},
		{"/workspace", "a//b/", "/workspace/a/b", false},
		{"/home/user/workspace/", "a//b/", "/home/user/workspace/a/b", false},
		{"/workspace", "src\\pkg\\x.py", "/workspace/src/pkg/x.py", false},
		{"/home/user/workspace/", "src\\pkg\\x.py", "/home/user/workspace/src/pkg/x.py", false},
		{"/workspace", "../x", "path escapes the workspace: '../x'", true},
		{"/home/user/workspace/", "../x", "path escapes the workspace: '../x'", true},
		{"/workspace", "..", "path escapes the workspace: '..'", true},
		{"/home/user/workspace/", "..", "path escapes the workspace: '..'", true},
		{"/workspace", "a/../../x", "path escapes the workspace: 'a/../../x'", true},
		{"/home/user/workspace/", "a/../../x", "path escapes the workspace: 'a/../../x'", true},
		{"/workspace", "a/b/../../..", "path escapes the workspace: 'a/b/../../..'", true},
		{"/home/user/workspace/", "a/b/../../..", "path escapes the workspace: 'a/b/../../..'", true},
		{"/workspace", "a/..", "/workspace", false},
		{"/home/user/workspace/", "a/..", "/home/user/workspace/", false},
		{"/workspace", "/etc/passwd", "absolute paths are not allowed: '/etc/passwd'", true},
		{"/home/user/workspace/", "/etc/passwd", "absolute paths are not allowed: '/etc/passwd'", true},
		{"/workspace", "\\\\srv\\share", "absolute paths are not allowed: '\\\\\\\\srv\\\\share'", true},
		{"/home/user/workspace/", "\\\\srv\\share", "absolute paths are not allowed: '\\\\\\\\srv\\\\share'", true},
		{"/workspace", "C:\\x", "absolute paths are not allowed: 'C:\\\\x'", true},
		{"/home/user/workspace/", "C:\\x", "absolute paths are not allowed: 'C:\\\\x'", true},
		{"/workspace", "C:x", "absolute paths are not allowed: 'C:x'", true},
		{"/home/user/workspace/", "C:x", "absolute paths are not allowed: 'C:x'", true},
		{"/workspace", "1:x", "absolute paths are not allowed: '1:x'", true},
		{"/home/user/workspace/", "1:x", "absolute paths are not allowed: '1:x'", true},
		{"/workspace", "\u00e9:x", "absolute paths are not allowed: '\u00e9:x'", true},
		{"/home/user/workspace/", "\u00e9:x", "absolute paths are not allowed: '\u00e9:x'", true},
		{"/workspace", "a:b", "absolute paths are not allowed: 'a:b'", true},
		{"/home/user/workspace/", "a:b", "absolute paths are not allowed: 'a:b'", true},
		{"/workspace", "a\u0000b", "path contains a NUL byte", true},
		{"/home/user/workspace/", "a\u0000b", "path contains a NUL byte", true},
		{"/workspace", ".lha/checklist.json", "/workspace/.lha/checklist.json", false},
		{"/home/user/workspace/", ".lha/checklist.json", "/home/user/workspace/.lha/checklist.json", false},
		{"/workspace", "./.git/hooks/pre-commit", "/workspace/.git/hooks/pre-commit", false},
		{"/home/user/workspace/", "./.git/hooks/pre-commit", "/home/user/workspace/.git/hooks/pre-commit", false},
		{"/workspace", ".GIT/config", "/workspace/.GIT/config", false},
		{"/home/user/workspace/", ".GIT/config", "/home/user/workspace/.GIT/config", false},
		{"/workspace", ".Lha", "/workspace/.Lha", false},
		{"/home/user/workspace/", ".Lha", "/home/user/workspace/.Lha", false},
		{"/workspace", "a/../.git/config", "/workspace/.git/config", false},
		{"/home/user/workspace/", "a/../.git/config", "/home/user/workspace/.git/config", false},
		{"/workspace", "src/.git_notes", "/workspace/src/.git_notes", false},
		{"/home/user/workspace/", "src/.git_notes", "/home/user/workspace/src/.git_notes", false},
		{"/workspace", ".github/workflows/ci.yml", "/workspace/.github/workflows/ci.yml", false},
		{"/home/user/workspace/", ".github/workflows/ci.yml", "/home/user/workspace/.github/workflows/ci.yml", false},
		{"/workspace", ".gitignore", "/workspace/.gitignore", false},
		{"/home/user/workspace/", ".gitignore", "/home/user/workspace/.gitignore", false},
		{"/workspace", "sub/.git/x", "/workspace/sub/.git/x", false},
		{"/home/user/workspace/", "sub/.git/x", "/home/user/workspace/sub/.git/x", false},
		{"/workspace", "'quoted'", "/workspace/'quoted'", false},
		{"/home/user/workspace/", "'quoted'", "/home/user/workspace/'quoted'", false},
		{"/workspace", "mixed\"'", "/workspace/mixed\"'", false},
		{"/home/user/workspace/", "mixed\"'", "/home/user/workspace/mixed\"'", false},
		{"/workspace", "tab\there", "/workspace/tab\there", false},
		{"/home/user/workspace/", "tab\there", "/home/user/workspace/tab\there", false},
		{"/workspace", "\u00fcn\u00efc\u00f6d\u00e9/\u0444\u0430\u0439\u043b.txt", "/workspace/\u00fcn\u00efc\u00f6d\u00e9/\u0444\u0430\u0439\u043b.txt", false},
		{"/home/user/workspace/", "\u00fcn\u00efc\u00f6d\u00e9/\u0444\u0430\u0439\u043b.txt", "/home/user/workspace/\u00fcn\u00efc\u00f6d\u00e9/\u0444\u0430\u0439\u043b.txt", false},
	} {
		got, err := ContainedPosix(c.workdir, c.path)
		check("ContainedPosix("+c.workdir+", "+c.path+")", got, err, c.want, c.fails)
	}
}
