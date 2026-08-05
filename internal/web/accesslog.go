package web

import (
	"log"
	"net/http"
	"time"
)

// accessRecorder captures the response status and body size so the gateway can
// log per-request outcomes (the service previously produced almost no request
// logs, making every failure invisible to operations).
type accessRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (a *accessRecorder) WriteHeader(code int) {
	if a.status == 0 {
		a.status = code
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *accessRecorder) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	n, err := a.ResponseWriter.Write(b)
	a.bytes += n
	return n, err
}

// Flush keeps http.Flusher working for SSE endpoints.
func (a *accessRecorder) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &accessRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		dur := time.Since(start).Milliseconds()
		level := "info"
		if rec.status >= 400 {
			level = "error"
		}
		log.Printf("[access] %s method=%s path=%s status=%d dur=%dms bytes=%d ip=%s", level, r.Method, r.URL.Path, rec.status, dur, rec.bytes, clientIP(r))
	})
}
