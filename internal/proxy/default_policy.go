package proxy

import _ "embed"

//go:embed default-policy.json
var defaultPolicy string

// DefaultPolicy returns a standalone read-only policy without environment settings.
func DefaultPolicy() string {
	return defaultPolicy
}
