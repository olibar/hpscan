//go:build windows

package config

import (
	"os"
	"path/filepath"
)

// platformDefaultPath is %ProgramData%\hpscan\config.yaml: readable by the
// service (LocalSystem) and by the user who runs `hpscan config`.
func platformDefaultPath() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "hpscan", "config.yaml")
}
