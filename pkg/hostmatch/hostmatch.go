// Package hostmatch provides glob-style hostname matching.
package hostmatch

import "strings"

// Match checks if hostname matches a wildcard pattern.
// Supports * as a wildcard that matches any single label,
// and ** to match across multiple labels.
//
// Examples:
//   - "*" matches everything
//   - "flags.vercel.com" matches exactly "flags.vercel.com"
//   - "*.example.com" matches "foo.example.com" but not "bar.foo.example.com"
//   - "**.example.com" matches "foo.example.com" and "bar.foo.example.com"
//   - "api.*.internal" matches "api.foo.internal"
func Match(pattern, hostname string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	hostname = strings.ToLower(strings.TrimSpace(hostname))

	if pattern == "*" {
		return true
	}

	if pattern == hostname {
		return true
	}

	// Handle ** (matches multiple labels)
	if strings.HasPrefix(pattern, "**.") {
		suffix := pattern[2:] // Include the dot: ".example.com"
		return strings.HasSuffix(hostname, suffix) || hostname == pattern[3:]
	}

	// Handle single * wildcard
	patternParts := strings.Split(pattern, ".")
	hostParts := strings.Split(hostname, ".")

	if len(patternParts) != len(hostParts) {
		return false
	}

	for i, pp := range patternParts {
		if pp == "*" {
			continue
		}
		if pp != hostParts[i] {
			return false
		}
	}

	return true
}
