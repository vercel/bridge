package vercel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrTokenNotFound = errors.New("vercel token not found")

// ResolveToken returns a Vercel API token from VERCEL_TOKEN or the Vercel CLI auth file.
func ResolveToken() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve vercel token: get home dir: %w", err)
	}
	return resolveToken(os.Getenv, os.ReadFile, homeDir)
}

func resolveToken(
	getenv func(string) string,
	readFile func(string) ([]byte, error),
	homeDir string,
) (string, error) {
	if token := getenv("VERCEL_TOKEN"); token != "" {
		return token, nil
	}

	for _, path := range authConfigPaths(homeDir) {
		data, err := readFile(path)
		if err != nil {
			continue
		}

		var auth struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(data, &auth); err != nil {
			continue
		}
		if auth.Token != "" {
			return auth.Token, nil
		}
	}

	return "", ErrTokenNotFound
}

func authConfigPaths(homeDir string) []string {
	return []string{
		filepath.Join(homeDir, ".local", "share", "com.vercel.cli", "auth.json"),
		filepath.Join(homeDir, "Library", "Application Support", "com.vercel.cli", "auth.json"),
		filepath.Join(homeDir, ".config", "com.vercel.cli", "auth.json"),
	}
}
