package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// randRead is package-level so tests can swap in a deterministic
// or always-failing reader and exercise the sentinel-UUID path
// without exposing the sentinel constant. Mirrors the markup-svc
// httpapi/correlationid.go pattern.
var randRead = rand.Read

// SetRandReadForTesting swaps the package-level random source and
// returns the previous one. Test-only -- production code never calls
// this -- but the function is exported so the test file in package
// middleware_test (a separate package) can reach it without dropping
// the external-test-package isolation the rest of the suite uses.
func SetRandReadForTesting(next func([]byte) (int, error)) func([]byte) (int, error) {
	prev := randRead
	randRead = next
	return prev
}

// CorrelationIDHeader is the wire-shape header read on inbound
// requests and stamped on outbound responses + downstream requests.
// The name matches markup-svc/internal/httpapi.CorrelationIDHeader
// so the value propagates end-to-end through the platform with no
// translation.
const CorrelationIDHeader = "X-Correlation-ID"

// correlationIDKey is the context key under which the resolved
// correlation ID is stashed. The unexported type prevents
// collision with any other package that might use a plain-string
// key. CorrelationIDFromContext is the exported reader.
type correlationIDKey struct{}

// CorrelationID wraps next so every request:
//
//  1. carries an X-Correlation-ID value in r.Context() reachable
//     via CorrelationIDFromContext (either the inbound header or a
//     newly minted UUID v4 when the header is absent or blank), and
//  2. has the value stamped on the response's X-Correlation-ID
//     header so the client can correlate its observability to the
//     gateway's logs.
//
// The downstream reverse-proxy adapter reads the value from the
// context and stamps it on the outbound request to the backend.
func CorrelationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationIDHeader)
		if id == "" {
			id = newUUID()
		}
		w.Header().Set(CorrelationIDHeader, id)
		ctx := context.WithValue(r.Context(), correlationIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// CorrelationIDFromContext returns the correlation ID stashed by
// the CorrelationID middleware or "" when the request never went
// through the middleware (e.g., direct unit-test invocations of a
// handler).
func CorrelationIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey{}).(string)
	return id
}

// newUUID returns a UUID v4 as a lowercase 8-4-4-4-12 hex string.
// crypto/rand is the source; setting the version (0x40) and
// variant (0x80) bits per RFC 4122 keeps the output recognizable
// as v4 to downstream consumers that classify by these bits. The
// gateway has no external UUID dependency at the v0.0.x baseline;
// rolling 30 lines here keeps the dependency surface empty.
func newUUID() string {
	var b [16]byte
	if _, err := randRead(b[:]); err != nil {
		// crypto/rand.Read returning an error is reserved for
		// pathological cases (the OS RNG is unreachable); the
		// process would not be functional in that state. Return
		// a fixed sentinel so logs at least record SOMETHING
		// rather than an empty string that downstream filters
		// might silently drop.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst)
}
