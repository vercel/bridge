// Package identity manages the persistent device identity for the bridge CLI.
// Each machine is assigned a unique KSUID on first use, stored in ~/.bridge/device-id.txt.
package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/segmentio/ksuid"
)

const (
	// bridgeDir is the directory under the user's home where bridge config is stored.
	bridgeDir = ".bridge"
	// deviceIDFile is the filename containing the device KSUID.
	deviceIDFile = "device-id.txt"
)

// EnsureDeviceID reads the device ID from ~/.bridge/device-id.txt.
// If the file does not exist, it generates a new KSUID, creates the file
// (and the ~/.bridge directory if needed), and returns the new ID.
// This should be called as early CLI middleware so every command can assume
// the device ID exists.
func EnsureDeviceID() (string, error) {
	path, err := deviceIDPath()
	if err != nil {
		return "", err
	}

	// Try to read existing ID — lowercase for Kubernetes namespace compatibility.
	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.ToLower(strings.TrimSpace(string(data)))
		if id != "" {
			return id, nil
		}
	}

	// Generate new KSUID — lowercase for Kubernetes namespace compatibility.
	id := strings.ToLower(ksuid.New().String())

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create %s directory: %w", dir, err)
	}

	// Write device ID
	if err := os.WriteFile(path, []byte(id+"\n"), 0644); err != nil {
		return "", fmt.Errorf("failed to write device ID: %w", err)
	}

	return id, nil
}

// GetDeviceID reads the device ID from ~/.bridge/device-id.txt.
// Returns an error if the file does not exist.
func GetDeviceID() (string, error) {
	path, err := deviceIDPath()
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("device ID not found at %s: %w (run any bridge command first to generate it)", path, err)
	}

	id := strings.ToLower(strings.TrimSpace(string(data)))
	if id == "" {
		return "", fmt.Errorf("device ID file is empty at %s", path)
	}

	return id, nil
}

// ShortDeviceID returns the first 6 characters of a device KSUID.
func ShortDeviceID(deviceID string) string {
	if len(deviceID) > 6 {
		return deviceID[:6]
	}
	return deviceID
}

func deviceIDPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(home, bridgeDir, deviceIDFile), nil
}
