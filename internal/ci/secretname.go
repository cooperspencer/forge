package ci

import (
	"fmt"
	"strings"
)

// NormalizeSecretName upper-cases a secret name and checks it is usable as an
// environment variable: [A-Z_][A-Z0-9_]*, at most 100 characters, and not in
// the namespace forge sets itself (FORGE_*, CI).
func NormalizeSecretName(name string) (string, error) {
	n := strings.ToUpper(strings.TrimSpace(name))
	if n == "" {
		return "", fmt.Errorf("secret name is required")
	}
	if len(n) > 100 {
		return "", fmt.Errorf("secret name %q is too long (max 100 characters)", name)
	}
	for i, r := range n {
		ok := r == '_' || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return "", fmt.Errorf("invalid secret name %q: use letters, digits and _, not starting with a digit", name)
		}
	}
	if n == "CI" || strings.HasPrefix(n, "FORGE_") || strings.HasPrefix(n, "MATRIX_") {
		return "", fmt.Errorf("secret name %q is reserved (CI, FORGE_*, MATRIX_*)", name)
	}
	return n, nil
}
