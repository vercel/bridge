// Package session manages local bridge session files stored in ~/.bridge/sessions/.
// Each session is a JSON-serialized protobuf message written when a bridge is created
// and removed when the bridge is torn down.
package session

import (
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

const sessionsDir = "sessions"

// Save writes a session file to ~/.bridge/sessions/<name>.json.
func Save(name, devcontainerConfigPath string) error {
	dir, err := sessionDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create sessions directory: %w", err)
	}

	s := &bridgev1.Session{
		Name:                   name,
		TimeCreated:            timestamppb.Now(),
		DevcontainerConfigPath: devcontainerConfigPath,
	}

	data, err := protojson.Marshal(s)
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}

	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write session file: %w", err)
	}
	return nil
}

// Load reads a session file for the given bridge name.
func Load(name string) (*bridgev1.Session, error) {
	dir, err := sessionDir()
	if err != nil {
		return nil, err
	}

	path := filepath.Join(dir, name+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session not found for %q: %w", name, err)
	}

	s := &bridgev1.Session{}
	if err := protojson.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}
	return s, nil
}

// Delete removes the session file for the given bridge name.
// It is not an error if the file does not exist.
func Delete(name string) error {
	dir, err := sessionDir()
	if err != nil {
		return err
	}

	path := filepath.Join(dir, name+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete session file: %w", err)
	}
	return nil
}

func sessionDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, ".bridge", sessionsDir), nil
}
