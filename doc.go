// Package cappedhttp provides an HTTP client with hard limits on how much a
// server can make you decompress, buffer, and wait.
//
// Go's net/http Transport auto-negotiates gzip and transparently decompresses
// the response body for you when Transport.DisableCompression is false (the
// default) and the request sets no Accept-Encoding of its own. That
// decompression happens with no byte counter and no size limit: a response
// whose Content-Length is small on the wire can expand, unbounded, into
// however much memory reading the body takes. http.MaxBytesReader guards
// incoming request bodies on the server side; there is no client-side
// equivalent in the standard library.
//
// cappedhttp owns the decoding step itself so it can meter wire bytes and
// decoded bytes separately, as the response body streams, and abort the
// instant any of three limits trips: an absolute cap on decoded bytes, a cap
// on wire bytes, and a live decoded-to-wire compression ratio. See the
// package README for the full design rationale.
package cappedhttp
