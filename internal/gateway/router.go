package gateway

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Route maps a path prefix to a backend URL. cmd parses operator
// flags like --route=/decide=>http://markup-svc:8080 into Route
// values; the reverse-proxy adapter (a future commit) hands Backend
// to httputil.NewSingleHostReverseProxy on construction.
//
// Prefix matching is a literal string-prefix test on the request
// path. Two routes with overlapping prefixes ("/decide" and
// "/decide/v2") both match a "/decide/v2/foo" request; the Router
// picks the longest matching prefix. See Match for the dispatch.
type Route struct {
	Prefix  string
	Backend *url.URL
}

// Router selects a Route by longest-prefix match on the request
// path. Routes are stored in a slice sorted by prefix length
// descending so Match iterates the longest first and returns the
// first prefix match.
//
// Complexity is O(N) per Match call where N is the route count.
// For the expected v0.0.1 menu of a handful of routes this is
// sub-microsecond; a precomputed trie can land in a follow-up if
// the route list grows materially larger. See ADR-0001.
type Router struct {
	routes []Route
}

// NewRouter validates routes and returns a Router. Errors:
//
//   - "no routes configured" if routes is empty. Operators who want
//     a default-404 gateway today should pass at least one /healthz-
//     -only route once that handler ships; until then an empty
//     gateway is a boot error rather than a silent surprise.
//   - "route[i] prefix is empty" if any Route has Prefix == "".
//   - "route[i] backend is nil" if any Route has Backend == nil.
//   - "duplicate prefix %q at routes[i] and routes[j]" if two routes
//     share a prefix (operator typo).
//
// Routes are stored in length-descending order so Match's iteration
// returns the longest matching prefix first.
func NewRouter(routes []Route) (*Router, error) {
	if len(routes) == 0 {
		return nil, fmt.Errorf("no routes configured")
	}
	seen := make(map[string]int, len(routes))
	for i, r := range routes {
		if r.Prefix == "" {
			return nil, fmt.Errorf("route[%d] prefix is empty", i)
		}
		if r.Backend == nil {
			return nil, fmt.Errorf("route[%d] backend is nil", i)
		}
		if prev, ok := seen[r.Prefix]; ok {
			return nil, fmt.Errorf("duplicate prefix %q at routes[%d] and routes[%d]", r.Prefix, prev, i)
		}
		seen[r.Prefix] = i
	}

	sorted := make([]Route, len(routes))
	copy(sorted, routes)
	sort.SliceStable(sorted, func(i, j int) bool {
		// Length descending. Ties keep input order (sort.SliceStable).
		return len(sorted[i].Prefix) > len(sorted[j].Prefix)
	})
	return &Router{routes: sorted}, nil
}

// Match returns the longest-prefix matching Route for path or
// (Route{}, false) when no route matches.
func (r *Router) Match(path string) (Route, bool) {
	for _, route := range r.routes {
		if strings.HasPrefix(path, route.Prefix) {
			return route, true
		}
	}
	return Route{}, false
}

// Routes returns the configured Routes in length-descending order.
// Useful for the cmd's structured boot-log event and for tests
// that need to verify the router's internal ordering.
func (r *Router) Routes() []Route {
	out := make([]Route, len(r.routes))
	copy(out, r.routes)
	return out
}
