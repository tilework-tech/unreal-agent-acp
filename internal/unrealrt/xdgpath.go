// VENDORED from github.com/unreallabsai/unreal-agent@55747f5 (MIT; see third_party/unreal-agent/LICENSE)
//   cmd/internal/xdgpath/directory.go
// Modified: package name; ConfigDirectory, SessionDirectory and directory
// renamed configDirectory, sessionDirectory and xdgDirectory. Merge-resolve on
// every upstream bump; see VENDORED.md.
// Delete once upstream exposes a public runtime package.

package unrealrt

import (
	"fmt"
	"os"
	"path/filepath"
)

func configDirectory(getenv func(string) string) (string, error) {
	return xdgDirectory(getenv, "XDG_CONFIG_HOME", ".config", "unreal-agent")
}

func sessionDirectory(getenv func(string) string) (string, error) {
	return xdgDirectory(getenv, "XDG_STATE_HOME", filepath.Join(".local", "state"), filepath.Join("unreal-agent", "sessions"))
}

func xdgDirectory(getenv func(string) string, variable, fallback, subdirectory string) (string, error) {
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
