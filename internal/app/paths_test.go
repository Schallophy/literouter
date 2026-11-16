package app

import (
	"os"
	"path/filepath"
	"testing"
)

// chdir moves into dir for the test and back afterwards. t.Chdir is newer
// than the Go version this module promises, so it is done by hand.
func chdir(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("chdir back %s: %v", previous, err)
		}
	})
}

// home pins the home folder both Unix and Windows read.
func home(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LITEROUTER_CONFIG", "")
	t.Setenv("LITEROUTER_DATA_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
}

func TestResolvePathsUsesXDG(t *testing.T) {
	base := t.TempDir()
	home(t, base)
	clearEnv(t)
	chdir(t, t.TempDir())

	configHome := filepath.Join(base, "cfg")
	dataHome := filepath.Join(base, "share")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	configPath, dataDir := resolvePaths()
	if want := filepath.Join(configHome, "literouter", "config.json"); configPath != want {
		t.Errorf("config path = %q, want %q", configPath, want)
	}
	if want := filepath.Join(dataHome, "literouter", "data"); dataDir != want {
		t.Errorf("data dir = %q, want %q", dataDir, want)
	}
}

func TestResolvePathsFallsBackToHome(t *testing.T) {
	base := t.TempDir()
	home(t, base)
	clearEnv(t)
	chdir(t, t.TempDir())

	configPath, dataDir := resolvePaths()
	if want := filepath.Join(base, ".config", "literouter", "config.json"); configPath != want {
		t.Errorf("config path = %q, want %q", configPath, want)
	}
	if want := filepath.Join(base, ".local", "share", "literouter", "data"); dataDir != want {
		t.Errorf("data dir = %q, want %q", dataDir, want)
	}
}

func TestResolvePathsKeepsConfigBesideTheBinary(t *testing.T) {
	base := t.TempDir()
	home(t, base)
	clearEnv(t)
	cwd := t.TempDir()
	chdir(t, cwd)
	if err := os.WriteFile(filepath.Join(cwd, legacyConfig), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	configPath, dataDir := resolvePaths()
	if want := filepath.Join(cwd, legacyConfig); configPath != want {
		t.Errorf("config path = %q, want the one beside the binary %q", configPath, want)
	}
	if want := filepath.Join(cwd, legacyData); dataDir != want {
		t.Errorf("data dir = %q, want %q", dataDir, want)
	}
}

func TestResolvePathsOneEnvDoesNotFallBackToTheWorkingDirectory(t *testing.T) {
	base := t.TempDir()
	home(t, base)
	clearEnv(t)
	cwd := t.TempDir()
	chdir(t, cwd)
	// A legacy config is present, but an explicit one takes over and the
	// data directory must not follow it into the checkout.
	if err := os.WriteFile(filepath.Join(cwd, legacyConfig), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(base, "elsewhere", "config.json")
	t.Setenv("LITEROUTER_CONFIG", configPath)

	gotConfig, gotData := resolvePaths()
	if want := absoluteOr(configPath); gotConfig != want {
		t.Errorf("config path = %q, want %q", gotConfig, want)
	}
	if want := filepath.Join(base, ".local", "share", "literouter", "data"); gotData != want {
		t.Errorf("data dir = %q, want the XDG one %q", gotData, want)
	}
}

func TestResolvePathsIgnoresRelativeXDG(t *testing.T) {
	base := t.TempDir()
	home(t, base)
	clearEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	t.Setenv("XDG_DATA_HOME", "relative/data")

	configPath, dataDir := resolvePaths()
	if want := filepath.Join(base, ".config", "literouter", "config.json"); configPath != want {
		t.Errorf("config path = %q, want %q", configPath, want)
	}
	if want := filepath.Join(base, ".local", "share", "literouter", "data"); dataDir != want {
		t.Errorf("data dir = %q, want %q", dataDir, want)
	}
}

func TestResolvePathsBothEnvWin(t *testing.T) {
	base := t.TempDir()
	home(t, base)
	clearEnv(t)
	chdir(t, t.TempDir())

	configPath := filepath.Join(base, "a", "config.json")
	dataDir := filepath.Join(base, "b", "data")
	t.Setenv("LITEROUTER_CONFIG", configPath)
	t.Setenv("LITEROUTER_DATA_DIR", dataDir)

	gotConfig, gotData := resolvePaths()
	if gotConfig != absoluteOr(configPath) {
		t.Errorf("config path = %q, want %q", gotConfig, absoluteOr(configPath))
	}
	if gotData != absoluteOr(dataDir) {
		t.Errorf("data dir = %q, want %q", gotData, absoluteOr(dataDir))
	}
}
