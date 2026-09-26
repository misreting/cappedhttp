package cappedhttp

import (
	"math"
	"net/http"
	"time"
)

// Default limits applied by New when the corresponding Options field is left
// at its zero value.
const (
	// DefaultMaxBytes is the default cap on decoded response body bytes.
	DefaultMaxBytes int64 = 10 << 20 // 10 MiB

	// DefaultMaxRatio is the default cap on decoded÷wire bytes.
	DefaultMaxRatio float64 = 100

	// DefaultRatioGraceBytes is the default number of decoded bytes seen
	// before the ratio check starts evaluating.
	DefaultRatioGraceBytes int64 = 1 << 20 // 1 MiB

	// DefaultTimeout is the default overall operation budget: connect,
	// redirects and body transfer, together.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRedirects is the default number of redirect hops followed.
	DefaultMaxRedirects = 5

	// DefaultAcceptEncoding is the Accept-Encoding header value sent when
	// Options.AcceptEncoding is left empty and DisableAcceptEncoding is
	// false. gzip is the only encoding this package can decode, so it is
	// the only one ever advertised or accepted.
	DefaultAcceptEncoding = "gzip"
)

// Options configures a Client. The zero value of every field falls back to
// the matching Default constant, except DisableAcceptEncoding and Base,
// whose zero values (false and nil) already mean "use the built-in
// behaviour".
type Options struct {
	// MaxBytes caps the response body size after decoding. Zero means
	// DefaultMaxBytes; use math.MaxInt64 to effectively disable it.
	MaxBytes int64

	// MaxCompressedBytes caps the bytes read off the wire, before
	// decoding. Zero means "use MaxBytes" (a reasonable floor: the wire
	// body is never larger than the decoded one for the encodings this
	// package supports, since gzip does not expand data enough to
	// matter). Use math.MaxInt64 to effectively disable it.
	MaxCompressedBytes int64

	// MaxRatio caps decoded bytes ÷ wire bytes, checked continuously once
	// RatioGraceBytes of decoded output has been seen. Zero means
	// DefaultMaxRatio. Set to math.Inf(1) to disable the ratio check
	// entirely and rely on MaxBytes/MaxCompressedBytes alone.
	MaxRatio float64

	// RatioGraceBytes is how many decoded bytes are allowed before the
	// ratio check starts being enforced. Small, legitimately-compressible
	// payloads (a repetitive JSON array, for instance) routinely compress
	// 100x or more; without a grace floor those are false positives. Zero
	// means DefaultRatioGraceBytes.
	RatioGraceBytes int64

	// Timeout bounds the whole operation: connecting, following
	// redirects, and reading the full response body. Zero means
	// DefaultTimeout; a negative value disables the timeout.
	Timeout time.Duration

	// MaxRedirects caps the number of redirect hops followed. Zero means
	// DefaultMaxRedirects; a negative value disables the redirect cap
	// (net/http's own internal limit of 10 still applies).
	MaxRedirects int

	// AcceptEncoding, if set, overrides the Accept-Encoding header sent
	// on outgoing requests that don't already set one. Leave it empty to
	// send DefaultAcceptEncoding. It should not name an encoding this
	// package cannot decode (currently only "gzip" and "identity"),
	// since the response would then be refused with
	// UnsupportedEncodingError.
	AcceptEncoding string

	// DisableAcceptEncoding, if true, sends no Accept-Encoding header at
	// all, leaving compression entirely up to the server's defaults (most
	// servers send identity, uncompressed, absent that header). A
	// response that arrives compressed anyway is still decoded and capped
	// normally, since Content-Encoding is honoured regardless of what was
	// requested.
	DisableAcceptEncoding bool

	// Base is the underlying http.RoundTripper used to make requests. If
	// nil, Client builds its own *http.Transport cloned from
	// http.DefaultTransport with DisableCompression set to true.
	//
	// If you supply your own Base, you are responsible for making sure it
	// does not itself perform transparent decompression: cappedhttp needs
	// the raw, still-compressed body to meter wire bytes and decoded
	// bytes separately. An *http.Transport with DisableCompression: false
	// will auto-decode ahead of cappedhttp whenever the request has no
	// Accept-Encoding of its own, which defeats wire metering; Client
	// detects this case (via http.Response.Uncompressed) and returns an
	// error rather than silently enforcing a weaker cap.
	Base http.RoundTripper
}

// resolved is Options after defaults have been applied and validated once.
type resolved struct {
	maxBytes              int64
	maxCompressedBytes    int64
	maxRatio              float64
	ratioGraceBytes       int64
	timeout               time.Duration
	maxRedirects          int
	acceptEncoding        string
	disableAcceptEncoding bool
}

func resolve(o Options) resolved {
	r := resolved{
		maxBytes:              o.MaxBytes,
		maxCompressedBytes:    o.MaxCompressedBytes,
		maxRatio:              o.MaxRatio,
		ratioGraceBytes:       o.RatioGraceBytes,
		timeout:               o.Timeout,
		maxRedirects:          o.MaxRedirects,
		acceptEncoding:        o.AcceptEncoding,
		disableAcceptEncoding: o.DisableAcceptEncoding,
	}
	if r.maxBytes == 0 {
		r.maxBytes = DefaultMaxBytes
	}
	if r.maxCompressedBytes == 0 {
		r.maxCompressedBytes = r.maxBytes
	}
	if r.maxRatio == 0 {
		r.maxRatio = DefaultMaxRatio
	}
	if r.ratioGraceBytes == 0 {
		r.ratioGraceBytes = DefaultRatioGraceBytes
	}
	if o.Timeout == 0 {
		r.timeout = DefaultTimeout
	} else if o.Timeout < 0 {
		r.timeout = 0
	}
	if o.MaxRedirects == 0 {
		r.maxRedirects = DefaultMaxRedirects
	} else if o.MaxRedirects < 0 {
		r.maxRedirects = -1
	}
	if r.acceptEncoding == "" {
		r.acceptEncoding = DefaultAcceptEncoding
	}
	return r
}

func (r resolved) ratioDisabled() bool {
	return math.IsInf(r.maxRatio, 1)
}
