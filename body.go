package cappedhttp

import (
	"compress/gzip"
	"fmt"
	"io"
	"strings"
)

// supportedEncodings lists the Content-Encoding values this package can
// decode. Anything else in a response's Content-Encoding header is refused
// with UnsupportedEncodingError rather than handed back still compressed.
var supportedEncodings = []string{"gzip", "identity"}

// wireCounter wraps the raw, still-compressed response body. Every Read
// counts bytes as they come off the wire and refuses to hand back more than
// maxCompressedBytes, closing the underlying body the instant the cap trips
// so nothing further is read from the socket.
type wireCounter struct {
	body  io.ReadCloser
	limit int64 // <=0 disables the check
	n     int64
}

func (w *wireCounter) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	w.n += int64(n)
	if w.limit > 0 && w.n > w.limit {
		w.body.Close()
		return 0, &ResponseTooLargeError{Limit: w.limit, Observed: w.n, Wire: true}
	}
	return n, err
}

// limitedBody is what Transport hands back as http.Response.Body for a
// single request/response hop. It reads through the decoder (if any), counts
// decoded bytes, and enforces MaxBytes and the live compression ratio on
// every Read — aborting and closing the underlying connection the instant a
// limit trips, before any offending bytes are returned to the caller.
type limitedBody struct {
	underlying io.ReadCloser // the raw network body; Close tears down the connection
	wire       *wireCounter
	decoded    io.Reader // wire itself (identity) or a decoder reading from wire

	opts resolved
	n    int64 // decoded bytes handed back so far

	closeErr error
	closed   bool
}

// newLimitedBody builds the decode-and-meter chain for one response.
// contentEncoding is the response's Content-Encoding header, verbatim.
func newLimitedBody(body io.ReadCloser, contentEncoding string, o resolved) (*limitedBody, error) {
	wc := &wireCounter{body: body, limit: o.maxCompressedBytes}

	enc := strings.TrimSpace(contentEncoding)
	var decoded io.Reader
	switch strings.ToLower(enc) {
	case "", "identity":
		decoded = wc
	case "gzip":
		zr, err := gzip.NewReader(wc)
		if err != nil {
			body.Close()
			return nil, fmt.Errorf("cappedhttp: opening gzip stream: %w", err)
		}
		decoded = zr
	default:
		body.Close()
		return nil, &UnsupportedEncodingError{Encoding: enc, Supported: supportedEncodings}
	}

	return &limitedBody{
		underlying: body,
		wire:       wc,
		decoded:    decoded,
		opts:       o,
	}, nil
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, io.EOF
	}

	n, err := b.decoded.Read(p)
	if n > 0 {
		b.n += int64(n)

		if b.opts.maxBytes > 0 && b.n > b.opts.maxBytes {
			b.abort()
			return 0, &ResponseTooLargeError{Limit: b.opts.maxBytes, Observed: b.n, Wire: false}
		}

		if !b.opts.ratioDisabled() && b.n > b.opts.ratioGraceBytes {
			wire := b.wire.n
			if wire > 0 {
				ratio := float64(b.n) / float64(wire)
				if ratio > b.opts.maxRatio {
					b.abort()
					return 0, &CompressionBombError{
						Ratio:           ratio,
						Limit:           b.opts.maxRatio,
						CompressedBytes: wire,
						DecodedBytes:    b.n,
					}
				}
			}
		}
	}
	return n, err
}

func (b *limitedBody) abort() {
	if !b.closed {
		b.underlying.Close()
	}
}

func (b *limitedBody) Close() error {
	if b.closed {
		return b.closeErr
	}
	b.closed = true
	b.closeErr = b.underlying.Close()
	return b.closeErr
}
