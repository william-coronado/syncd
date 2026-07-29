package runtime

import "testing"

func TestParseArgs_RequiresLocalAndRemote(t *testing.T) {
	cases := [][]string{
		{},
		{"--local", "/tmp/local"},
		{"--remote", "/tmp/remote"},
	}
	for _, args := range cases {
		if _, err := ParseArgs(args); err == nil {
			t.Fatalf("ParseArgs(%v) expected error, got nil", args)
		}
	}
}

func TestParseArgs_ValidArgsResolveToAbsolutePaths(t *testing.T) {
	cfg, err := ParseArgs([]string{"--local", "relative/local", "--remote", "relative/remote"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if cfg.LocalRoot == "relative/local" {
		t.Fatalf("expected LocalRoot to be resolved to an absolute path, got %q", cfg.LocalRoot)
	}
	if cfg.RemoteRoot == "relative/remote" {
		t.Fatalf("expected RemoteRoot to be resolved to an absolute path, got %q", cfg.RemoteRoot)
	}
}

func TestParseArgs_RejectsZeroOrNegativeWorkers(t *testing.T) {
	_, err := ParseArgs([]string{"--local", "/tmp/local", "--remote", "/tmp/remote", "--workers", "0"})
	if err == nil {
		t.Fatal("expected error for --workers=0, got nil")
	}
}

func TestParseArgs_DryRunAndSilentFlags(t *testing.T) {
	cfg, err := ParseArgs([]string{"--local", "/tmp/local", "--remote", "/tmp/remote", "--dry-run", "--silent"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if !cfg.DryRun {
		t.Fatal("expected DryRun to be true")
	}
	if !cfg.Silent {
		t.Fatal("expected Silent to be true")
	}
}

func TestParseArgs_DefaultsWorkersToPositive(t *testing.T) {
	cfg, err := ParseArgs([]string{"--local", "/tmp/local", "--remote", "/tmp/remote"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if cfg.Workers < 1 {
		t.Fatalf("expected default Workers >= 1, got %d", cfg.Workers)
	}
}
