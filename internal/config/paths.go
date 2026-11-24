package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// ConfigPath is the default config file: $XDG_CONFIG_HOME/localci/config.yaml,
// else ~/.config/localci/config.yaml (on macOS too).
func ConfigPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "localci", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "localci", "config.yaml")
}

// DataDir is where state and logs live: $XDG_DATA_HOME/localci, else
// ~/Library/Application Support/localci on macOS and ~/.local/share/localci elsewhere.
func DataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "localci")
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "localci")
	}
	return filepath.Join(home, ".local", "share", "localci")
}
