# cappedhttp

An `http.Client` with hard limits on how much a server can make you
decompress, buffer, and wait.

```
one response: 595 KB on the wire, 200 MB decoded (344x)

http.Client      67ms   rss  +520 MB   read 209715200 bytes (209715200 bytes)
cappedhttp        1ms   rss    +4 MB   refused — cappedhttp: compression ratio
                                        270.9x exceeded limit 100.0x
                                        (3992 wire bytes decoded to 1081344)
```

That's `go run ./examples/bombdemo`, unedited, on Go 1.27.1 darwin/arm64
(`ru_maxrss` via `syscall.Getrusage`, so `rss` is peak resident memory for
that process). The response is 595 KB, its `Content-Length` is honest, and
reading it the normal way — `io.ReadAll(resp.Body)`, the thing you'd actually
write to do something with the body — costs 520 MB of resident memory in the
stock client. `cappedhttp` refuses it after about 1 MB decoded.

## The problem

Go's `net/http` decompresses `Content-Encoding: gzip` for you by default, and
nothing in the standard library lets you bound how large that decompression
is allowed to get. Confirmed by reading the Go 1.27.1 standard library
itself, in `src/net/http/transport.go`:

- When `Transport.DisableCompression` is `false` (the default) and a request
  sets no `Accept-Encoding` of its own, the transport adds
  `Accept-Encoding: gzip` itself (`transport.go:2999-3017`) and, if the
  response comes back gzipped, wraps `resp.Body` in a `gzipReader`
  (`transport.go:2587`).
- `gzipReader.Read` (`transport.go:3369-3376`) is a direct call to the
  underlying `gzip.Reader`'s `Read`. No byte counter, no size limit, nothing
  between your code and however much decompressed data the server wants to
  hand you.
- `http.MaxBytesReader` looks like it should help. It doesn't: it caps
  incoming *request* bodies on the server side. There is no client-side
  equivalent anywhere in `net/http`.

The two defences people reach for don't work either:

- **Checking `Content-Length` first.** It describes the *compressed* body.
  `TestContentLengthDoesNotBoundDecodedSize` in this repo demonstrates it
  directly: a 15 KB, entirely honest `Content-Length` decodes to 5 MiB.
  It's also attacker-controlled, and often absent.
- **Not asking for compression.** If you don't set `Accept-Encoding`
  yourself, the transport asks for gzip *on your behalf* and decodes the
  answer transparently — you never got a say. Setting it yourself (or
  disabling compression) is exactly the mechanism this package builds on;
  see below.

Because decoding happens inside `net/http`, before any `http.RoundTripper`
wrapper or response body wrapper you install gets to see the bytes, a
decompressed-size cap cannot be bolted on from outside the transport. This
package's `Transport` disables the standard library's auto-decompression and
runs the gzip decoder itself, which is the only place the check can live.

Anything that fetches URLs it doesn't control is exposed: webhook delivery,
link previews, scrapers, and agent/LLM tooling that retrieves arbitrary
pages.

## Install

```sh
go get github.com/agentscope-ai-java/cappedhttp
```

Go >= 1.27. No runtime dependencies — standard library only.

## Use

```go
c := cappedhttp.New(cappedhttp.Options{
    MaxBytes:           10 << 20, // decoded body cap
    MaxCompressedBytes: 2 << 20,  // wire cap
    MaxRatio:           100,      // decoded ÷ wire
    Timeout:            30 * time.Second,
})

resp, err := c.Get(ctx, url)
if err != nil {
    var bomb *cappedhttp.CompressionBombError
    if errors.As(err, &bomb) {
        log.Printf("%d wire bytes decoded to %d, refused", bomb.CompressedBytes, bomb.DecodedBytes)
    }
    return err
}
defer resp.Body.Close()

body, err := io.ReadAll(resp.Body) // limits are enforced while this streams
```

`Client.Do(req)` is also available for anything beyond a bare `GET` — it
takes an `*http.Request` the same way `http.Client.Do` does, with the
request's own `context.Context` bounding cancellation as usual.

Limits are enforced **while the body streams**: `Body.Read` returns the error
the instant a limit trips and closes the underlying connection. Nothing is
buffered past a cap; the worst-case overshoot is one internal read chunk
(a few tens of KB), not the rest of the response.

If you'd rather wire the metering into your own `http.Client` — a custom
transport chain, an existing `RoundTripper` stack — `cappedhttp.Transport`
is the piece that does the work and implements `http.RoundTripper` on its
own; `Client` is a thin `Get`/`Do` convenience layered on top of it for
overall timeout and redirect handling.

## Options

| Option | Default | What it bounds |
| --- | --- | --- |
| `MaxBytes` | 10 MiB | Body size after decompression. |
| `MaxCompressedBytes` | `MaxBytes` | Bytes accepted off the wire. |
| `MaxRatio` | 100 | Decoded ÷ wire size. `math.Inf(1)` disables it. |
| `RatioGraceBytes` | 1 MiB | Decoded bytes before the ratio check starts. |
| `Timeout` | 30s | Connect, redirects and body transfer, together. |
| `MaxRedirects` | 5 | Redirect hops. |
| `AcceptEncoding` | `gzip` | The header sent when the request has none. |
| `DisableAcceptEncoding` | `false` | Send no `Accept-Encoding` at all. |
| `Base` | an internal `*http.Transport` | Underlying `http.RoundTripper`. |

### Why the ratio has a grace floor

`TestGet_CompressionBombTrips` and `TestRatioGraceFloor_SmallCompressiblePayloadIsAllowed`
in this repo pin down both sides of this: a 20 MiB payload of repeated text
compresses ~1000x and is correctly refused after about 64 KiB decoded; a
400 KiB payload that happens to compress >100x is let through untouched,
because it never accumulates enough decoded bytes to clear the 1 MiB grace
floor. Small, repetitive payloads — a JSON array of near-identical records,
for instance — routinely compress that well and are completely ordinary.
Applying a ratio limit to them from the first byte produces nothing but
false positives; `RatioGraceBytes` holds the check back until enough has
decoded for the ratio to mean something. Below that floor, `MaxBytes` is the
only limit that applies.

### Errors

All are concrete, distinct types (not a shared base type — Go doesn't need
one for `errors.As` to work), each carrying the limit and the value observed
when it tripped:

- `*ResponseTooLargeError` — `Limit`, `Observed`, `Wire` (true when the wire
  cap tripped, false for the decoded cap)
- `*CompressionBombError` — `Ratio`, `Limit`, `CompressedBytes`,
  `DecodedBytes`
- `*RequestTimeoutError` — `Timeout` (also matches
  `errors.Is(err, context.DeadlineExceeded)`)
- `*TooManyRedirectsError` — `MaxRedirects`
- `*UnsupportedEncodingError` — `Encoding`, `Supported`

```go
var bomb *cappedhttp.CompressionBombError
if errors.As(err, &bomb) {
    log.Printf("%.0fx ratio (limit %.0fx): %d wire bytes -> %d decoded",
        bomb.Ratio, bomb.Limit, bomb.CompressedBytes, bomb.DecodedBytes)
}
```

### Content encodings

Only `gzip` (and `identity`, i.e. no encoding) — the two encodings the Go
standard library has decoders for without a third-party dependency. A
response whose `Content-Encoding` is anything else (`deflate`, `br`, `zstd`,
...) is refused outright with `*UnsupportedEncodingError` before any of its
body is read. It is never handed back to the caller still compressed as
though it were the decoded body — `TestGet_UnsupportedEncoding` asserts both
the error type and that `resp` comes back `nil`.

## Why `Transport` decodes gzip itself, instead of leaving it to `net/http`

This is the crux of the design, and it was a deliberate choice, not an
oversight of the existing behaviour.

Go's `Transport.DisableCompression` doc comment says outright that when it's
`false` (the default) and the request sets no `Accept-Encoding`, the
transport "transparently decode[s]" a gzipped response for you. That's
convenient, and it's also exactly the thing that makes a decompressed-size
cap impossible to add from outside: by the time your `RoundTripper` wrapper
or response body wrapper sees `resp.Body`, the bytes have already been
through `gzipReader.Read` with no counter attached (verified above, citing
`transport.go`). You cannot observe wire bytes and decoded bytes separately
if `net/http` has already collapsed them into one decoded stream before you
get a look.

So `cappedhttp.Transport`:

1. Builds its own base `*http.Transport` (when `Options.Base` is nil) with
   `DisableCompression: true`.
2. Sets `Accept-Encoding` itself on every outgoing request that doesn't
   already have one (this alone is also sufficient on its own — per the same
   doc comment, a request with an explicit `Accept-Encoding` is never
   auto-decoded even with `DisableCompression: false` — but setting both is
   what makes the guarantee independent of exactly which base transport is
   in play).
3. Receives the response body **still compressed**, and decodes it itself
   with `gzip.NewReader` wrapped around a byte-counting reader, so it always
   has both the wire count and the decoded count as the body streams.

If you supply your own `Options.Base` and it performs its own transparent
decompression anyway (compression left enabled, and `DisableAcceptEncoding:
true` so `cappedhttp` never sets its own header), `Transport` detects this
via `resp.Uncompressed` and refuses the response with an explicit error
rather than silently enforcing a weaker cap on an already-decoded stream —
see `TestBaseTransportAutoDecompression_IsDetected`.

## What it does not do

- **It is not SSRF protection.** It bounds the size and duration of a
  response, not where the request goes. Pair it with your own checks if the
  URL comes from a user.
- **Only `gzip` and `identity`.** No `deflate`, `br`, or `zstd` decoding —
  standard library only, and the standard library only ships a gzip decoder.
  See "Content encodings" above.
- **HTTP/1.1 and HTTP/2 both work** (whatever the base `*http.Transport`
  negotiates), but only through the client path — there's no server-side
  piece here.
- **No cookie jar, no automatic retry.** `Client` wraps a plain
  `*http.Client`; set `Base`/build your own `http.Client` around `Transport`
  if you need either.

## Develop

```sh
go build ./...
go vet ./...
go test -race ./...
gofmt -l .              # must print nothing

go run ./examples/bombdemo
```

## License

MIT
