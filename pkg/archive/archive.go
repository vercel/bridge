// Package archive provides utilities for packing and unpacking tar.gz archives.
package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// PackGlobFiles walks fsys and packs every regular file whose base name
// matches the glob pattern into a tar.gz archive. The walk is recursive,
// so pattern "*.yaml" matches YAML files at any depth.
func PackGlobFiles(fsys fs.FS, pattern string) ([]byte, error) {
	// Validate the pattern early.
	if _, err := path.Match(pattern, ""); err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
	}

	var matches []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		matched, err := path.Match(pattern, path.Base(p))
		if err != nil {
			return err
		}
		if matched {
			matches = append(matches, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk filesystem: %w", err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no files matched pattern %q", pattern)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, name := range matches {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("failed to read %q: %w", name, err)
		}

		header := &tar.Header{
			Name: name,
			Size: int64(len(data)),
			Mode: 0644,
		}
		if err := tw.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("failed to write tar header for %q: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("failed to write tar data for %q: %w", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}

	return buf.Bytes(), nil
}

// Unpack extracts a tar.gz archive and returns a map of filename to file
// contents. It validates against path traversal attacks.
func Unpack(data []byte) (map[string][]byte, error) {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	result := make(map[string][]byte)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read tar entry: %w", err)
		}

		// Validate against path traversal.
		name := filepath.Clean(header.Name)
		if strings.Contains(name, "..") {
			return nil, fmt.Errorf("path traversal detected in archive entry %q", header.Name)
		}
		if filepath.IsAbs(name) {
			return nil, fmt.Errorf("absolute path detected in archive entry %q", header.Name)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}

		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("failed to read archive entry %q: %w", name, err)
		}
		result[name] = content
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("archive contains no files")
	}

	return result, nil
}
