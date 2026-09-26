package cappedhttp

import (
	"context"
	"fmt"
	"time"
)

// ResponseTooLargeError reports that a response body exceeded a configured
// byte limit. Wire is true when the limit that tripped is MaxCompressedBytes
// (bytes read off the wire, before decoding); Wire is false when it is
// MaxBytes (bytes handed to the caller, after decoding).
type ResponseTooLargeError struct {
	Limit    int64
	Observed int64
	Wire     bool
}

// Error implements the error interface for ResponseTooLargeError.
func (e *ResponseTooLargeError) Error() string {
	kind := "decoded"
	if e.Wire {
		kind = "wire"
	}
	return fmt.Sprintf("cappedhttp: response exceeded %s limit of %d bytes (observed %d)", kind, e.Limit, e.Observed)
}

// CompressionBombError reports that the ratio of decoded bytes to wire bytes
// exceeded MaxRatio, once RatioGraceBytes worth of decoded output had been
// seen. CompressedBytes and DecodedBytes are the counts observed at the
// moment the check tripped, not the final totals of a full response.
type CompressionBombError struct {
	Ratio           float64
	Limit           float64
	CompressedBytes int64
	DecodedBytes    int64
}

// Error implements the error interface for CompressionBombError.
func (e *CompressionBombError) Error() string {
	return fmt.Sprintf("cappedhttp: compression ratio %.1fx exceeded limit %.1fx (%d wire bytes decoded to %d)",
		e.Ratio, e.Limit, e.CompressedBytes, e.DecodedBytes)
}

// RequestTimeoutError reports that Options.Timeout elapsed before the
// request — including any redirects and the full body transfer — completed.
type RequestTimeoutError struct {
	Timeout time.Duration
}

// Error implements the error interface for RequestTimeoutError.
func (e *RequestTimeoutError) Error() string {
	return fmt.Sprintf("cappedhttp: request exceeded timeout of %s", e.Timeout)
}

// Unwrap lets errors.Is(err, context.DeadlineExceeded) succeed for a
// RequestTimeoutError, since that is what actually caused it under the hood.
func (e *RequestTimeoutError) Unwrap() error {
	return context.DeadlineExceeded
}

// TooManyRedirectsError reports that the request followed more redirects
// than Options.MaxRedirects allows.
type TooManyRedirectsError struct {
	MaxRedirects int
}

// Error implements the error interface for TooManyRedirectsError.
func (e *TooManyRedirectsError) Error() string {
	return fmt.Sprintf("cappedhttp: stopped after %d redirects", e.MaxRedirects)
}

// UnsupportedEncodingError reports a Content-Encoding this client has no
// decoder for. A response with such an encoding is refused outright — it is
// never handed back to the caller still compressed as though it were the
// decoded body.
type UnsupportedEncodingError struct {
	Encoding  string
	Supported []string
}

// Error implements the error interface for UnsupportedEncodingError.
func (e *UnsupportedEncodingError) Error() string {
	return fmt.Sprintf("cappedhttp: unsupported content-encoding %q (supported: %v)", e.Encoding, e.Supported)
}
