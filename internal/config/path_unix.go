//go:build !windows

package config

import (
	"os"
	"path/filepath"
)

// platformDefaultPath is ~/.config/hpscan/config.yaml.
func platformDefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.yaml"
	}
	return filepath.Join(home, ".config", "hpscan", "config.yaml")
}
