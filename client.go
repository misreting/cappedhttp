package cappedhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is an HTTP client that enforces Options on every request: an
// absolute cap on decoded response bytes, a cap on wire bytes, a live
// compression-ratio check, an overall time budget covering redirects and
// body transfer, and a redirect count limit. Its zero value is not usable;
// construct one with New.
type Client struct {
	opts       resolved
	httpClient *http.Client
}

// New returns a Client configured with o. See Options for what each field
// controls and its default when left at zero.
func New(o Options) *Client {
	r := resolve(o)
	transport := NewTransport(o)

	hc := &http.Client{
		Transport: transport,
	}
	if r.maxRedirects >= 0 {
		max := r.maxRedirects
		hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > max {
				return &TooManyRedirectsError{MaxRedirects: max}
			}
			return nil
		}
	}

	return &Client{opts: r, httpClient: hc}
}

// Get issues a GET request for url, bounded by ctx and the Client's
// Options.Timeout, and returns the response with its body wrapped so that
// reading it enforces every configured limit.
func (c *Client) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("cappedhttp: %w", err)
	}
	return c.Do(req)
}

// Do sends req (whose context bounds cancellation, as with http.Client.Do)
// and returns the response with its body wrapped so that reading it
// enforces every configured limit. If Options.Timeout is positive, Do
// derives a deadline from it — covering connection, redirects and the full
// body transfer together — independent of any deadline already on
// req.Context(); whichever fires first wins. The derived context is kept
// alive until the returned response's Body is closed, and any timeout it
// causes surfaces as a *RequestTimeoutError from Body.Read (or from Do
// itself, if the timeout expires before headers arrive).
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	var cancel context.CancelFunc = func() {}
	if c.opts.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.opts.timeout)
		req = req.Clone(ctx)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		cancel()
		if te := asTimeout(err, c.opts.timeout); te != nil {
			return nil, te
		}
		return nil, unwrapOurs(err)
	}

	resp.Body = &timeoutBody{
		inner:   resp.Body,
		cancel:  cancel,
		timeout: c.opts.timeout,
	}
	return resp, nil
}

// asTimeout reports whether err (as returned by http.Client.Do, typically
// wrapped in a *url.Error) was ultimately caused by the deadline Do derived
// from a positive timeout, returning the *RequestTimeoutError to use if so.
func asTimeout(err error, timeout time.Duration) *RequestTimeoutError {
	if timeout <= 0 {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &RequestTimeoutError{Timeout: timeout}
	}
	return nil
}

// unwrapOurs strips the *url.Error wrapping http.Client.Do adds around any
// error a RoundTripper or CheckRedirect returns, so that a limit tripped
// during the header phase (an unsupported Content-Encoding, say, or a
// redirect count exceeded) comes back as the bare, distinctly-typed error
// rather than wrapped in "Get \"...\": ...". errors.As would still find it
// either way, since *url.Error implements Unwrap; this just gives a cleaner
// value and message for the common case of a caller comparing types
// directly.
func unwrapOurs(err error) error {
	var tooMany *TooManyRedirectsError
	if errors.As(err, &tooMany) {
		return tooMany
	}
	var unsupported *UnsupportedEncodingError
	if errors.As(err, &unsupported) {
		return unsupported
	}
	var tooLarge *ResponseTooLargeError
	if errors.As(err, &tooLarge) {
		return tooLarge
	}
	var bomb *CompressionBombError
	if errors.As(err, &bomb) {
		return bomb
	}
	return err
}

// timeoutBody wraps a response body to translate a context deadline into
// RequestTimeoutError and to release the derived context's resources once
// the body is closed (or once it errors out, since nothing more will be
// read from it after that).
type timeoutBody struct {
	inner   io.ReadCloser
	cancel  context.CancelFunc
	timeout time.Duration
	closed  bool
}

func (b *timeoutBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if err != nil && b.timeout > 0 && errors.Is(err, context.DeadlineExceeded) {
		err = &RequestTimeoutError{Timeout: b.timeout}
	}
	return n, err
}

func (b *timeoutBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	err := b.inner.Close()
	b.cancel()
	return err
}
