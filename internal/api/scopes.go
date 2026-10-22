package api

import (
	"fmt"
	"strings"
)

// Permission names. A key holds a set of these; together they are everything a
// key can do, which is deliberately less than a browser login can do.
const (
	scopeInbound = "inbound" // may POST /api/inbound
	scopeRead    = "read"    // may read inboxes and messages
)

// permissionsDefault is what a key created without a permission list gets. Keys
// predate scoping and were used both ways, so "both" is the honest default.
const permissionsDefault = scopeInbound + "," + scopeRead

// parsePermissions validates and canonicalises a permission list from a create
// request: deduped, ordered inbound,read. Unknown or empty input is an error.
func parsePermissions(in []string) (string, error) {
	if len(in) == 0 {
		return "", fmt.Errorf("permissions must not be empty")
	}

	var inbound, read bool
	for _, raw := range in {
		switch strings.TrimSpace(raw) {
		case scopeInbound:
			inbound = true
		case scopeRead:
			read = true
		default:
			return "", fmt.Errorf("unknown permission %q, want %q or %q", raw, scopeInbound, scopeRead)
		}
	}

	set := make([]string, 0, 2)
	if inbound {
		set = append(set, scopeInbound)
	}
	if read {
		set = append(set, scopeRead)
	}
	// Unreachable through the API: the handler rejects an empty array before
	// parsing. Storing a key that can do nothing would be worse than failing.
	if len(set) == 0 {
		return "", fmt.Errorf("permissions must not be empty")
	}
	return strings.Join(set, ","), nil
}

// hasPermission reports whether the canonical set contains want. The column is
// a comma-separated list, so an exact element match is required: a substring
// match would let "readonly" satisfy "read".
func hasPermission(set, want string) bool {
	for _, p := range strings.Split(set, ",") {
		if strings.TrimSpace(p) == want {
			return true
		}
	}
	return false
}

// splitPermissions turns the stored column back into a slice for JSON. It never
// returns nil, so an empty set serialises as [] and not null.
func splitPermissions(set string) []string {
	if strings.TrimSpace(set) == "" {
		return []string{}
	}
	return strings.Split(set, ",")
}
