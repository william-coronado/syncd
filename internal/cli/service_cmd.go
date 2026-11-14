package cli

import (
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"syncd/internal/app"
	"syncd/internal/runtime"
	"syncd/internal/service"
)

var (
	serviceOpts watchFlags
	serviceName string
	serviceBin  string
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Install, remove or inspect 'syncd watch' as a background service",
	Long: `Run 'syncd watch' as a per-user background service that starts at login
and is restarted if it exits: a launchd agent on macOS
(~/Library/LaunchAgents), a systemd user unit on Linux
(~/.config/systemd/user). No root access is needed.`,
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install (or update) and start the watch service",
	Long: `Install 'syncd watch' as a background service and start it. It takes the
same flags as 'syncd watch'; the service always runs with --silent and
--wait-for-remote, so it waits for the share to be mounted rather than
failing when it starts first.

Running it again replaces the service's settings and restarts it. Use --name
to run more than one service (one per local/remote pair).

Run 'syncd init' for the pair first.`,
	RunE: runServiceInstall,
}

var serviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the watch service",
	RunE: func(_ *cobra.Command, _ []string) error {
		m, err := newServiceManager()
		if err != nil {
			return err
		}
		installed, err := m.Uninstall(serviceName)
		if err != nil {
			return err
		}
		if !installed {
			fmt.Println("no service was installed")
			return nil
		}
		fmt.Println("service stopped and removed (sync state, logs and trash are kept)")
		return nil
	},
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether the watch service is installed and running",
	RunE: func(_ *cobra.Command, _ []string) error {
		m, err := newServiceManager()
		if err != nil {
			return err
		}
		status, err := m.Status(serviceName)
		fmt.Print(status)
		return err
	},
}

func init() {
	addWatchFlags(serviceInstallCmd, &serviceOpts)
	// Always on for a service; not the user's to choose here.
	_ = serviceInstallCmd.Flags().MarkHidden("silent")
	_ = serviceInstallCmd.Flags().MarkHidden("wait-for-remote")
	serviceInstallCmd.Flags().StringVar(&serviceBin, "bin", "", "syncd executable the service runs (default: this one)")

	for _, c := range []*cobra.Command{serviceInstallCmd, serviceUninstallCmd, serviceStatusCmd} {
		c.Flags().StringVar(&serviceName, "name", "", "Service name, to run more than one (letters, digits, - and _)")
		serviceCmd.AddCommand(c)
	}
	rootCmd.AddCommand(serviceCmd)
}

func newServiceManager() (service.Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return service.Manager{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return service.Manager{GOOS: stdruntime.GOOS, Home: home, UID: os.Getuid(), Run: service.ExecRunner}, nil
}

func runServiceInstall(cmd *cobra.Command, _ []string) error {
	cfg, err := serviceOpts.watchConfig()
	if err != nil {
		return err
	}
	notes, err := checkServiceRoots(cfg)
	if err != nil {
		return err
	}

	bin := serviceBin
	if bin == "" {
		if bin, err = os.Executable(); err != nil {
			return fmt.Errorf("locate the syncd executable (pass --bin): %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(bin); err == nil {
			bin = resolved
		}
	}
	if bin, err = filepath.Abs(bin); err != nil {
		return fmt.Errorf("resolve --bin: %w", err)
	}
	if info, err := os.Stat(bin); err != nil || info.IsDir() {
		return fmt.Errorf("syncd executable not found at %s", bin)
	}

	// The same state directory the daemon will use, for launchd's
	// stdout/stderr files.
	paths, err := runtime.ResolvePaths(runtime.Config{StateDir: absIfSet(cfg.StateDir)})
	if err != nil {
		return err
	}

	m, err := newServiceManager()
	if err != nil {
		return err
	}
	res, err := m.Install(service.Spec{
		Name:        serviceName,
		Binary:      bin,
		Args:        serviceWatchArgs(cmd.Flags(), cfg),
		Description: fmt.Sprintf("syncd watch (%s <-> %s)", cfg.LocalRoot, cfg.RemoteRoot),
		LogDir:      paths.StateDir,
	})
	if res.Path != "" {
		fmt.Println("wrote " + res.Path)
	}
	if err != nil {
		return err
	}
	fmt.Printf("service started: %s <-> %s\n", cfg.LocalRoot, cfg.RemoteRoot)
	for _, note := range append(notes, res.Notes...) {
		fmt.Println("note: " + note)
	}
	return nil
}

// checkServiceRoots catches, at install time, the setup mistakes that would
// otherwise only show up in a background service's log: a local root that
// isn't there, or a mounted share that was never initialized. A share that
// is simply not mounted right now is fine - the daemon waits for it.
func checkServiceRoots(cfg app.WatchConfig) (notes []string, err error) {
	info, err := os.Stat(cfg.LocalRoot)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("--local %s is not an existing directory (run 'syncd init' first)", cfg.LocalRoot)
	}
	if _, err := os.Stat(cfg.RemoteRoot); err != nil {
		return []string{fmt.Sprintf("%s is not available right now; the service will wait for it", cfg.RemoteRoot)}, nil
	}
	if err := runtime.CheckRemoteCanary(cfg.RemoteRoot); err != nil {
		return nil, fmt.Errorf("%w; run 'syncd init --local %s --remote %s' first", err, cfg.LocalRoot, cfg.RemoteRoot)
	}
	return nil, nil
}

// serviceWatchArgs builds the `syncd watch` command line the service runs:
// the two roots, the always-on service flags, then every other flag the
// user actually passed (defaults are left to the daemon, so a later syncd
// that changes a default is not pinned to the old one). Paths are made
// absolute, since a service does not start in the directory it was
// installed from.
func serviceWatchArgs(flags *pflag.FlagSet, cfg app.WatchConfig) []string {
	args := []string{"watch", "--local", cfg.LocalRoot, "--remote", cfg.RemoteRoot, "--silent", "--wait-for-remote"}
	isPath := map[string]bool{"state-dir": true, "db-path": true, "log-path": true, "lock-path": true}
	skip := map[string]bool{"local": true, "remote": true, "silent": true, "wait-for-remote": true, "name": true, "bin": true}
	flags.Visit(func(f *pflag.Flag) {
		if skip[f.Name] {
			return
		}
		value := f.Value.String()
		if isPath[f.Name] {
			value = absIfSet(value)
		}
		args = append(args, "--"+f.Name+"="+value)
	})
	return args
}

func absIfSet(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
