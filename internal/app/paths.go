package app

// Where the configuration and the data directory live.
//
// The binary used to write config.json and data/ into the working directory,
// which is fine for a portable copy but litters a checkout. Defaults now
// follow XDG on every system, Windows included, so one set of environment
// variables describes a deployment wherever it runs.
//
// Precedence, each path on its own:
//
//  1. LITEROUTER_CONFIG / LITEROUTER_DATA_DIR
//  2. a config.json in the working directory (what earlier versions did,
//     kept so an existing checkout or portable copy keeps working)
//  3. XDG_CONFIG_HOME / XDG_DATA_HOME, else ~/.config and ~/.local/share
//
// A relative XDG value is ignored, as the XDG spec asks.

import (
	"os"
	"path/filepath"
	"strings"
)

const appName = "literouter"

// legacyConfig is the file earlier versions read from the working directory.
const legacyConfig = "config.json"

// legacyData is the data directory that went beside it.
const legacyData = "data"

// resolvePaths returns the configuration file and the data directory, both
// absolute when the filesystem allows it.
func resolvePaths() (configPath, dataDir string) {
	configEnv := strings.TrimSpace(os.Getenv("LITEROUTER_CONFIG"))
	dataEnv := strings.TrimSpace(os.Getenv("LITEROUTER_DATA_DIR"))

	// Both unset: a config.json beside the binary keeps the old layout, so
	// running an already-configured copy never suddenly reads somewhere else.
	if configEnv == "" && dataEnv == "" && fileExists(legacyConfig) {
		return absoluteOr(legacyConfig), absoluteOr(legacyData)
	}

	if configEnv == "" {
		configEnv = filepath.Join(xdgHome("XDG_CONFIG_HOME", ".config"), appName, "config.json")
	}
	if dataEnv == "" {
		dataEnv = filepath.Join(xdgHome("XDG_DATA_HOME", filepath.Join(".local", "share")), appName, "data")
	}
	return absoluteOr(configEnv), absoluteOr(dataEnv)
}

// xdgHome is the base folder named by env, else fallback under the home
// folder. A relative value is not a base folder and is ignored.
func xdgHome(env, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" && filepath.IsAbs(value) {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	return filepath.Join(home, fallback)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// absoluteOr is path as an absolute path, or path itself when the current
// directory can't be resolved.
func absoluteOr(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}
