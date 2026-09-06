// Package config loads, validates and rewrites the hpscan YAML configuration.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the flat on-disk configuration. Keys are flat on purpose so that
// `hpscan config set <key> <value>` can address every field.
type Config struct {
	Printer     string `yaml:"printer"`
	Port        int    `yaml:"port"`
	Name        string `yaml:"name"`
	OutputDir   string `yaml:"output_dir"`
	Format      string `yaml:"format"`
	Resolution  int    `yaml:"resolution"`
	ColorMode   string `yaml:"color_mode"`
	Paper       string `yaml:"paper"`
	Filename    string `yaml:"filename"`
	PageTimeout string `yaml:"page_timeout"`
	LogLevel    string `yaml:"log_level"`
	LogFile     string `yaml:"log_file"`
}

// Defaults returns a configuration that works without any edits once the
// printer is discoverable through mDNS.
func Defaults() Config {
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	if host == "" {
		host = "hpscan"
	}
	return Config{
		Port:        8080,
		Name:        host,
		OutputDir:   "~/Scans",
		Format:      "pdf",
		Resolution:  300,
		ColorMode:   "color",
		Paper:       "a4",
		Filename:    "scan_{date}_{time}",
		PageTimeout: "120s",
		LogLevel:    "info",
	}
}

// DefaultPath returns the config file location: $HPSCAN_CONFIG if set,
// otherwise ~/.config/hpscan/config.yaml (Windows: %ProgramData%\hpscan).
func DefaultPath() string {
	if p := os.Getenv("HPSCAN_CONFIG"); p != "" {
		return p
	}
	return platformDefaultPath()
}

// Load reads and validates the configuration at path. Missing fields take
// their defaults.
func Load(path string) (Config, error) {
	slog.Debug("config: loading", "path", path)
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("validate config %s: %w", path, err)
	}
	slog.Debug("config: loaded", "printer", cfg.Printer, "port", cfg.Port,
		"name", cfg.Name, "output_dir", cfg.OutputDir, "format", cfg.Format,
		"resolution", cfg.Resolution, "color_mode", cfg.ColorMode, "paper", cfg.Paper)
	return cfg, nil
}

// Validate checks enumerated fields and normalises case.
func (c *Config) Validate() error {
	c.Format = strings.ToLower(c.Format)
	c.ColorMode = strings.ToLower(c.ColorMode)
	c.Paper = strings.ToLower(c.Paper)
	c.LogLevel = strings.ToLower(c.LogLevel)
	switch c.Format {
	case "pdf", "jpeg":
	default:
		return fmt.Errorf("format must be pdf or jpeg, got %q", c.Format)
	}
	switch c.ColorMode {
	case "color", "gray":
	default:
		return fmt.Errorf("color_mode must be color or gray, got %q", c.ColorMode)
	}
	switch c.Paper {
	case "a4", "letter":
	default:
		return fmt.Errorf("paper must be a4 or letter, got %q", c.Paper)
	}
	if c.Resolution < 75 || c.Resolution > 1200 {
		return fmt.Errorf("resolution must be between 75 and 1200, got %d", c.Resolution)
	}
	if c.Name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if c.Filename == "" {
		return fmt.Errorf("filename must not be empty")
	}
	if _, err := time.ParseDuration(c.PageTimeout); err != nil {
		return fmt.Errorf("page_timeout: %w", err)
	}
	return nil
}

// PageTimeoutDuration returns the parsed page_timeout. Validate must have run.
func (c Config) PageTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(c.PageTimeout)
	return d
}

// ExpandedOutputDir returns output_dir with a leading ~ expanded.
func (c Config) ExpandedOutputDir() string {
	return ExpandHome(c.OutputDir)
}

// ExpandHome replaces a leading "~" with the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// WriteSample writes the annotated sample configuration to path, creating
// parent directories. It refuses to overwrite an existing file.
func WriteSample(path string, cfg Config) error {
	slog.Debug("config: writing sample", "path", path)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config %s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(Sample(cfg)), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Set updates one top-level key in the YAML file at path while preserving
// comments and ordering of the other keys.
func Set(path, key, value string) error {
	slog.Debug("config: set", "path", path, "key", key, "value", value)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}
	root := doc.Content[0]
	if !isKnownKey(key) {
		return fmt.Errorf("unknown config key %q", key)
	}
	found := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			setScalar(root.Content[i+1], value)
			found = true
			break
		}
	}
	if !found {
		v := &yaml.Node{Kind: yaml.ScalarNode}
		setScalar(v, value)
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	var check Config
	if err := yaml.Unmarshal(out, &check); err != nil {
		return fmt.Errorf("re-parse config: %w", err)
	}
	check = merge(Defaults(), check)
	if err := check.Validate(); err != nil {
		return fmt.Errorf("new value rejected: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	slog.Debug("config: set done", "key", key)
	return nil
}

func setScalar(n *yaml.Node, value string) {
	n.Kind = yaml.ScalarNode
	n.Tag = ""
	n.Style = 0
	n.Value = value
	if value == "" {
		n.Style = yaml.DoubleQuotedStyle
	}
}

func isKnownKey(key string) bool {
	data, _ := yaml.Marshal(Defaults())
	var m map[string]any
	_ = yaml.Unmarshal(data, &m)
	_, ok := m[key]
	return ok
}

// merge fills zero-valued fields of c from d so validation sees defaults.
func merge(d, c Config) Config {
	if c.Port == 0 {
		c.Port = d.Port
	}
	if c.Name == "" {
		c.Name = d.Name
	}
	if c.Format == "" {
		c.Format = d.Format
	}
	if c.Resolution == 0 {
		c.Resolution = d.Resolution
	}
	if c.ColorMode == "" {
		c.ColorMode = d.ColorMode
	}
	if c.Paper == "" {
		c.Paper = d.Paper
	}
	if c.Filename == "" {
		c.Filename = d.Filename
	}
	if c.PageTimeout == "" {
		c.PageTimeout = d.PageTimeout
	}
	if c.LogLevel == "" {
		c.LogLevel = d.LogLevel
	}
	return c
}

// Sample renders the annotated sample config with cfg's values filled in.
func Sample(cfg Config) string {
	return fmt.Sprintf(`# hpscan configuration
# Edit by hand or with: hpscan config set <key> <value>

# Printer hostname or IP address. Prefer the Bonjour hostname (e.g.
# HPxxxxxx.local, shown by "hpscan discover"): it is derived from the printer's
# MAC address and survives IP changes. Leave empty to auto-discover the first
# HP scanner on the local network via mDNS. If the configured address stops
# answering, the daemon falls back to mDNS discovery automatically.
printer: %q

# HTTP port of the printer's embedded web services. 8080 for most HP
# LEDM printers (some use 80). Ignored when auto-discovering (mDNS supplies it).
port: %d

# Destination name shown in the printer's "Scan to Computer" menu.
name: %q

# Folder where scans are saved. "~" expands to the home directory.
output_dir: %q

# Default output format: pdf | jpeg
# When the printer offers "Save as PDF" / "Save as JPEG" shortcuts, the
# shortcut chosen on the printer wins over this default.
format: %s

# Scan resolution in DPI: 75 .. 1200 (200 or 300 is typical for documents).
resolution: %d

# color | gray
color_mode: %s

# Scan area: a4 | letter (clamped to the scanner's maximum).
paper: %s

# Filename pattern without extension. Tokens: {date} {time} {page}
# {page} is only used for jpeg output.
filename: %q

# For multi-page PDF: how long to wait for another page before the
# document is closed if the printer never sends a "pages complete" event.
page_timeout: %s

# debug | info | warn | error
log_level: %s

# Log file path. Empty logs to stderr (launchd / systemd capture it).
log_file: %q
`, cfg.Printer, cfg.Port, cfg.Name, cfg.OutputDir, cfg.Format, cfg.Resolution,
		cfg.ColorMode, cfg.Paper, cfg.Filename, cfg.PageTimeout, cfg.LogLevel, cfg.LogFile)
}
