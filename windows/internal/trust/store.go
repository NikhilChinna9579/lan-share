// Package trust keeps the list of paired devices on disk (trust.json).
package trust

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Device is one paired device.
type Device struct {
	ID       string    `json:"id"` // key fingerprint
	Name     string    `json:"name"`
	PairedAt time.Time `json:"paired_at"`
}

// Store is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	path    string
	devices map[string]Device
}

// Open loads dir/trust.json, or starts empty if it doesn't exist.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "trust.json"), devices: map[string]Device{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Device
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, d := range list {
		s.devices[d.ID] = d
	}
	return s, nil
}

// Get returns the device with this fingerprint, if paired.
func (s *Store) Get(id string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	return d, ok
}

// Add saves a newly paired device (replacing one with the same ID).
func (s *Store) Add(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[d.ID] = d
	return s.save()
}

// List returns all paired devices.
func (s *Store) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	return out
}

func (s *Store) save() error {
	list := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		list = append(list, d)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	// Write then rename, so a crash never leaves a half-written trust file.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
