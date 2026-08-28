package api

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Compressing what goes over the wire.
//
// Nothing here was compressed, and the cockpit is a megabyte of JavaScript and
// ninety kilobytes of CSS served from the daemon: measured with Lighthouse
// under its mobile throttling, first contentful paint was 6.9s and the report
// named 788 KiB of savings from this alone. The daemon is read over a tailnet
// from a phone, which is exactly the connection that throttling models.
//
// gzip rather than brotli, which would be perhaps 15% smaller and is not in the
// standard library. One dependency for one increment on a payload that already
// drops by two thirds is not a trade this project makes.
//
// A streaming wrapper rather than files precompressed at build time: the assets
// are immutable and cached for a year by the browser, so a client pays this
// once, and a build step that produces a second copy of every asset is a thing
// that can rot out of sync with the first.

// gzipTypes are the content types worth compressing.
//
// Compressing an already-compressed byte stream costs CPU to add bytes, so
// images, fonts and archives are left alone. SVG is text and compresses like it.
var gzipTypes = []string{
	"text/",
	"application/json",
	"application/javascript",
	"application/manifest+json",
	"image/svg+xml",
}

// gzipMin is the size below which compressing is not worth the framing.
//
// A gzip stream carries about 20 bytes of header and trailer, and the daemon's
// small JSON answers ("status": "asked") are shorter than that. Only responses
// that declare their length can be measured before the fact; a streamed one is
// compressed on the assumption that anything worth streaming is worth
// compressing.
const gzipMin = 512

var gzipPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// compressed wraps a handler so responses are gzipped for clients that ask.
func compressed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A websocket upgrade is not a response to compress: it is a
		// handshake, after which the connection stops being HTTP. Wrapped, the
		// library asks for http.Hijacker, does not find it through this type,
		// and answers 501 -- which is the event stream, and therefore the whole
		// live board, gone. Hijack is passed through below as well, but the
		// upgrade is better not wrapped at all.
		if r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		// Said whether or not this response ends up compressed: the answer
		// varies by request header, and a cache that does not know that will
		// hand a gzipped body to a client that cannot read it.
		w.Header().Add("Vary", "Accept-Encoding")

		cw := &gzipWriter{ResponseWriter: w}
		defer cw.close()
		next.ServeHTTP(cw, r)
	})
}

// gzipWriter decides at the moment the header is written, which is the first
// point at which the content type is known.
type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (w *gzipWriter) WriteHeader(status int) {
	w.decide(status)
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipWriter) decide(status int) {
	if w.decided {
		return
	}
	w.decided = true

	h := w.Header()
	// Server-sent events are a stream a person is waiting on: buffered into a
	// compressor, the board would stop updating until enough bytes had
	// accumulated to flush, which is indistinguishable from a daemon that has
	// died. Flushes do reach through this wrapper, but the browser also has to
	// see each event as it happens and the saving on a few hundred bytes an
	// hour is nothing.
	ct := h.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return
	}
	// Already encoded by whoever wrote it, or a status with no body.
	if h.Get("Content-Encoding") != "" || status == http.StatusNoContent || status == http.StatusNotModified {
		return
	}
	if !compressible(ct) {
		return
	}
	if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil && n < gzipMin {
		return
	}

	// The length of the uncompressed body is not the length of what is sent,
	// and a wrong one truncates the response.
	h.Del("Content-Length")
	h.Set("Content-Encoding", "gzip")

	w.gz = gzipPool.Get().(*gzip.Writer)
	w.gz.Reset(w.ResponseWriter)
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	if !w.decided {
		// Written without an explicit WriteHeader, which means net/http is
		// about to sniff the type and send a 200. Sniff it here first, or the
		// decision would be taken with no content type at all.
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.decide(http.StatusOK)
	}
	if w.gz != nil {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush reaches the client rather than stopping in the compressor. Streaming
// endpoints depend on it, and a Flush that only flushed gzip's buffer into a
// buffered ResponseWriter would still leave the reader waiting.
func (w *gzipWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands the raw connection over, for anything that stops speaking HTTP
// after the handshake. Without it a wrapped websocket endpoint answers 501.
func (w *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	// Nothing compressed can survive the connection being taken over, and a
	// gzip trailer written after it would corrupt the first frames.
	w.decided = true
	w.gz = nil
	return h.Hijack()
}

func (w *gzipWriter) close() {
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	gzipPool.Put(w.gz)
	w.gz = nil
}

func compressible(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	for _, p := range gzipTypes {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}
