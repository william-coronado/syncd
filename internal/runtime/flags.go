package runtime

// Config is the legacy flags-based configuration for the always-on sync
// daemon (formerly app.Run, removed as dead code once the cobra CLI -
// cmd/syncd -> cli.Execute - superseded it). Config itself is still used by
// ResolvePaths (see paths.go) for state-dir/db/log/lock path resolution, so
// it's kept; only its ParseArgs flag parser, which had no remaining caller,
// was removed.
type Config struct {
	LocalRoot  string
	RemoteRoot string
	DryRun     bool
	Silent     bool
	Init       bool
	Workers    int
	StateDir   string
	DBPath     string
	LogPath    string
	LockPath   string
}
