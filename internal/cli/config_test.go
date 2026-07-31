package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

// resetViper undoes viper's global config-file wiring between tests, since
// initSyncConfig mutates package-level viper state (SetConfigFile /
// AddConfigPath / SetConfigName) that would otherwise leak across tests.
func resetViper(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
}

// TestLoadSyncConfig_ExplicitMissingConfigFileErrors is a regression test
// for review finding "Medium 10": initSyncConfig discarded
// viper.ReadInConfig()'s error unconditionally (`_ = viper.ReadInConfig()`).
// The doc comment justifying that only covers the DEFAULT search path (no
// ~/.syncd.yaml is fine, since ad-hoc --left/--right jobs don't need one) -
// but the same line also swallowed the error when the user passed an
// explicit --config pointing at a file that doesn't exist, producing a
// misleading "no jobs found" instead of a real error.
func TestLoadSyncConfig_ExplicitMissingConfigFileErrors(t *testing.T) {
	resetViper(t)

	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	_, err := loadSyncConfig(missing)
	if err == nil {
		t.Fatal("expected loadSyncConfig to error for a missing explicit --config file, got nil")
	}
}

// TestLoadSyncConfig_ExplicitMalformedConfigFileErrors covers the malformed
// (not just missing) half of the same finding.
func TestLoadSyncConfig_ExplicitMalformedConfigFileErrors(t *testing.T) {
	resetViper(t)

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("jobs: [this is not: valid: yaml"), 0o644); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	_, err := loadSyncConfig(bad)
	if err == nil {
		t.Fatal("expected loadSyncConfig to error for a malformed explicit --config file, got nil")
	}
}

// TestLoadSyncConfig_DefaultAbsentConfigIsNotAnError ensures the fix is
// scoped correctly: the default search path (no --config given) must still
// tolerate ~/.syncd.yaml simply not existing - ad-hoc --left/--right jobs
// don't require a config file at all.
func TestLoadSyncConfig_DefaultAbsentConfigIsNotAnError(t *testing.T) {
	resetViper(t)
	t.Setenv("HOME", t.TempDir()) // guaranteed not to contain .syncd.yaml

	cfg, err := loadSyncConfig("")
	if err != nil {
		t.Fatalf("expected no error when the default config file is simply absent, got: %v", err)
	}
	if len(cfg.Jobs) != 0 {
		t.Fatalf("expected an empty SyncConfig, got %+v", cfg)
	}
}

// TestLoadSyncConfig_ExplicitValidConfigFileLoadsJobs is the happy-path
// sibling: a valid, explicit --config file must still load normally.
func TestLoadSyncConfig_ExplicitValidConfigFileLoadsJobs(t *testing.T) {
	resetViper(t)

	good := filepath.Join(t.TempDir(), "good.yaml")
	yaml := "jobs:\n  - name: test-job\n    left: /a\n    right: /b\n    mode: mirror\n"
	if err := os.WriteFile(good, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := loadSyncConfig(good)
	if err != nil {
		t.Fatalf("loadSyncConfig: %v", err)
	}
	if len(cfg.Jobs) != 1 || cfg.Jobs[0].Name != "test-job" {
		t.Fatalf("expected 1 job named test-job, got %+v", cfg.Jobs)
	}
}
