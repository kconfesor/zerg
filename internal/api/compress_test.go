package api

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What is compressed, and what must not be.
//
// The cockpit is a megabyte of JavaScript read over a tailnet from a phone and
// nothing compressed it: Lighthouse measured 6.9s to first paint and named
// 788 KiB of savings from this alone. The care is in the exceptions: an event
// stream buffered into a compressor stops arriving, which looks exactly like a
// daemon that has died.
func TestWhatIsCompressedAndWhatIsLeftAlone(t *testing.T) {
	big := strings.Repeat("const x = 1;\n", 2000)

	cases := []struct {
		name        string
		contentType string
		body        string
		accept      string
		wantGzip    bool
	}{
		{
			name: "javascript for a client that asked", contentType: "application/javascript",
			body: big, accept: "gzip, deflate, br", wantGzip: true,
		},
		{
			name: "json", contentType: "application/json",
			body:   `{"files":[` + strings.Repeat(`{"path":"a.rs"},`, 200) + `null]}`,
			accept: "gzip", wantGzip: true,
		},
		{
			name: "css", contentType: "text/css", body: big, accept: "gzip", wantGzip: true,
		},
		{
			// The client cannot read it, so it is not sent.
			name: "a client that did not ask", contentType: "application/javascript",
			body: big, accept: "", wantGzip: false,
		},
		{
			// Already compressed: spending CPU to add bytes.
			name: "a png", contentType: "image/png", body: big, accept: "gzip", wantGzip: false,
		},
		{
			// The framing costs more than the saving.
			name: "a short answer", contentType: "application/json",
			body: `{"status":"asked"}`, accept: "gzip", wantGzip: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := compressed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Length", itoa(len(tc.body)))
				_, _ = io.WriteString(w, tc.body)
			}))

			req := httptest.NewRequest("GET", "/asset", nil)
			if tc.accept != "" {
				req.Header.Set("Accept-Encoding", tc.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get("Content-Encoding") == "gzip"
			if got != tc.wantGzip {
				t.Fatalf("Content-Encoding = %q, want gzip: %v",
					rec.Header().Get("Content-Encoding"), tc.wantGzip)
			}

			// Whatever was decided, the client has to end up with the bytes
			// that were written.
			body := rec.Body.Bytes()
			if got {
				zr, err := gzip.NewReader(rec.Body)
				if err != nil {
					t.Fatalf("the body is not gzip: %v", err)
				}
				body, err = io.ReadAll(zr)
				if err != nil {
					t.Fatalf("reading the compressed body: %v", err)
				}
				// A stale Content-Length here truncates the response.
				if rec.Header().Get("Content-Length") != "" {
					t.Error("Content-Length survived compression; it describes the wrong body")
				}
				if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
					t.Error("no Vary: a cache would serve this to a client that cannot read it")
				}
			}
			if string(body) != tc.body {
				t.Errorf("the body came back %d bytes, want %d", len(body), len(tc.body))
			}
		})
	}
}

// An event stream is a stream. Compressed, the board stops updating until
// enough bytes accumulate to flush, which reads as a dead daemon.
func TestAnEventStreamIsNotCompressedAndStillFlushes(t *testing.T) {
	flushed := make(chan struct{}, 4)
	h := compressed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 3; i++ {
			_, _ = io.WriteString(w, "data: "+strings.Repeat("x", 400)+"\n\n")
			f, ok := w.(http.Flusher)
			if !ok {
				t.Error("the wrapper hid http.Flusher, so nothing can stream through it")
				return
			}
			f.Flush()
			flushed <- struct{}{}
		}
	}))

	req := httptest.NewRequest("GET", "/api/stream", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("the event stream was encoded %q", enc)
	}
	if len(flushed) != 3 {
		t.Errorf("%d flushes reached the writer, want 3", len(flushed))
	}
	if !strings.HasPrefix(rec.Body.String(), "data: ") {
		t.Error("the stream did not arrive as text")
	}
}

// A websocket upgrade has to reach the handler intact.
//
// Wrapped, the library asks the writer for http.Hijacker, does not find it,
// and answers 501: the live stream, and with it the board's updates, gone.
// Four stream tests caught this the first time.
func TestAnUpgradeIsNotWrapped(t *testing.T) {
	var hijackable bool
	h := compressed(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hijackable = w.(http.Hijacker)
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))

	req := httptest.NewRequest("GET", "/api/stream", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	// httptest's recorder is not a Hijacker, so this asks the question the
	// wrapper decides: does the handler see the writer it was given?
	h.ServeHTTP(&hijackRecorder{ResponseRecorder: httptest.NewRecorder()}, req)

	if !hijackable {
		t.Error("the handler could not hijack the connection; a websocket answers 501 here")
	}
}

// hijackRecorder is a recorder that can be hijacked, like a real connection.
type hijackRecorder struct {
	*httptest.ResponseRecorder
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
