package configstore

import (
	"errors"
	"os"
	"path/filepath"

	khunquantconfig "github.com/cryptoquantumwave/khunquant/pkg/config"
)

const (
	configDirName  = ".khunquant"
	configFileName = "config.json"
)

func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, configDirName), nil
}

func Load() (*khunquantconfig.Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	return khunquantconfig.LoadConfig(path)
}

func Save(cfg *khunquantconfig.Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return khunquantconfig.SaveConfig(path, cfg)
}
