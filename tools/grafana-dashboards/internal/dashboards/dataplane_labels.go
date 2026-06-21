package dashboards

import "strings"

func dataplaneLabels(extra ...string) string {
	pairs := []string{`namespace=~"$namespace"`, `engine=~"$engine"`}
	pairs = append(pairs, extra...)
	return "{" + strings.Join(pairs, ", ") + "}"
}

func dataplaneLogLabels(extra ...string) string {
	pairs := []string{`namespace=~"$namespace"`, `engine=~"$engine"`}
	pairs = append(pairs, extra...)
	return "{" + strings.Join(pairs, ", ") + "}"
}
