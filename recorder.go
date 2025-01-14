package httputil

import (
	"bufio"
	"net"
	"net/http"
)

// HttpStatusRecorder implements the default http.ResponseWriter interface, used to record the returned HTTP status code.
type HttpStatusRecorder struct {
	http.ResponseWriter
	Status       int
	BytesWritten int
}

// NewHttpStatusRecorder creates a HttpStatusRecorder (no http.Flusher, Hijacker, or Pusher interfaces).
func NewHttpStatusRecorder(writer http.ResponseWriter) *HttpStatusRecorder {
	return &HttpStatusRecorder{ResponseWriter: writer, Status: http.StatusOK}
}

// WriteHeader overrides the default WriteHeader behavior to also write access logs.
func (rec *HttpStatusRecorder) WriteHeader(code int) {
	rec.Status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *HttpStatusRecorder) Write(p []byte) (n int, err error) {
	n, err = rec.ResponseWriter.Write(p)
	rec.BytesWritten += n
	return
}

func (rec *HttpStatusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := rec.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil && rec.Status == 0 {
		// The status will be StatusSwitchingProtocols if there was no error and
		// WriteHeader has not been called yet
		rec.Status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}
