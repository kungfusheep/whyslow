package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// settingsEnv overrides the settings path, so tests never touch the real file.
const settingsEnv = "WHYSLOW_SETTINGS_PATH"

type settings struct {
	Theme string `json:"theme,omitempty"`
}

func settingsPath() (string, error) {
	if path := os.Getenv(settingsEnv); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "whyslow", "settings.json"), nil
}

func loadSettings() settings {
	var s settings
	path, err := settingsPath()
	if err != nil {
		return s
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

// saveSettings writes through a temp file and a rename, so a crash never leaves half a file.
func saveSettings(s settings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
