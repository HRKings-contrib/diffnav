package config

import (
	"os"
	"path/filepath"
	"runtime"
	"time"

	"gopkg.in/yaml.v3"
)

type UIConfig struct {
	HideHeader      bool   `yaml:"hideHeader"`
	HideFooter      bool   `yaml:"hideFooter"`
	ShowFileTree    bool   `yaml:"showFileTree"`
	FileTreeWidth   int    `yaml:"fileTreeWidth"`
	SearchTreeWidth int    `yaml:"searchTreeWidth"`
	Icons           string `yaml:"icons"`          // "nerd-fonts-status" (default), "nerd-fonts-simple", "nerd-fonts-filetype", "nerd-fonts-full", "unicode", "ascii"
	ColorFileNames  bool   `yaml:"colorFileNames"` // Color filenames by git status (default: true)
	ShowDiffStats   bool   `yaml:"showDiffStats"`  // Show the amount of lines added / removed next to the file
	SideBySide      bool   `yaml:"sideBySide"`     // Side-by-side diff view (default: true) — kept for backward compat
	DeltaDisplay    string `yaml:"deltaDisplay"`    // Display mode for delta: "side-by-side" (default), "inline"
	DifftDisplay    string `yaml:"difftDisplay"`    // Display mode for difft: "side-by-side" (default), "inline", "side-by-side-show-both"
}

type WatchConfig struct {
	Enabled  bool
	Cmd      string
	Interval time.Duration
}

type DifftConfig struct {
	Enabled bool
	GitCmd  string
}

type Config struct {
	UI    UIConfig    `yaml:"ui"`
	Watch WatchConfig `yaml:"-"`
	Difft DifftConfig `yaml:"-"`
}

func DefaultConfig() Config {
	return Config{
		UI: UIConfig{
			HideHeader:      false,
			HideFooter:      false,
			ShowFileTree:    true,
			FileTreeWidth:   30,
			SearchTreeWidth: 50,
			Icons:           "nerd-fonts-status",
			ColorFileNames:  true,
			SideBySide:      true,
			ShowDiffStats:   true,
			DeltaDisplay:    "side-by-side",
			DifftDisplay:    "side-by-side",
		},
	}
}

// ResolveDisplay returns the effective display mode string for the active diff tool.
// It respects the legacy SideBySide bool when DeltaDisplay/DifftDisplay are not explicitly set.
func (c *Config) ResolveDisplay(difft bool) string {
	if difft {
		if c.UI.DifftDisplay != "" {
			return c.UI.DifftDisplay
		}
		if c.UI.SideBySide {
			return "side-by-side"
		}
		return "inline"
	}
	if c.UI.DeltaDisplay != "" {
		return c.UI.DeltaDisplay
	}
	if c.UI.SideBySide {
		return "side-by-side"
	}
	return "inline"
}

func getConfigFilePath() string {
	var configDirs []string

	// Environment variable override - useful for development or non-standard setups.
	if dir := os.Getenv("DIFFNAV_CONFIG_DIR"); dir != "" {
		if s, err := os.Stat(dir); err == nil && s.IsDir() {
			return filepath.Join(dir, "config.yml")
		}
	}

	// On macOS, check XDG_CONFIG_HOME first (if user explicitly set it),
	// then fall back to ~/.config (common for CLI tools).
	// os.UserConfigDir() already handles this for Linux.
	if runtime.GOOS == "darwin" {
		if xdgConfigDir := os.Getenv("XDG_CONFIG_HOME"); xdgConfigDir != "" {
			configDirs = append(configDirs, xdgConfigDir)
		}
		if home := os.Getenv("HOME"); home != "" {
			configDirs = append(configDirs, filepath.Join(home, ".config"))
		}
	}

	// Standard OS-specific config directory.
	if configDir, err := os.UserConfigDir(); err == nil {
		configDirs = append(configDirs, configDir)
	}

	// Return the first config file that exists.
	for _, dir := range configDirs {
		configPath := filepath.Join(dir, "diffnav", "config.yml")
		if _, err := os.Stat(configPath); err == nil {
			return configPath
		}
	}

	// If no config file exists, return the preferred path for creation.
	if len(configDirs) > 0 {
		return filepath.Join(configDirs[0], "diffnav", "config.yml")
	}
	return ""
}

func Load() Config {
	cfg := DefaultConfig()

	configPath := getConfigFilePath()
	if configPath == "" {
		return cfg
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return cfg
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return DefaultConfig()
	}

	return cfg
}
