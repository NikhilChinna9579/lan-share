//go:build windows

package identity

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// On Windows the identity (certificate + private key) is encrypted with DPAPI for the
// signed-in Windows user: another user, or the file copied to another PC, can't use it.
// An older plain identity.pem is converted the first time and then deleted.

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtect   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32.NewProc("LocalFree")
)

type blob struct {
	n    uint32
	data *byte
}

func newBlob(d []byte) *blob {
	if len(d) == 0 {
		return &blob{}
	}
	return &blob{n: uint32(len(d)), data: &d[0]}
}

func (b *blob) bytes() []byte {
	out := make([]byte, b.n)
	copy(out, unsafe.Slice(b.data, b.n))
	return out
}

const cryptprotectUIForbidden = 0x1

var entropy = []byte("LAN Share identity v1")

func protect(plain []byte) ([]byte, error) {
	var out blob
	r, _, err := procCryptProtect.Call(uintptr(unsafe.Pointer(newBlob(plain))), 0, uintptr(unsafe.Pointer(newBlob(entropy))),
		0, 0, cryptprotectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	return out.bytes(), nil
}

func unprotect(sealed []byte) ([]byte, error) {
	var out blob
	r, _, err := procCryptUnprotect.Call(uintptr(unsafe.Pointer(newBlob(sealed))), 0, uintptr(unsafe.Pointer(newBlob(entropy))),
		0, 0, cryptprotectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	return out.bytes(), nil
}

func load(dir string) ([]byte, error) {
	sealed, err := os.ReadFile(filepath.Join(dir, "identity.dpapi"))
	if err == nil {
		return unprotect(sealed)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// From an older version: convert, then remove the plain copy.
	plain, err := os.ReadFile(filepath.Join(dir, "identity.pem"))
	if err != nil {
		return nil, err
	}
	if save(dir, plain) == nil {
		os.Remove(filepath.Join(dir, "identity.pem"))
	}
	return plain, nil
}

func save(dir string, data []byte) error {
	sealed, err := protect(data)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "identity.dpapi"), sealed, 0o600)
}

// Protected says whether the key is kept in the system's protected storage.
func Protected() bool { return true }
