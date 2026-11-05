package xdgpath

import (
	"fmt"
	"os"
	"path/filepath"
)

func ConfigDirectory(getenv func(string) string) (string, error) {
	return directory(getenv, "XDG_CONFIG_HOME", ".config", "unreal-agent")
}

func SessionDirectory(getenv func(string) string) (string, error) {
	return directory(getenv, "XDG_STATE_HOME", filepath.Join(".local", "state"), filepath.Join("unreal-agent", "sessions"))
}

func directory(getenv func(string) string, variable, fallback, subdirectory string) (string, error) {
	if root := getenv(variable); filepath.IsAbs(root) {
		return filepath.Join(root, subdirectory), nil
	}
	home := getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w; set an absolute %s or HOME", err, variable)
		}
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("set an absolute %s or HOME", variable)
	}
	return filepath.Join(home, fallback, subdirectory), nil
}
