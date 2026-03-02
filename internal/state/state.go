// Package state manages persistent tunnel state in ~/.portal/tunnels.json.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StateFile is the top-level structure persisted to disk.
type StateFile struct {
	Tunnels []TunnelState `json:"tunnels"`
}

// TunnelState records metadata about a deployed tunnel.
type TunnelState struct {
	Name               string    `json:"name"`
	SourceContext      string    `json:"source_context"`
	DestinationContext string    `json:"destination_context"`
	Namespace          string    `json:"namespace"`
	TunnelPort         int       `json:"tunnel_port"`
	CreatedAt          time.Time `json:"created_at"`
	CACertPath         string    `json:"ca_cert_path,omitempty"`
	Mode               string    `json:"mode"`
	Services           []string  `json:"services"`
}

// Store provides thread-safe CRUD operations on the tunnel state file.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore creates a Store backed by the given file path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// DefaultDir returns ~/.portal, creating it with 0700 if needed.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".portal")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", dir, err)
	}
	return dir, nil
}

// DefaultPath returns ~/.portal/tunnels.json.
func DefaultPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve default path: %w", err)
	}
	return filepath.Join(dir, "tunnels.json"), nil
}

// Load reads and parses the state file. Returns an empty StateFile if the file does not exist.
func (s *Store) Load() (*StateFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

// Save atomically writes the state file to disk using a temp file and rename.
func (s *Store) Save(sf *StateFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(sf)
}

// Add appends a tunnel to the state file. Returns an error if a tunnel with the same name exists.
func (s *Store) Add(t TunnelState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sf, err := s.loadLocked()
	if err != nil {
		return fmt.Errorf("loading state for add: %w", err)
	}

	for _, existing := range sf.Tunnels {
		if existing.Name == t.Name {
			return fmt.Errorf("tunnel %q already exists", t.Name)
		}
	}

	sf.Tunnels = append(sf.Tunnels, t)
	if err := s.saveLocked(sf); err != nil {
		return fmt.Errorf("saving state after add: %w", err)
	}
	return nil
}

// Remove deletes a tunnel by name. Returns an error if not found.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sf, err := s.loadLocked()
	if err != nil {
		return fmt.Errorf("loading state for remove: %w", err)
	}

	idx := -1
	for i, t := range sf.Tunnels {
		if t.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("tunnel %q not found", name)
	}

	sf.Tunnels = append(sf.Tunnels[:idx], sf.Tunnels[idx+1:]...)
	if err := s.saveLocked(sf); err != nil {
		return fmt.Errorf("saving state after remove: %w", err)
	}
	return nil
}

// Get returns the tunnel with the given name, or nil if not found.
func (s *Store) Get(name string) (*TunnelState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sf, err := s.loadLocked()
	if err != nil {
		return nil, fmt.Errorf("loading state for get: %w", err)
	}

	for i := range sf.Tunnels {
		if sf.Tunnels[i].Name == name {
			return &sf.Tunnels[i], nil
		}
	}
	return nil, nil
}

// List returns all tunnels.
func (s *Store) List() ([]TunnelState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sf, err := s.loadLocked()
	if err != nil {
		return nil, fmt.Errorf("loading state for list: %w", err)
	}
	return sf.Tunnels, nil
}

// loadLocked reads the state file without acquiring the mutex. Caller must hold s.mu.
func (s *Store) loadLocked() (*StateFile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &StateFile{}, nil
		}
		return nil, fmt.Errorf("failed to read state file %s: %w", s.path, err)
	}

	var sf StateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("failed to parse state file %s: %w", s.path, err)
	}
	return &sf, nil
}

// saveLocked writes the state file atomically without acquiring the mutex. Caller must hold s.mu.
func (s *Store) saveLocked(sf *StateFile) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "tunnels-*.json.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	return nil
}
