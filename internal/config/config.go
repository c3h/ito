package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Backend string

const (
	BackendLocal Backend = "local"
	BackendCloud Backend = "cloud"
)

type Cloud struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type Config struct {
	Backend Backend `json:"backend"`
	Cloud   *Cloud  `json:"cloud,omitempty"`
}

func HomeDir() (string, error) {
	home := os.Getenv("ITO_HOME")
	if home != "" {
		return home, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".ito"), nil
}

func Path(home string) string {
	return filepath.Join(home, "config.json")
}

func LocalDBPath(home string) string {
	return filepath.Join(home, "ito.db")
}

func Load() (Config, error) {
	home, err := HomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve ito home: %w", err)
	}
	return load(Path(home))
}

func load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{Backend: BackendLocal}, nil
		}
		return Config{}, fmt.Errorf("read config file %q: %w", path, err)
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Config{}, fmt.Errorf("config file %q is malformed: expected a JSON object", path)
	}

	var cfg Config
	if err := json.Unmarshal(trimmed, &cfg); err != nil {
		return Config{}, fmt.Errorf("config file %q is malformed: %w", path, err)
	}
	return normalize(cfg, path)
}

func normalize(cfg Config, path string) (Config, error) {
	if cfg.Backend == "" {
		cfg.Backend = BackendLocal
	}
	switch cfg.Backend {
	case BackendLocal:
		return cfg, nil
	case BackendCloud:
		if cfg.Cloud == nil || cfg.Cloud.URL == "" || cfg.Cloud.Token == "" {
			return Config{}, fmt.Errorf("config file %q is malformed: cloud backend requires url and token", path)
		}
		return cfg, nil
	default:
		return Config{}, fmt.Errorf("config file %q is malformed: backend must be %q or %q", path, BackendLocal, BackendCloud)
	}
}

func Write(cfg Config) error {
	home, err := HomeDir()
	if err != nil {
		return fmt.Errorf("resolve ito home: %w", err)
	}
	path := Path(home)
	cfg, err = normalize(cfg, path)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config file %q: %w", path, err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("create ito home %q: %w", home, err)
	}
	temp, err := os.CreateTemp(home, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create config file in %q: %w", home, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure config file %q: %w", tempPath, err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write config file %q: %w", tempPath, err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync config file %q: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close config file %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace config file %q: %w", path, err)
	}
	return nil
}
