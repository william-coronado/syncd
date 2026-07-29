package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// defaultIgnorePatterns are always active, whether or not a user-supplied
// .drivesyncignore exists. ".synclock" and "*.tmp" are DriveSync's own
// protocol filenames (the remote canary and atomic-write staging files);
// "#recycle" and ".venv" are Synology recycle bins and Python virtualenvs,
// neither of which should ever be synced.
var defaultIgnorePatterns = []string{
	".synclock",
	"*.tmp",
	"#recycle",
	".venv",
}

const ignoreFileName = ".drivesyncignore"

type ignoreRule struct {
	negate  bool
	dirOnly bool
	re      *regexp.Regexp
}

// IgnoreMatcher decides whether a scanned path should be excluded from sync,
// using gitignore-style patterns: the built-in defaults above, followed by
// any patterns from a .drivesyncignore file in the local root. Rules are
// evaluated in order and the last match wins, so a user pattern prefixed
// with "!" can re-include something a default (or earlier) pattern would
// otherwise exclude.
type IgnoreMatcher struct {
	rules []ignoreRule
}

// LoadIgnoreMatcher builds a matcher from the built-in defaults plus
// <localRoot>/.drivesyncignore, if present. The same matcher is applied to
// both the local and remote scans so ignore rules behave symmetrically
// regardless of which side a matching path actually exists on.
func LoadIgnoreMatcher(localRoot string) (*IgnoreMatcher, error) {
	m := &IgnoreMatcher{}
	for _, p := range defaultIgnorePatterns {
		if err := m.addPattern(p); err != nil {
			return nil, fmt.Errorf("internal default ignore pattern %q: %w", p, err)
		}
	}
	patterns, err := readIgnoreFile(filepath.Join(localRoot, ignoreFileName))
	if err != nil {
		return nil, err
	}
	for _, p := range patterns {
		if err := m.addPattern(p); err != nil {
			return nil, fmt.Errorf("invalid pattern %q in %s: %w", p, ignoreFileName, err)
		}
	}
	return m, nil
}

func readIgnoreFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, nil
}

// Match reports whether relPath (forward-slash separated and root-relative,
// as produced by NormalizeRelativePath) should be excluded from sync.
func (m *IgnoreMatcher) Match(relPath string, isDir bool) bool {
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.re.MatchString(relPath) {
			ignored = !r.negate
		}
	}
	return ignored
}

func (m *IgnoreMatcher) addPattern(raw string) error {
	pattern := raw
	negate := false
	if strings.HasPrefix(pattern, "!") {
		negate = true
		pattern = pattern[1:]
	}
	if strings.HasPrefix(pattern, `\#`) || strings.HasPrefix(pattern, `\!`) {
		pattern = pattern[1:]
	}
	dirOnly := strings.HasSuffix(pattern, "/")
	if dirOnly {
		pattern = strings.TrimSuffix(pattern, "/")
	}
	anchored := strings.Contains(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	if pattern == "" {
		return fmt.Errorf("empty pattern")
	}
	re, err := globToRegexp(pattern, anchored)
	if err != nil {
		return err
	}
	m.rules = append(m.rules, ignoreRule{negate: negate, dirOnly: dirOnly, re: re})
	return nil
}

// globToRegexp translates a single gitignore-style glob into a regexp
// anchored to a full match against a "/"-separated relative path. Unanchored
// patterns (no "/" in the original) match the basename at any depth;
// anchored patterns (containing "/") match the full path from root. Supports
// "*" (any run of non-"/" characters), "**" (any run of path segments), and
// "?" (any single non-"/" character).
func globToRegexp(pattern string, anchored bool) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; {
		case c == '*' && i+1 < len(runes) && runes[i+1] == '*':
			i++
			if i+1 < len(runes) && runes[i+1] == '/' {
				b.WriteString("(?:.*/)?")
				i++
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
