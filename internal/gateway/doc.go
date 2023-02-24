// Package gateway defines the domain types for the decision-gateway
// project: Route (path prefix to backend URL mapping) and Router
// (longest-prefix selection over a list of Routes). See ADR-0001.
//
// The package is pure -- no HTTP wiring, no I/O. The reverse-proxy
// adapter that consumes Router lands in internal/proxy in a future
// commit; this separation keeps the selection logic trivially
// testable and makes a future alternative router implementation
// (a prefix trie, a regex matcher, a host-based router) reachable
// without touching the proxy code.
//
// Router is immutable after construction; the routes slice is
// read-only post-NewRouter. Safe for concurrent use by many
// goroutines without external synchronization.
package gateway
