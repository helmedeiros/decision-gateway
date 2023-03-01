package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/helmedeiros/decision-gateway/internal/gateway"
)

// routeFlagList implements flag.Value so --route is repeatable on
// the command line. Each occurrence is one spec of the form
// PREFIX=>BACKEND_URL, e.g. /decide=>http://markup-svc:8080. The
// arrow separator (=>) gives operators a visual cue distinct from
// the value's `=` characters (URL credentials, query strings).
type routeFlagList []string

// String implements flag.Value.
func (r *routeFlagList) String() string {
	return strings.Join(*r, ",")
}

// Set implements flag.Value -- appends each --route occurrence.
func (r *routeFlagList) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// parseRouteFlags turns the operator-supplied --route values into a
// []gateway.Route. Each spec must contain the => separator with a
// non-empty prefix and a parseable backend URL. The backend URL
// must have a scheme (http/https) and a host; a bare host string
// or a missing scheme fails boot with a message naming the
// offending spec.
func parseRouteFlags(specs []string) ([]gateway.Route, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("--route is required at least once (format: PREFIX=>BACKEND_URL)")
	}
	routes := make([]gateway.Route, 0, len(specs))
	for _, spec := range specs {
		prefix, backendSpec, ok := splitArrow(spec)
		if !ok {
			return nil, fmt.Errorf("--route %q: missing '=>' separator (want PREFIX=>BACKEND_URL)", spec)
		}
		if prefix == "" {
			return nil, fmt.Errorf("--route %q: prefix is empty", spec)
		}
		if !strings.HasPrefix(prefix, "/") {
			return nil, fmt.Errorf("--route %q: prefix must start with '/'", spec)
		}
		backend, err := url.Parse(backendSpec)
		if err != nil {
			return nil, fmt.Errorf("--route %q: backend %q is not a valid URL: %w", spec, backendSpec, err)
		}
		if backend.Scheme == "" {
			return nil, fmt.Errorf("--route %q: backend %q has no scheme (want http:// or https://)", spec, backendSpec)
		}
		if backend.Host == "" {
			return nil, fmt.Errorf("--route %q: backend %q has no host", spec, backendSpec)
		}
		routes = append(routes, gateway.Route{Prefix: prefix, Backend: backend})
	}
	return routes, nil
}

// splitArrow splits spec on the first occurrence of "=>" and
// returns (prefix, backend, true). When "=>" is absent the third
// return is false and prefix/backend are empty.
func splitArrow(spec string) (string, string, bool) {
	idx := strings.Index(spec, "=>")
	if idx < 0 {
		return "", "", false
	}
	return spec[:idx], spec[idx+2:], true
}
