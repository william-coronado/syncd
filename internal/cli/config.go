package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// SyncConfig is the structure of the batch job config file (~/.syncd.yaml)
// consumed by the `syncd sync` command when it isn't given --left/--right
// for a one-off job. Ported from file_sync_util's cmd/root.go Config/SyncJob
// (name kept distinct from the legacy runtime.Config used by the always-on
// daemon, and tags adapted for our viper setup).
type SyncConfig struct {
	Jobs []SyncJob `mapstructure:"jobs"`
}

// SyncJob is a single synchronization job entry in the batch config file.
type SyncJob struct {
	Name  string `mapstructure:"name"`
	Left  string `mapstructure:"left"`
	Right string `mapstructure:"right"`
	Mode  string `mapstructure:"mode"` // two-way, mirror, update
}

// initSyncConfig wires viper to read ~/.syncd.yaml (or the file at
// cfgFile, if set) before the sync command runs. Ported from
// file_sync_util's initConfig, adapted from .syncutil to .syncd.
//
// A ReadInConfig error is only tolerated when cfgFile is empty AND it's
// specifically "the default ~/.syncd.yaml simply doesn't exist" - ad-hoc
// --left/--right jobs don't require a config file at all, so that case must
// stay silent. Every other case (an explicit --config that's missing,
// unreadable, or malformed YAML; or a malformed default ~/.syncd.yaml) must
// surface as a real error instead of being swallowed into a misleading "no
// jobs found" later (review finding "Medium 10").
func initSyncConfig(cfgFile string) error {
	explicit := cfgFile != ""
	if explicit {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)

		viper.AddConfigPath(home)
		viper.SetConfigType("yaml")
		viper.SetConfigName(".syncd")
	}

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !explicit && errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("read config file: %w", err)
	}
	return nil
}

// loadSyncConfig reads the batch config file (via initSyncConfig) and
// unmarshals it into a SyncConfig.
func loadSyncConfig(cfgFile string) (SyncConfig, error) {
	if err := initSyncConfig(cfgFile); err != nil {
		return SyncConfig{}, err
	}
	var cfg SyncConfig
	if err := viper.Unmarshal(&cfg); err != nil {
		return SyncConfig{}, err
	}
	return cfg, nil
}
