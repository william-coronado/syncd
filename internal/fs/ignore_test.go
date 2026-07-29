package fs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreMatcher_DefaultsExcludeProtocolAndKnownJunkPaths(t *testing.T) {
	root := t.TempDir()
	matcher := newTestMatcher(t, root)

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{".synclock", false, true},
		{"nested/.synclock", false, true},
		{"upload.tmp", false, true},
		{"sub/dir/partial.tmp", false, true},
		{"#recycle", true, true},
		{".venv", true, true},
		{"project/.venv", true, true},
		{"real.txt", false, false},
		{"src/main.go", false, false},
	}
	for _, tc := range cases {
		if got := matcher.Match(tc.path, tc.isDir); got != tc.want {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}
}

func TestIgnoreMatcher_UserPatternsFromDriveSyncIgnoreFile(t *testing.T) {
	root := t.TempDir()
	content := "# comment lines and blanks are skipped\n\nnode_modules\n/only-at-root\nbuild/\n"
	if err := os.WriteFile(filepath.Join(root, ignoreFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write ignore file: %v", err)
	}
	matcher := newTestMatcher(t, root)

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"pkg/node_modules", true, true}, // unanchored: matches at any depth
		{"only-at-root", false, true},
		{"nested/only-at-root", false, false}, // anchored (leading "/"): root only
		{"build", true, true},
		{"build", false, false}, // dirOnly: trailing "/" excludes matching a plain file
		{"src/app.go", false, false},
	}
	for _, tc := range cases {
		if got := matcher.Match(tc.path, tc.isDir); got != tc.want {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}
}

func TestIgnoreMatcher_NegationReincludesAPattern(t *testing.T) {
	root := t.TempDir()
	content := "*.log\n!important.log\n"
	if err := os.WriteFile(filepath.Join(root, ignoreFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write ignore file: %v", err)
	}
	matcher := newTestMatcher(t, root)

	if !matcher.Match("debug.log", false) {
		t.Error("expected debug.log to be ignored")
	}
	if matcher.Match("important.log", false) {
		t.Error("expected important.log to be re-included by negation pattern")
	}
}

func TestIgnoreMatcher_MissingDriveSyncIgnoreFileIsNotAnError(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadIgnoreMatcher(root); err != nil {
		t.Fatalf("LoadIgnoreMatcher with no ignore file present: %v", err)
	}
}

func TestIgnoreMatcher_DoubleStarMatchesAcrossPathSegments(t *testing.T) {
	root := t.TempDir()
	content := "cache/**/*.bin\n"
	if err := os.WriteFile(filepath.Join(root, ignoreFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write ignore file: %v", err)
	}
	matcher := newTestMatcher(t, root)

	if !matcher.Match("cache/a/b/file.bin", false) {
		t.Error("expected nested path under cache/ to match cache/**/*.bin")
	}
	if matcher.Match("cache/a/b/file.txt", false) {
		t.Error("did not expect a non-.bin file to match")
	}
}

func TestMacOSJunkExcludedByDefault(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadIgnoreMatcher(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/.DS_Store", "b/._resourcefork", ".Spotlight-V100/x"} {
		if !m.Match(p, false) {
			t.Errorf("expected %q to be ignored by default", p)
		}
	}
}

func TestReadsSyncdignoreFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".syncdignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadIgnoreMatcher(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match("build", true) {
		t.Error("expected build/ to be ignored from .syncdignore")
	}
}
