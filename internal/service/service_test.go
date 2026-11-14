package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
}

func (c call) String() string { return c.name + " " + strings.Join(c.args, " ") }

// recorder is a Runner that records every command and answers from reply.
type recorder struct {
	calls []call
	reply func(c call) (string, error)
}

func (r *recorder) run(name string, args ...string) (string, error) {
	c := call{name, args}
	r.calls = append(r.calls, c)
	if r.reply != nil {
		return r.reply(c)
	}
	return "", nil
}

func (r *recorder) lines() []string {
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.String()
	}
	return out
}

func testSpec(logDir string) Spec {
	return Spec{
		Binary:      "/usr/local/bin/syncd",
		Args:        []string{"watch", "--local", "/Users/me/My Projects", "--remote", "/Volumes/dev", "--silent", "--wait-for-remote", "--max-delete=25%"},
		Description: "syncd watch (/Users/me/My Projects <-> /Volumes/dev)",
		LogDir:      logDir,
	}
}

func TestRenderPlist(t *testing.T) {
	spec := testSpec("/Users/me/.local/state/syncd")
	spec.Args = append(spec.Args, "--state-dir=/tmp/a&b<c>")
	got := RenderPlist(spec)
	for _, want := range []string{
		"<string>com.syncd.watch</string>",
		"<string>/usr/local/bin/syncd</string>",
		"<string>/Users/me/My Projects</string>", // one argument, space and all
		"<string>--state-dir=/tmp/a&amp;b&lt;c&gt;</string>",
		"<key>KeepAlive</key>\n  <true/>",
		"<string>/Users/me/.local/state/syncd/service.err.log</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist is missing %q:\n%s", want, got)
		}
	}
	spec.Name = "music"
	if got := RenderPlist(spec); !strings.Contains(got, "<string>com.syncd.watch.music</string>") || !strings.Contains(got, "service-music.out.log") {
		t.Errorf("named plist:\n%s", got)
	}
}

func TestRenderUnit(t *testing.T) {
	spec := testSpec("")
	got := RenderUnit(spec)
	wantExec := `ExecStart=/usr/local/bin/syncd watch --local "/Users/me/My Projects" --remote /Volumes/dev --silent --wait-for-remote "--max-delete=25%%"`
	if !strings.Contains(got, wantExec+"\n") {
		t.Errorf("unit ExecStart, want:\n%s\ngot:\n%s", wantExec, got)
	}
	for _, want := range []string{"Restart=on-failure", "WantedBy=default.target", "Description=syncd watch (/Users/me/My Projects <-> /Volumes/dev)"} {
		if !strings.Contains(got, want) {
			t.Errorf("unit is missing %q:\n%s", want, got)
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/mnt/dev":         "/mnt/dev",
		"--workers=4":      "--workers=4",
		"":                 `""`,
		"a b":              `"a b"`,
		`say "hi"`:         `"say \"hi\""`,
		`back\slash`:       `"back\\slash"`,
		"$HOME/100%":       `"$$HOME/100%%"`,
		"semi;colon":       `"semi;colon"`,
		"/srv/naïve/path":  `"/srv/naïve/path"`,
		"--max-delete=50%": `"--max-delete=50%%"`,
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestInstallDarwin(t *testing.T) {
	prev := bootstrapRetryDelay
	bootstrapRetryDelay = 0
	t.Cleanup(func() { bootstrapRetryDelay = prev })

	home := t.TempDir()
	logDir := filepath.Join(home, "state")
	// The first bootstrap lands while the old instance is still exiting.
	bootstraps := 0
	rec := &recorder{reply: func(c call) (string, error) {
		if c.args[0] == "bootstrap" {
			if bootstraps++; bootstraps == 1 {
				return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
			}
		}
		return "", nil
	}}
	m := Manager{GOOS: "darwin", Home: home, UID: 501, Run: rec.run}

	res, err := m.Install(testSpec(logDir))
	if err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.syncd.watch.plist")
	if res.Path != plist {
		t.Fatalf("path = %s, want %s", res.Path, plist)
	}
	if b, err := os.ReadFile(plist); err != nil || !strings.Contains(string(b), "com.syncd.watch") {
		t.Fatalf("plist not written: %v", err)
	}
	if _, err := os.Stat(logDir); err != nil {
		t.Fatalf("log dir not created: %v", err)
	}
	want := []string{
		"launchctl bootout gui/501/com.syncd.watch",
		"launchctl bootstrap gui/501 " + plist,
		"launchctl bootstrap gui/501 " + plist,
	}
	if got := rec.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestInstallLinux(t *testing.T) {
	t.Setenv("USER", "me")
	home := t.TempDir()
	rec := &recorder{reply: func(c call) (string, error) {
		if c.name == "loginctl" {
			return "no\n", nil
		}
		return "", nil
	}}
	m := Manager{GOOS: "linux", Home: home, UID: 1000, Run: rec.run}
	spec := testSpec("")
	spec.Name = "dev"

	res, err := m.Install(spec)
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "syncd-watch-dev.service")
	if res.Path != unit {
		t.Fatalf("path = %s, want %s", res.Path, unit)
	}
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable syncd-watch-dev.service",
		"systemctl --user restart syncd-watch-dev.service",
		"loginctl show-user me -p Linger --value",
	}
	if got := rec.lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if notes := strings.Join(res.Notes, "\n"); !strings.Contains(notes, "enable-linger me") {
		t.Errorf("no linger hint in notes:\n%s", notes)
	}
}

func TestInstallLinuxReportsSystemctlFailure(t *testing.T) {
	rec := &recorder{reply: func(c call) (string, error) {
		if c.name == "systemctl" && c.args[1] == "enable" {
			return "Failed to connect to bus", errors.New("exit status 1")
		}
		return "", nil
	}}
	m := Manager{GOOS: "linux", Home: t.TempDir(), Run: rec.run}
	_, err := m.Install(testSpec(""))
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("got %v, want the systemctl failure", err)
	}
}

func TestUninstall(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			prev := bootstrapRetryDelay
			bootstrapRetryDelay = 0
			t.Cleanup(func() { bootstrapRetryDelay = prev })

			home := t.TempDir()
			rec := &recorder{}
			m := Manager{GOOS: goos, Home: home, UID: 501, Run: rec.run}
			res, err := m.Install(testSpec(filepath.Join(home, "state")))
			if err != nil {
				t.Fatal(err)
			}

			rec.calls = nil
			installed, err := m.Uninstall("")
			if err != nil || !installed {
				t.Fatalf("Uninstall: installed=%v err=%v", installed, err)
			}
			if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
				t.Fatal("definition was not removed")
			}
			first := rec.lines()[0]
			wantFirst := map[string]string{
				"darwin": "launchctl bootout gui/501/com.syncd.watch",
				"linux":  "systemctl --user disable --now syncd-watch.service",
			}[goos]
			if first != wantFirst {
				t.Fatalf("first command %q, want %q", first, wantFirst)
			}

			installed, err = m.Uninstall("")
			if err != nil || installed {
				t.Fatalf("second Uninstall: installed=%v err=%v", installed, err)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	home := t.TempDir()

	darwin := Manager{GOOS: "darwin", Home: home, UID: 501, Run: func(string, ...string) (string, error) {
		return "gui/501/com.syncd.watch = {\n\tactive count = 1\n\tstate = running\n\n\tprogram = /x/syncd\n\tpid = 4242\n\tsockets = {\n\t\tstate = nested\n\t}\n}", nil
	}}
	got, err := darwin.Status("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "not installed") || !strings.Contains(got, "state: running\n") || !strings.Contains(got, "pid: 4242\n") {
		t.Errorf("darwin status:\n%s", got)
	}

	notLoaded := Manager{GOOS: "darwin", Home: home, UID: 501, Run: func(string, ...string) (string, error) {
		return "Could not find service", errors.New("exit status 113")
	}}
	if got, err := notLoaded.Status(""); err != nil || !strings.Contains(got, "state: not loaded") {
		t.Errorf("darwin not loaded: %q, %v", got, err)
	}

	linux := Manager{GOOS: "linux", Home: home, Run: func(string, ...string) (string, error) {
		return "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=running\nMainPID=777\nExecMainStatus=0", nil
	}}
	if got, err := linux.Status(""); err != nil || !strings.Contains(got, "state: active (running), enabled\n") || !strings.Contains(got, "pid: 777\n") {
		t.Errorf("linux status: %q, %v", got, err)
	}

	missing := Manager{GOOS: "linux", Home: home, Run: func(string, ...string) (string, error) {
		return "LoadState=not-found\nActiveState=inactive\nMainPID=0", nil
	}}
	if got, err := missing.Status(""); err != nil || !strings.Contains(got, "state: not loaded") {
		t.Errorf("linux not loaded: %q, %v", got, err)
	}
}

func TestRejectsBadNameAndPlatform(t *testing.T) {
	m := Manager{GOOS: "darwin", Home: t.TempDir(), Run: (&recorder{}).run}
	spec := testSpec(t.TempDir())
	spec.Name = "../evil"
	if _, err := m.Install(spec); err == nil {
		t.Error("accepted a service name with path characters")
	}
	if _, err := m.Uninstall("a b"); err == nil {
		t.Error("Uninstall accepted a bad name")
	}
	win := Manager{GOOS: "windows", Home: t.TempDir(), Run: (&recorder{}).run}
	if _, err := win.Install(testSpec(t.TempDir())); err == nil {
		t.Error("Install succeeded on an unsupported platform")
	}
}
