package cappedhttp_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentscope-ai-java/cappedhttp"
)

// gzipBytes compresses data at the given level and returns the compressed
// bytes, so tests can build responses with a known, real wire size and a
// known, real decoded size.
func gzipBytes(t *testing.T, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatalf("gzip.NewWriterLevel: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// repeatToSize returns pattern repeated until it is exactly n bytes long,
// which compresses extremely well — the raw material for a compression
// bomb, and also for the "small legitimately-compressible payload" the
// ratio grace floor exists for.
func repeatToSize(pattern string, n int) []byte {
	buf := make([]byte, 0, n+len(pattern))
	for len(buf) < n {
		buf = append(buf, pattern...)
	}
	return buf[:n]
}

func gzipServer(t *testing.T, body []byte, encoding string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if encoding != "" {
			w.Header().Set("Content-Encoding", encoding)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
}

func TestGet_PlainResponse(t *testing.T) {
	want := []byte("hello, world")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(want)
	}))
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestGet_GzipResponse_RoundTrips(t *testing.T) {
	want := []byte(strings.Repeat("round trip me\n", 1000))
	compressed := gzipBytes(t, want, gzip.BestCompression)
	srv := gzipServer(t, compressed, "gzip")
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{MaxBytes: 1 << 20})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded body did not round-trip: got %d bytes, want %d", len(got), len(want))
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding header should be stripped once cappedhttp has decoded the body, got %q",
			resp.Header.Get("Content-Encoding"))
	}
}

// TestContentLengthDoesNotBoundDecodedSize demonstrates the exact gap this
// package exists for: a small, entirely honest Content-Length describes the
// compressed body, not the decoded one, so checking it up front tells you
// nothing about how large the response will be once decoded.
func TestContentLengthDoesNotBoundDecodedSize(t *testing.T) {
	decoded := repeatToSize("compression bombs are small on the wire. ", 5<<20) // 5 MiB decoded
	compressed := gzipBytes(t, decoded, gzip.BestCompression)
	srv := gzipServer(t, compressed, "gzip")
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.ContentLength > 0 && resp.ContentLength >= int64(len(decoded)) {
		t.Fatalf("test is not exercising the gap: Content-Length %d already describes the decoded size", resp.ContentLength)
	}
	if len(compressed) >= len(decoded)/10 {
		t.Fatalf("test payload is not compressible enough to demonstrate the gap: %d wire bytes for %d decoded",
			len(compressed), len(decoded))
	}
	// Even a client that inspected Content-Length before reading (a small,
	// honest number) would have learned nothing about the 5 MiB the body
	// actually decodes to.
	t.Logf("Content-Length reported %d bytes on the wire; decoding it actually yields %d bytes (%.0fx)",
		len(compressed), len(decoded), float64(len(decoded))/float64(len(compressed)))
}

func TestGet_MaxBytesTrips(t *testing.T) {
	decoded := repeatToSize("x", 2<<20) // 2 MiB, incompressible-ish choice of level below avoids a huge ratio dominating
	compressed := gzipBytes(t, decoded, gzip.BestSpeed)
	srv := gzipServer(t, compressed, "gzip")
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{
		MaxBytes: 1 << 20, // smaller than the 2 MiB decoded body
		MaxRatio: math.Inf(1),
	})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	_, err = io.Copy(io.Discard, resp.Body)
	var tooLarge *cappedhttp.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Copy err = %v, want *ResponseTooLargeError", err)
	}
	if tooLarge.Wire {
		t.Fatalf("tripped limit reported as wire, want decoded")
	}
	if tooLarge.Limit != 1<<20 {
		t.Fatalf("Limit = %d, want %d", tooLarge.Limit, int64(1<<20))
	}
}

func TestGet_MaxCompressedBytesTrips(t *testing.T) {
	// Plain (identity) body: wire bytes and decoded bytes are the same
	// stream, so a small MaxCompressedBytes with a large MaxBytes isolates
	// the wire check.
	body := bytes.Repeat([]byte("a"), 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{
		MaxBytes:           1 << 30,
		MaxCompressedBytes: 100,
		MaxRatio:           math.Inf(1),
	})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	_, err = io.Copy(io.Discard, resp.Body)
	var tooLarge *cappedhttp.ResponseTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("Copy err = %v, want *ResponseTooLargeError", err)
	}
	if !tooLarge.Wire {
		t.Fatalf("tripped limit reported as decoded, want wire")
	}
	if tooLarge.Limit != 100 {
		t.Fatalf("Limit = %d, want 100", tooLarge.Limit)
	}
}

func TestGet_CompressionBombTrips(t *testing.T) {
	decoded := repeatToSize("compression bombs are small on the wire. ", 20<<20) // 20 MiB, extremely repetitive
	compressed := gzipBytes(t, decoded, gzip.BestCompression)
	srv := gzipServer(t, compressed, "gzip")
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{
		MaxBytes:        50 << 20, // large enough that MaxBytes would not be what trips
		RatioGraceBytes: 64 << 10,
		MaxRatio:        10,
	})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	n, err := io.Copy(io.Discard, resp.Body)
	var bomb *cappedhttp.CompressionBombError
	if !errors.As(err, &bomb) {
		t.Fatalf("Copy err = %v, want *CompressionBombError", err)
	}
	if bomb.Ratio <= 10 {
		t.Fatalf("Ratio = %.1f, want > 10", bomb.Ratio)
	}
	if n >= int64(len(decoded)) {
		t.Fatalf("read %d bytes, want an abort well before the full %d-byte decoded body", n, len(decoded))
	}
	t.Logf("aborted after %d decoded bytes at ratio %.1fx (limit %.1fx)", n, bomb.Ratio, bomb.Limit)
}

// TestRatioGraceFloor_SmallCompressiblePayloadIsAllowed shows the flip side
// of TestGet_CompressionBombTrips: a small, ordinary payload that happens to
// compress extremely well must not be flagged, because it never accumulates
// enough decoded bytes to clear RatioGraceBytes.
func TestRatioGraceFloor_SmallCompressiblePayloadIsAllowed(t *testing.T) {
	decoded := repeatToSize("ordinary, very repetitive JSON-ish payload. ", 400<<10) // 400 KiB
	compressed := gzipBytes(t, decoded, gzip.BestCompression)
	if ratio := float64(len(decoded)) / float64(len(compressed)); ratio < 100 {
		t.Fatalf("test payload only compressed %.1fx, want > 100x to exercise the grace floor meaningfully", ratio)
	}
	srv := gzipServer(t, compressed, "gzip")
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{
		MaxBytes: 10 << 20,
		// RatioGraceBytes left at its 1 MiB default, comfortably above
		// this response's 400 KiB decoded size.
		MaxRatio: 100,
	})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("small, ordinary compressible payload was refused: %v", err)
	}
	if !bytes.Equal(got, decoded) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(got), len(decoded))
	}
}

func TestGet_UnsupportedEncoding(t *testing.T) {
	srv := gzipServer(t, []byte("whatever bytes, never read"), "br")
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{})
	resp, err := c.Get(context.Background(), srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("Get succeeded, want UnsupportedEncodingError")
	}
	var unsupported *cappedhttp.UnsupportedEncodingError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *UnsupportedEncodingError", err)
	}
	if unsupported.Encoding != "br" {
		t.Fatalf("Encoding = %q, want %q", unsupported.Encoding, "br")
	}
	if resp != nil {
		t.Fatalf("resp = %v, want nil: an undecodable encoding must never be handed back as if it were the body", resp)
	}
}

func TestGet_Timeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-block // hang until the test unblocks this, well past the timeout
	}))
	// srv.Close waits for outstanding handlers to return, so block must be
	// closed (LIFO) before srv.Close runs, or the two would deadlock.
	defer srv.Close()
	defer close(block)

	c := cappedhttp.New(cappedhttp.Options{Timeout: 50 * time.Millisecond})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		var timeout *cappedhttp.RequestTimeoutError
		if !errors.As(err, &timeout) {
			t.Fatalf("Get err = %v, want *RequestTimeoutError", err)
		}
		return
	}
	defer resp.Body.Close()

	_, err = io.Copy(io.Discard, resp.Body)
	var timeout *cappedhttp.RequestTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("Copy err = %v, want *RequestTimeoutError", err)
	}
}

func TestGet_TooManyRedirects(t *testing.T) {
	var hops atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{MaxRedirects: 2})
	resp, err := c.Get(context.Background(), srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("Get succeeded, want *TooManyRedirectsError")
	}
	var tooMany *cappedhttp.TooManyRedirectsError
	if !errors.As(err, &tooMany) {
		t.Fatalf("err = %v, want *TooManyRedirectsError", err)
	}
	if tooMany.MaxRedirects != 2 {
		t.Fatalf("MaxRedirects = %d, want 2", tooMany.MaxRedirects)
	}
}

func TestGet_ContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-block
	}))
	// See TestGet_Timeout: close(block) must run before srv.Close (which
	// waits for outstanding handlers), so it is deferred second — defers
	// run LIFO, so it fires first.
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	c := cappedhttp.New(cappedhttp.Options{})
	resp, err := c.Get(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	cancel()
	_, err = io.Copy(io.Discard, resp.Body)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Copy err = %v, want context.Canceled in its chain", err)
	}
}

// TestBaseTransportAutoDecompression_IsDetected verifies the deliberate
// design choice in transport.go: if a caller supplies a custom Base
// RoundTripper that performs its own transparent decompression (because it
// wasn't told not to, and cappedhttp was told not to set Accept-Encoding
// itself), cappedhttp refuses to silently meter the already-decoded stream
// as though it still had wire-byte visibility.
func TestBaseTransportAutoDecompression_IsDetected(t *testing.T) {
	decoded := []byte("hello, decoded")
	compressed := gzipBytes(t, decoded, gzip.BestCompression)
	srv := gzipServer(t, compressed, "gzip")
	defer srv.Close()

	c := cappedhttp.New(cappedhttp.Options{
		DisableAcceptEncoding: true,              // cappedhttp won't set its own Accept-Encoding...
		Base:                  &http.Transport{}, // ...and this base auto-negotiates and auto-decodes on its own.
	})
	resp, err := c.Get(context.Background(), srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("Get succeeded despite the base transport auto-decompressing; wire metering would be silently wrong")
	}
	if !strings.Contains(err.Error(), "auto-decompressed") {
		t.Fatalf("err = %v, want a message about the base transport auto-decompressing", err)
	}
}

func TestGzipBombDecompressionInStdlibHasNoLimit(t *testing.T) {
	// This is not a cappedhttp test; it is the verification the README's
	// design rationale rests on. It documents, executably, that a stock
	// http.Client with default settings decodes an arbitrarily large gzip
	// body with no limit of its own, confirming the gap read out of
	// src/net/http/transport.go's gzipReader.
	decoded := repeatToSize("stdlib will decode all of this, no matter how large. ", 8<<20) // 8 MiB
	compressed := gzipBytes(t, decoded, gzip.BestCompression)
	srv := gzipServer(t, compressed, "gzip")
	defer srv.Close()

	resp, err := http.Get(srv.URL) // no Accept-Encoding set: stdlib negotiates gzip and decodes it for us
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != len(decoded) {
		t.Fatalf("stock client read %d bytes, want the full %d-byte decoded body with no limit applied", len(got), len(decoded))
	}
}
