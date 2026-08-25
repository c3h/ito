package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Ledger is the connection to the shared change log (decision 0005). The
// token is a credential: the file carrying it is written with 0600 and the
// CLI never prints it.
type Ledger struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

type Config struct {
	Ledger *Ledger `json:"ledger,omitempty"`
}

// ErrRetiredCloudBackend marks a config that still selects the per-statement
// cloud backend retired by decision 0005.
var ErrRetiredCloudBackend = errors.New("the cloud backend is no longer supported: the store is always local and machines sync through a Ledger; remove the backend and cloud fields, then connect with 'ito ledger connect' once it ships")

// configFile is the on-disk shape, including the backend field that predates
// decision 0005.
type configFile struct {
	Backend string  `json:"backend"`
	Ledger  *Ledger `json:"ledger,omitempty"`
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
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read config file %q: %w", path, err)
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Config{}, fmt.Errorf("config file %q is malformed: expected a JSON object", path)
	}

	var file configFile
	if err := json.Unmarshal(trimmed, &file); err != nil {
		return Config{}, fmt.Errorf("config file %q is malformed: %w", path, err)
	}
	switch file.Backend {
	case "", "local":
		return Config{Ledger: file.Ledger}, nil
	case "cloud":
		return Config{}, fmt.Errorf("config file %q: %w", path, ErrRetiredCloudBackend)
	default:
		return Config{}, fmt.Errorf("config file %q is malformed: unknown backend %q", path, file.Backend)
	}
}

func Write(cfg Config) error {
	home, err := HomeDir()
	if err != nil {
		return fmt.Errorf("resolve ito home: %w", err)
	}
	path := Path(home)
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
