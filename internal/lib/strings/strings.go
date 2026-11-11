package strings

import (
	"strings"
)

// AnyContainSubstring checks if b is a's element's substring
func AnyContainSubstring(a []string, b string) bool {
	return strings.Contains(strings.Join(a, ","), b)
}
