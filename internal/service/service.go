// Package service installs `syncd watch` as a per-user background service:
// a launchd agent on macOS, a systemd user unit on Linux. It only writes
// the service definition and drives launchctl/systemctl; the daemon itself
// is unchanged.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Spec describes one service to install.
type Spec struct {
	// Name distinguishes several services on one machine (one per root
	// pair); empty is the default, unnamed service.
	Name string
	// Binary is the absolute path of the syncd executable to run.
	Binary string
	// Args are syncd's arguments ("watch", "--local", ...).
	Args []string
	// Description is shown by systemctl/journalctl.
	Description string
	// LogDir receives the service's stdout/stderr on macOS (launchd has no
	// journal). Unused on Linux, where they go to the user journal.
	LogDir string
}

// Runner runs an external command and returns its combined output.
type Runner func(name string, args ...string) (string, error)

// ExecRunner is the Runner that really runs commands.
func ExecRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Manager installs, removes and inspects services for one platform.
type Manager struct {
	GOOS string // "darwin" or "linux"
	Home string // the user's home directory
	UID  int
	Run  Runner
}

// Result reports what Install did.
type Result struct {
	Path  string   // the service definition file written
	Notes []string // things the user should know or do next
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateName(name string) error {
	if name != "" && !nameRE.MatchString(name) {
		return fmt.Errorf("invalid service name %q (use letters, digits, - and _)", name)
	}
	return nil
}

// Label is the launchd label for name.
func Label(name string) string {
	if name == "" {
		return "com.syncd.watch"
	}
	return "com.syncd.watch." + name
}

// UnitName is the systemd unit name for name.
func UnitName(name string) string {
	if name == "" {
		return "syncd-watch.service"
	}
	return "syncd-watch-" + name + ".service"
}

// DefinitionPath is where the service definition for name lives.
func (m Manager) DefinitionPath(name string) (string, error) {
	switch m.GOOS {
	case "darwin":
		return filepath.Join(m.Home, "Library", "LaunchAgents", Label(name)+".plist"), nil
	case "linux":
		return filepath.Join(m.Home, ".config", "systemd", "user", UnitName(name)), nil
	}
	return "", fmt.Errorf("installing a service is not supported on %s", m.GOOS)
}

// LogPaths returns the stdout and stderr files of the macOS service name.
func LogPaths(logDir, name string) (stdout, stderr string) {
	base := "service"
	if name != "" {
		base += "-" + name
	}
	return filepath.Join(logDir, base+".out.log"), filepath.Join(logDir, base+".err.log")
}

// Install writes the service definition and (re)starts the service, so
// running it again with different arguments updates a service in place.
func (m Manager) Install(spec Spec) (Result, error) {
	if err := validateName(spec.Name); err != nil {
		return Result{}, err
	}
	path, err := m.DefinitionPath(spec.Name)
	if err != nil {
		return Result{}, err
	}
	var content string
	if m.GOOS == "darwin" {
		if err := os.MkdirAll(spec.LogDir, 0o755); err != nil {
			return Result{}, fmt.Errorf("create log directory: %w", err)
		}
		content = RenderPlist(spec)
	} else {
		content = RenderUnit(spec)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{}, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Result{}, fmt.Errorf("write service definition: %w", err)
	}
	res := Result{Path: path}

	if m.GOOS == "darwin" {
		domain := fmt.Sprintf("gui/%d", m.UID)
		// Unload a previous version first; "not loaded" is the normal case.
		_, _ = m.Run("launchctl", "bootout", domain+"/"+Label(spec.Name))
		if err := m.bootstrap(domain, path); err != nil {
			return res, err
		}
		_, stderr := LogPaths(spec.LogDir, spec.Name)
		res.Notes = append(res.Notes,
			"startup problems are written to "+stderr,
			"if macOS asks whether syncd may access files on a network volume (or a protected folder), allow it; until then the daemon cannot read the share",
		)
		return res, nil
	}

	unit := UnitName(spec.Name)
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", unit},
		{"--user", "restart", unit},
	} {
		if out, err := m.Run("systemctl", args...); err != nil {
			return res, fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	res.Notes = append(res.Notes, "follow its output with: journalctl --user -u "+unit+" -f")
	if user := os.Getenv("USER"); user != "" {
		if out, err := m.Run("loginctl", "show-user", user, "-p", "Linger", "--value"); err == nil && strings.TrimSpace(out) == "no" {
			res.Notes = append(res.Notes, "the service only runs while you are logged in; to start it at boot and keep it running after logout: sudo loginctl enable-linger "+user)
		}
	}
	return res, nil
}

// bootstrapRetryDelay is indirected for tests.
var bootstrapRetryDelay = 500 * time.Millisecond

// bootstrap loads the plist. launchctl bootout returns before the old
// instance has finished exiting, and a bootstrap issued in that window
// fails ("Input/output error"), so it is retried briefly.
func (m Manager) bootstrap(domain, plist string) error {
	var out string
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if out, err = m.Run("launchctl", "bootstrap", domain, plist); err == nil {
			return nil
		}
		time.Sleep(bootstrapRetryDelay)
	}
	return fmt.Errorf("launchctl bootstrap %s %s: %w: %s", domain, plist, err, out)
}

// Uninstall stops the service and removes its definition. It reports
// whether there was a definition to remove.
func (m Manager) Uninstall(name string) (bool, error) {
	if err := validateName(name); err != nil {
		return false, err
	}
	path, err := m.DefinitionPath(name)
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(path)
	installed := statErr == nil

	// Stop it even if the definition is already gone: a loaded service
	// outlives its file. Failure here just means it was not loaded.
	if m.GOOS == "darwin" {
		_, _ = m.Run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", m.UID, Label(name)))
	} else {
		_, _ = m.Run("systemctl", "--user", "disable", "--now", UnitName(name))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return installed, fmt.Errorf("remove service definition: %w", err)
	}
	if m.GOOS == "linux" {
		_, _ = m.Run("systemctl", "--user", "daemon-reload")
	}
	return installed, nil
}

// Status describes the service name in a few lines.
func (m Manager) Status(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	path, err := m.DefinitionPath(name)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(&b, "not installed (no %s)\n", path)
	} else {
		fmt.Fprintf(&b, "definition: %s\n", path)
	}

	if m.GOOS == "darwin" {
		out, err := m.Run("launchctl", "print", fmt.Sprintf("gui/%d/%s", m.UID, Label(name)))
		if err != nil {
			b.WriteString("state: not loaded\n")
			return b.String(), nil
		}
		for _, key := range []string{"state", "pid", "last exit code"} {
			if v, ok := launchctlField(out, key); ok {
				fmt.Fprintf(&b, "%s: %s\n", key, v)
			}
		}
		return b.String(), nil
	}

	// `show` succeeds for an unknown unit too (LoadState=not-found).
	out, err := m.Run("systemctl", "--user", "show", UnitName(name),
		"-p", "LoadState", "-p", "UnitFileState", "-p", "ActiveState", "-p", "SubState", "-p", "MainPID", "-p", "ExecMainStatus")
	if err != nil {
		return b.String(), fmt.Errorf("systemctl show: %w: %s", err, out)
	}
	props := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	if props["LoadState"] == "not-found" {
		b.WriteString("state: not loaded\n")
		return b.String(), nil
	}
	fmt.Fprintf(&b, "state: %s (%s), %s\n", props["ActiveState"], props["SubState"], props["UnitFileState"])
	if pid := props["MainPID"]; pid != "" && pid != "0" {
		fmt.Fprintf(&b, "pid: %s\n", pid)
	} else {
		fmt.Fprintf(&b, "last exit code: %s\n", props["ExecMainStatus"])
	}
	return b.String(), nil
}

// launchctlField extracts a top-level "key = value" line from `launchctl
// print` output (top-level lines are indented by a single tab).
func launchctlField(out, key string) (string, bool) {
	prefix := "\t" + key + " = "
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}
