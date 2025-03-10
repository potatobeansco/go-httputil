package httputil

import (
	"context"
	"github.com/felixge/httpsnoop"
	"github.com/potatobeansco/go-logutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"
)

// LogFormatterParams is the structure any formatter will be handed when time to log comes.
// Taken and modified from Gorilla mux.
type LogFormatterParams struct {
	Request    *http.Request
	URL        url.URL
	TimeStamp  time.Time
	StatusCode int
	Size       int
}

// LogFormatter gives the signature of the formatter function passed to CustomLoggingHandler.
type LogFormatter func(ctx context.Context, logger logutil.ContextLogger, params LogFormatterParams)

// loggingHandler is the http.Handler implementation for LoggingHandlerTo and its
// friends
type loggingHandler struct {
	writer    logutil.ContextLogger
	handler   http.Handler
	formatter LogFormatter
}

func (h loggingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	t := time.Now()
	logger, w := makeLogger(w)
	u := *req.URL

	h.handler.ServeHTTP(w, req)
	if req.MultipartForm != nil {
		err := req.MultipartForm.RemoveAll()
		if err != nil {
			return
		}
	}

	params := LogFormatterParams{
		Request:    req,
		URL:        u,
		TimeStamp:  t,
		StatusCode: logger.Status,
		Size:       logger.BytesWritten,
	}

	h.formatter(req.Context(), h.writer, params)
}

func makeLogger(w http.ResponseWriter) (*HttpStatusRecorder, http.ResponseWriter) {
	logger := NewHttpStatusRecorder(w)
	return logger, httpsnoop.Wrap(w, httpsnoop.Hooks{
		Write: func(httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return logger.Write
		},
		WriteHeader: func(httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return logger.WriteHeader
		},
	})
}

const lowerhex = "0123456789abcdef"

func appendQuoted(buf []byte, s string) []byte {
	var runeTmp [utf8.UTFMax]byte
	for width := 0; len(s) > 0; s = s[width:] { //nolint: wastedassign //TODO: why width starts from 0and reassigned as 1
		r := rune(s[0])
		width = 1
		if r >= utf8.RuneSelf {
			r, width = utf8.DecodeRuneInString(s)
		}
		if width == 1 && r == utf8.RuneError {
			buf = append(buf, `\x`...)
			buf = append(buf, lowerhex[s[0]>>4])
			buf = append(buf, lowerhex[s[0]&0xF])
			continue
		}
		if r == rune('"') || r == '\\' { // always backslashed
			buf = append(buf, '\\')
			buf = append(buf, byte(r))
			continue
		}
		if strconv.IsPrint(r) {
			n := utf8.EncodeRune(runeTmp[:], r)
			buf = append(buf, runeTmp[:n]...)
			continue
		}
		switch r {
		case '\a':
			buf = append(buf, `\a`...)
		case '\b':
			buf = append(buf, `\b`...)
		case '\f':
			buf = append(buf, `\f`...)
		case '\n':
			buf = append(buf, `\n`...)
		case '\r':
			buf = append(buf, `\r`...)
		case '\t':
			buf = append(buf, `\t`...)
		case '\v':
			buf = append(buf, `\v`...)
		default:
			switch {
			case r < ' ':
				buf = append(buf, `\x`...)
				buf = append(buf, lowerhex[s[0]>>4])
				buf = append(buf, lowerhex[s[0]&0xF])
			case r > utf8.MaxRune:
				r = 0xFFFD
				fallthrough
			case r < 0x10000:
				buf = append(buf, `\u`...)
				for s := 12; s >= 0; s -= 4 {
					buf = append(buf, lowerhex[r>>uint(s)&0xF])
				}
			default:
				buf = append(buf, `\U`...)
				for s := 28; s >= 0; s -= 4 {
					buf = append(buf, lowerhex[r>>uint(s)&0xF])
				}
			}
		}
	}
	return buf
}

// buildCommonLogLine builds a log entry for req in Apache Common Log Format.
// ts is the timestamp with which the entry should be logged.
// status and size are used to provide the response HTTP status and size.
func buildCommonLogLine(req *http.Request, url url.URL, ts time.Time, status int, size int) []byte {
	username := "-"
	if url.User != nil {
		if name := url.User.Username(); name != "" {
			username = name
		}
	}

	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}

	uri := req.RequestURI

	// Requests using the CONNECT method over HTTP/2.0 must use
	// the authority field (aka r.Host) to identify the target.
	// Refer: https://httpwg.github.io/specs/rfc7540.html#CONNECT
	if req.ProtoMajor == 2 && req.Method == "CONNECT" {
		uri = req.Host
	}
	if uri == "" {
		uri = url.RequestURI()
	}

	buf := make([]byte, 0, 3*(len(host)+len(username)+len(req.Method)+len(uri)+len(req.Proto)+50)/2)
	buf = append(buf, host...)
	buf = append(buf, " - "...)
	buf = append(buf, username...)
	buf = append(buf, " ["...)
	buf = append(buf, ts.Format("02/Jan/2006:15:04:05 -0700")...)
	buf = append(buf, `] "`...)
	buf = append(buf, req.Method...)
	buf = append(buf, " "...)
	buf = appendQuoted(buf, uri)
	buf = append(buf, " "...)
	buf = append(buf, req.Proto...)
	buf = append(buf, `" `...)
	buf = append(buf, strconv.Itoa(status)...)
	buf = append(buf, " "...)
	buf = append(buf, strconv.Itoa(size)...)
	return buf
}

// writeLog writes a log entry for req to w in Apache Common Log Format.
// ts is the timestamp with which the entry should be logged.
// status and size are used to provide the response HTTP status and size.
func writeLog(ctx context.Context, writer logutil.ContextLogger, params LogFormatterParams) {
	buf := buildCommonLogLine(params.Request, params.URL, params.TimeStamp, params.StatusCode, params.Size)
	writer.AccessCtx(ctx, string(buf))
}

// writeCombinedLog writes a log entry for req to w in Apache Combined Log Format.
// ts is the timestamp with which the entry should be logged.
// status and size are used to provide the response HTTP status and size.
func writeCombinedLog(ctx context.Context, writer logutil.ContextLogger, params LogFormatterParams) {
	buf := buildCommonLogLine(params.Request, params.URL, params.TimeStamp, params.StatusCode, params.Size)
	buf = append(buf, ` "`...)
	buf = appendQuoted(buf, params.Request.Referer())
	buf = append(buf, `" "`...)
	buf = appendQuoted(buf, params.Request.UserAgent())
	buf = append(buf, '"')
	writer.AccessCtx(ctx, string(buf))
}

// CombinedLoggingHandler return a http.Handler that wraps h and logs requests to out in
// Apache Combined Log Format.
//
// See http://httpd.apache.org/docs/2.2/logs.html#combined for a description of this format.
//
// LoggingHandler always sets the ident field of the log to -.
func CombinedLoggingHandler(logger logutil.ContextLogger, h http.Handler) http.Handler {
	return loggingHandler{logger, h, writeCombinedLog}
}

// LoggingHandler return a http.Handler that wraps h and logs requests to out in
// Apache Common Log Format (CLF).
//
// See http://httpd.apache.org/docs/2.2/logs.html#common for a description of this format.
//
// LoggingHandler always sets the ident field of the log to -
//
// Example:
//
//	r := mux.NewRouter()
//	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
//		w.Write([]byte("This is a catch-all route"))
//	})
//	loggedRouter := handlers.LoggingHandler(os.Stdout, r)
//	http.ListenAndServe(":1123", loggedRouter)
func LoggingHandler(logger logutil.ContextLogger, h http.Handler) http.Handler {
	return loggingHandler{logger, h, writeLog}
}

// CustomLoggingHandler provides a way to supply a custom log formatter
// while taking advantage of the mechanisms in this package.
func CustomLoggingHandler(logger logutil.ContextLogger, h http.Handler, f LogFormatter) http.Handler {
	return loggingHandler{logger, h, f}
}
