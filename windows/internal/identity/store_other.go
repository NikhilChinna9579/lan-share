//go:build !windows

package identity

import (
	"os"
	"path/filepath"
)

// On Linux/macOS (development, tests) the key is a file only this user can read.
func load(dir string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, "identity.pem")) }

func save(dir string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, "identity.pem"), data, 0o600)
}

// Protected says whether the key is kept in the system's protected storage.
func Protected() bool { return false }
