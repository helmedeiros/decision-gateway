// Package proxy ships the reverse-proxy adapter that turns a
// gateway.Router into an http.Handler. The Handler matches the
// inbound request path against the configured routes, selects the
// longest-prefix route, propagates the correlation ID on the
// outbound request to the backend, stamps the matched-route prefix
// on the writer wrapper so the access-log middleware reports it,
// and forwards via httputil.ReverseProxy. Misses (no route
// matched) return 404 with an opaque body.
//
// The package owns the integration between the routing decision
// (gateway.Router) and the cross-cutting middleware
// (middleware.RouteRecorder); the middlewares and the router stay
// independent of each other.
package proxy
