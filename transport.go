package cappedhttp

import (
	"crypto/tls"
	"fmt"
	"net/http"
)

// Transport is an http.RoundTripper that meters wire bytes and decoded bytes
// separately for every response it handles, refusing to hand back more than
// Options.MaxBytes decoded, Options.MaxCompressedBytes on the wire, or a
// decoded÷wire ratio above Options.MaxRatio — enforced as the body streams,
// not after the fact.
//
// # Why Transport decodes the body itself
//
// net/http's own Transport will, by default, add "Accept-Encoding: gzip" to
// a request that sets none and transparently gunzip the response for you
// (see Transport.DisableCompression's doc comment). That auto-decoding runs
// inside net/http, ahead of any RoundTripper wrapper or response body
// wrapper application code can install, and — confirmed by reading
// gzipReader.Read in the Go 1.27.1 standard library's
// src/net/http/transport.go (around line 3369) — it applies no byte counter
// and no size limit: it is a straight call to the underlying gzip.Reader's
// Read. There is no hook to observe how many compressed bytes produced a
// given number of decoded bytes, which is exactly the information a
// wire-vs-decoded ratio check needs. So Transport builds its default base
// *http.Transport with DisableCompression: true and sets its own
// Accept-Encoding header on every request, keeping the response body
// compressed until Transport's own decoder — gzip.NewReader wrapped around a
// counting reader — reads it. That is the only way to see both sizes.
type Transport struct {
	opts resolved
	base http.RoundTripper
}

// NewTransport returns a Transport configured with o. A nil o.Base gets a
// fresh *http.Transport cloned from http.DefaultTransport with
// DisableCompression set to true.
func NewTransport(o Options) *Transport {
	base := o.Base
	if base == nil {
		base = defaultBaseTransport()
	}
	return &Transport{opts: resolve(o), base: base}
}

func defaultBaseTransport() *http.Transport {
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Extremely unlikely in practice, but fall back to an equivalent
		// transport rather than panicking.
		return &http.Transport{DisableCompression: true, TLSClientConfig: &tls.Config{}}
	}
	t := dt.Clone()
	t.DisableCompression = true
	return t
}

// RoundTrip implements http.RoundTripper. It sends the request (adding
// Accept-Encoding unless the caller set one or opted out), then wraps the
// response body in the wire/decoded metering chain before returning.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.opts.disableAcceptEncoding && req.Header.Get("Accept-Encoding") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Accept-Encoding", t.opts.acceptEncoding)
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if resp.Uncompressed {
		// The base RoundTripper transparently decompressed the body
		// itself (Transport.DisableCompression was false on it and no
		// Accept-Encoding reached it), which means the wire byte count
		// cappedhttp would report is meaningless — the bytes it sees
		// from resp.Body are already decoded. Fail loudly instead of
		// silently enforcing a weaker cap.
		resp.Body.Close()
		return nil, fmt.Errorf("cappedhttp: base RoundTripper auto-decompressed the response; " +
			"set DisableCompression: true on any custom Options.Base so cappedhttp can meter wire bytes itself")
	}

	body, err := newLimitedBody(resp.Body, resp.Header.Get("Content-Encoding"), t.opts)
	if err != nil {
		return nil, err
	}
	resp.Body = body
	if resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		// We decoded it ourselves; present the response the way
		// net/http's own transparent gzip handling does, so callers
		// that inspect these fields aren't misled about what
		// resp.Body now yields.
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Uncompressed = true
	}
	return resp, nil
}

var _ http.RoundTripper = (*Transport)(nil)
