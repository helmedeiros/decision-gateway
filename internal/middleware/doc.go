// Package middleware ships the cross-cutting HTTP concerns the
// gateway applies to every request: correlation-ID propagation and
// structured access logging. See decision-gateway/ADR-0001.
//
// # Composition order
//
// Wire CorrelationID OUTSIDE AccessLog. The access log's per-request
// fields include the correlation ID; AccessLog reads it via
// CorrelationIDFromContext on the inbound request, which only carries
// the ID if a middleware above AccessLog has stamped it on the
// request's context. (Go's http.Request.WithContext does not
// propagate context mutations back out of an inner frame; AccessLog
// reads the request context AFTER next.ServeHTTP returns, so the
// stamping has to happen one frame up.)
//
//	handler := middleware.CorrelationID(
//	    middleware.AccessLog(out, nil,
//	        router))
//
// The matched-route value flows the opposite way -- inner handlers
// stamp it on the response writer via the RouteRecorder type
// assertion, and AccessLog reads it off the writer wrapper after
// next.ServeHTTP returns. See accesslog.go's RouteRecorder doc.
package middleware
