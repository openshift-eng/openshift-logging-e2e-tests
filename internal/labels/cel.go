package labels

import (
	"fmt"
	"strings"
)

// Has returns a CEL expression selecting specs that carry every given label.
// Suite qualifiers evaluate it against spec.Labels, so suite membership does
// not depend on bracket-tag substrings in test names. Compose expressions
// with extensiontests.And/Or.
func Has(names ...string) string {
	exprs := make([]string, len(names))
	for i, label := range names {
		exprs[i] = fmt.Sprintf("labels.exists(l, l == %q)", label)
	}
	return strings.Join(exprs, " && ")
}

// Lacks returns a CEL expression selecting specs that carry none of the given
// labels.
func Lacks(names ...string) string {
	exprs := make([]string, len(names))
	for i, label := range names {
		exprs[i] = "!" + Has(label)
	}
	return strings.Join(exprs, " && ")
}
