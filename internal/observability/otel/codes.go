package otel

import "go.opentelemetry.io/otel/codes"

// codeError returns the OTel codes.Error constant. Wrapped in a
// helper so the import does not leak into middleware.go's signature
// and the test file can stub it for the error-status branch.
func codeError() codes.Code { return codes.Error }
