package console

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Large transcript pages (hundreds of KB of JSON, mostly repeated tool output)
// dominated session-open time on EasyTier/mobile links. net/http does not
// compress on its own, so every event page went over the wire uncompressed.
var gzipPool = sync.Pool{New: func() any {
	return gzip.NewWriter(io.Discard)
}}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	compress bool
	decided  bool
}

func (w *gzipResponseWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	ct := w.Header().Get("Content-Type")
	w.compress = compressibleType(ct)
	if w.compress {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
	}
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	// 204/304 have no body; an empty gzip member would be wrong there.
	if code == http.StatusNoContent || code == http.StatusNotModified {
		w.compress = false
		w.decided = true
	} else {
		w.decide()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	w.decide()
	if !w.compress {
		return w.ResponseWriter.Write(b)
	}
	return w.gz.Write(b)
}

func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gzipResponseWriter) Flush() {
	if w.compress && w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func compressibleType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case ct == "text/event-stream":
		return false // streaming: compression would buffer events
	case strings.HasPrefix(ct, "text/"):
		return true
	case ct == "application/json",
		ct == "application/javascript",
		ct == "application/x-javascript",
		ct == "application/manifest+json",
		ct == "application/xml",
		ct == "image/svg+xml":
		return true
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		fields := strings.Split(part, ";")
		enc := strings.TrimSpace(fields[0])
		if !strings.EqualFold(enc, "gzip") {
			continue
		}
		// "gzip;q=0" is an explicit refusal; sending a gzip body anyway gives
		// the client something it declared it cannot decode.
		for _, f := range fields[1:] {
			f = strings.TrimSpace(f)
			if !strings.HasPrefix(f, "q=") {
				continue
			}
			if q, err := strconv.ParseFloat(strings.TrimPrefix(f, "q="), 64); err == nil && q <= 0 {
				return false
			}
		}
		return true
	}
	return false
}

// gzipMiddleware compresses textual responses. SSE is excluded: it must be
// flushed event by event, and per-event compression would add latency for no
// meaningful size win.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Advertise the negotiation even when this client did not ask for gzip:
		// a shared cache must not reuse an identity body for a gzip client.
		w.Header().Add("Vary", "Accept-Encoding")
		// File downloads keep their length (progress, resuming) and are
		// mostly already compressed.
		if r.Method == http.MethodHead || !acceptsGzip(r) ||
			r.Header.Get("Range") != "" || strings.HasSuffix(r.URL.Path, "/stream") ||
			strings.HasSuffix(r.URL.Path, "/files/download") {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(w)
		grw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() {
			// Close even with no body: the header already says gzip, so the
			// response must still be a valid (empty) gzip stream.
			if grw.compress {
				_ = gz.Close()
			}
			gz.Reset(io.Discard)
			gzipPool.Put(gz)
		}()
		next.ServeHTTP(grw, r)
	})
}
