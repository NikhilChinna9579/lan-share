//go:build !windows

package files

import (
	"os"
	"path/filepath"
)

// standardFolders on Linux/macOS (development and CI): the usual folders in home.
func standardFolders() []Root {
	home, _ := os.UserHomeDir()
	return []Root{
		{Key: "downloads", Label: "Downloads", Path: filepath.Join(home, "Downloads")},
		{Key: "documents", Label: "Documents", Path: filepath.Join(home, "Documents")},
		{Key: "desktop", Label: "Desktop", Path: filepath.Join(home, "Desktop")},
		{Key: "pictures", Label: "Pictures", Path: filepath.Join(home, "Pictures")},
		{Key: "videos", Label: "Videos", Path: filepath.Join(home, "Videos")},
		{Key: "music", Label: "Music", Path: filepath.Join(home, "Music")},
	}
}

func isHidden(_, name string) bool { return len(name) > 0 && name[0] == '.' }
