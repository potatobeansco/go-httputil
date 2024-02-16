package httputil

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"compress/lzw"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var unixEpochTime = time.Unix(0, 0)

// isZeroTime reports whether t is obviously unspecified (either zero or Unix()=0).
func isZeroTime(t time.Time) bool {
	return t.IsZero() || t.Equal(unixEpochTime)
}

type CompressorFileServerConfig struct {
	// Enables the "br" compression.
	EnableBrotli bool
	// Enables the "gzip" compression.
	EnableGzip bool
	// Enables the "deflate" compression.
	EnableZlib bool
	// Enables the "compress" compression.
	EnableLzw bool
	// Brotli encoder, you can use the cbrotli library (this requires cgo).
	BrotliEncoder BrotliCompressorEncoder
	// GZIP quality level: -1 to 9.
	// -1 for default compression, 0 for no compression, 9 for maximum compression.
	GzipQuality int
	// ZLIB quality level: -1 to 9.
	// -1 for default compression, 0 for no compression, 9 for maximum compression.
	ZlibQuality int
	// ContentTypes that are supported to be compressed.
	// Empty will compress all.
	ContentTypes []string
	// The minimum length of the content that is going to be written for compression to be active.
	MinLength     int
	IndexFiles    []string
	ErrorHandlers map[int]func(writer http.ResponseWriter, err error)
}

type compressorFileServer struct {
	config         *CompressorFileServerConfig
	root           http.FileSystem
	defaultHandler http.Handler
	indexFiles     []string
	errorHandlers  map[int]func(writer http.ResponseWriter, err error)
}

// scanETag determines if a syntactically valid ETag is present at s. If so,
// the ETag and remaining text after consuming ETag is returned. Otherwise,
// it returns "", "".
func scanETag(s string) (etag string, remain string) {
	s = textproto.TrimString(s)
	start := 0
	if strings.HasPrefix(s, "W/") {
		start = 2
	}
	if len(s[start:]) < 2 || s[start] != '"' {
		return "", ""
	}
	// ETag is either W/"text" or "text".
	// See RFC 7232 2.3.
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		switch {
		// Character values allowed in ETags.
		case c == 0x21 || c >= 0x23 && c <= 0x7E || c >= 0x80:
		case c == '"':
			return s[:i+1], s[i+1:]
		default:
			return "", ""
		}
	}
	return "", ""
}

// etagStrongMatch reports whether a and b match using strong ETag comparison.
// Assumes a and b are valid ETags.
func etagStrongMatch(a, b string) bool {
	return a == b && a != "" && a[0] == '"'
}

// etagWeakMatch reports whether a and b match using weak ETag comparison.
// Assumes a and b are valid ETags.
func etagWeakMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// condResult is the result of an HTTP request precondition check.
// See https://tools.ietf.org/html/rfc7232 section 3.
type condResult int

const (
	condNone condResult = iota
	condTrue
	condFalse
)

func (c *compressorFileServer) checkIfMatch(w http.ResponseWriter, r *http.Request) condResult {
	im := r.Header.Get("If-Match")
	if im == "" {
		return condNone
	}
	for {
		im = textproto.TrimString(im)
		if len(im) == 0 {
			break
		}
		if im[0] == ',' {
			im = im[1:]
			continue
		}
		if im[0] == '*' {
			return condTrue
		}
		etag, remain := scanETag(im)
		if etag == "" {
			break
		}
		if etagStrongMatch(etag, w.Header().Get("Etag")) {
			return condTrue
		}
		im = remain
	}

	return condFalse
}

func checkIfUnmodifiedSince(r *http.Request, modtime time.Time) condResult {
	ius := r.Header.Get("If-Unmodified-Since")
	if ius == "" || isZeroTime(modtime) {
		return condNone
	}
	t, err := http.ParseTime(ius)
	if err != nil {
		return condNone
	}

	// The Last-Modified header truncates sub-second precision so
	// the modtime needs to be truncated too.
	modtime = modtime.Truncate(time.Second)
	if ret := modtime.Compare(t); ret <= 0 {
		return condTrue
	}
	return condFalse
}

func checkIfNoneMatch(w http.ResponseWriter, r *http.Request) condResult {
	inm := r.Header.Get("If-None-Match")
	if inm == "" {
		return condNone
	}
	buf := inm
	for {
		buf = textproto.TrimString(buf)
		if len(buf) == 0 {
			break
		}
		if buf[0] == ',' {
			buf = buf[1:]
			continue
		}
		if buf[0] == '*' {
			return condFalse
		}
		etag, remain := scanETag(buf)
		if etag == "" {
			break
		}
		if etagWeakMatch(etag, w.Header().Get("Etag")) {
			return condFalse
		}
		buf = remain
	}
	return condTrue
}

func checkIfModifiedSince(r *http.Request, modtime time.Time) condResult {
	if r.Method != "GET" && r.Method != "HEAD" {
		return condNone
	}
	ims := r.Header.Get("If-Modified-Since")
	if ims == "" || isZeroTime(modtime) {
		return condNone
	}
	t, err := http.ParseTime(ims)
	if err != nil {
		return condNone
	}
	// The Last-Modified header truncates sub-second precision so
	// the modtime needs to be truncated too.
	modtime = modtime.Truncate(time.Second)
	if ret := modtime.Compare(t); ret <= 0 {
		return condFalse
	}
	return condTrue
}

func checkIfRange(w http.ResponseWriter, r *http.Request, modtime time.Time) condResult {
	if r.Method != "GET" && r.Method != "HEAD" {
		return condNone
	}
	ir := r.Header.Get("If-Range")
	if ir == "" {
		return condNone
	}
	etag, _ := scanETag(ir)
	if etag != "" {
		if etagStrongMatch(etag, w.Header().Get("Etag")) {
			return condTrue
		} else {
			return condFalse
		}
	}
	// The If-Range value is typically the ETag value, but it may also be
	// the modtime date. See golang.org/issue/8367.
	if modtime.IsZero() {
		return condFalse
	}
	t, err := http.ParseTime(ir)
	if err != nil {
		return condFalse
	}
	if t.Unix() == modtime.Unix() {
		return condTrue
	}
	return condFalse
}

func writeNotModified(w http.ResponseWriter) {
	// RFC 7232 section 4.1:
	// a sender SHOULD NOT generate representation metadata other than the
	// above listed fields unless said metadata exists for the purpose of
	// guiding cache updates (e.g., Last-Modified might be useful if the
	// response does not have an ETag field).
	h := w.Header()
	delete(h, "Content-Type")
	delete(h, "Content-Length")
	delete(h, "Content-Encoding")
	if h.Get("Etag") != "" {
		delete(h, "Last-Modified")
	}
	w.WriteHeader(http.StatusNotModified)
}

// errNoOverlap is returned by serveContent's parseRange if first-byte-pos of
// all of the byte-range-spec values is greater than the content size.
var errNoOverlap = errors.New("invalid range: failed to overlap")

// httpRange specifies the byte range to be sent to the client.
type httpRange struct {
	start, length int64
}

func (r httpRange) contentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, size)
}

func (r httpRange) mimeHeader(contentType string, size int64) textproto.MIMEHeader {
	return textproto.MIMEHeader{
		"Content-Range": {r.contentRange(size)},
		"Content-Type":  {contentType},
	}
}

// parseRange parses a Range header string as per RFC 7233.
// errNoOverlap is returned if none of the ranges overlap.
func parseRange(s string, size int64) ([]httpRange, error) {
	if s == "" {
		return nil, nil // header not present
	}
	const b = "bytes="
	if !strings.HasPrefix(s, b) {
		return nil, errors.New("invalid range")
	}
	var ranges []httpRange
	noOverlap := false
	for _, ra := range strings.Split(s[len(b):], ",") {
		ra = textproto.TrimString(ra)
		if ra == "" {
			continue
		}
		start, end, ok := strings.Cut(ra, "-")
		if !ok {
			return nil, errors.New("invalid range")
		}
		start, end = textproto.TrimString(start), textproto.TrimString(end)
		var r httpRange
		if start == "" {
			// If no start is specified, end specifies the
			// range start relative to the end of the file,
			// and we are dealing with <suffix-length>
			// which has to be a non-negative integer as per
			// RFC 7233 Section 2.1 "Byte-Ranges".
			if end == "" || end[0] == '-' {
				return nil, errors.New("invalid range")
			}
			i, err := strconv.ParseInt(end, 10, 64)
			if i < 0 || err != nil {
				return nil, errors.New("invalid range")
			}
			if i > size {
				i = size
			}
			r.start = size - i
			r.length = size - r.start
		} else {
			i, err := strconv.ParseInt(start, 10, 64)
			if err != nil || i < 0 {
				return nil, errors.New("invalid range")
			}
			if i >= size {
				// If the range begins after the size of the content,
				// then it does not overlap.
				noOverlap = true
				continue
			}
			r.start = i
			if end == "" {
				// If no end is specified, range extends to end of the file.
				r.length = size - r.start
			} else {
				i, err := strconv.ParseInt(end, 10, 64)
				if err != nil || r.start > i {
					return nil, errors.New("invalid range")
				}
				if i >= size {
					i = size - 1
				}
				r.length = i - r.start + 1
			}
		}
		ranges = append(ranges, r)
	}
	if noOverlap && len(ranges) == 0 {
		// The specified ranges did not overlap with the content.
		return nil, errNoOverlap
	}
	return ranges, nil
}

func sumRangesSize(ranges []httpRange) (size int64) {
	for _, ra := range ranges {
		size += ra.length
	}
	return
}

// countingWriter counts how many bytes have been written to it.
type countingWriter int64

func (w *countingWriter) Write(p []byte) (n int, err error) {
	*w += countingWriter(len(p))
	return len(p), nil
}

// rangesMIMESize returns the number of bytes it takes to encode the
// provided ranges as a multipart response.
func rangesMIMESize(ranges []httpRange, contentType string, contentSize int64) (encSize int64) {
	var w countingWriter
	mw := multipart.NewWriter(&w)
	for _, ra := range ranges {
		mw.CreatePart(ra.mimeHeader(contentType, contentSize))
		encSize += ra.length
	}
	mw.Close()
	encSize += int64(w)
	return
}

// CompressorFileServer creates another variant of http.FileServer which supports compression.
// It does not however support byte range (Accept-Ranges header), in which it will just use the normal
// http.FileServer and compression is disabled altogether.
//
// When compression is enabled, Content-Length header will be removed.
func CompressorFileServer(config *CompressorFileServerConfig, root http.FileSystem) http.Handler {
	indexFiles := []string{"index.html"}
	if config.IndexFiles != nil && len(config.IndexFiles) > 0 {
		indexFiles = config.IndexFiles
	}
	return &compressorFileServer{root: root, config: config, defaultHandler: http.FileServer(root), indexFiles: indexFiles, errorHandlers: config.ErrorHandlers}
}

func (c *compressorFileServer) error(writer http.ResponseWriter, statusCode int, err error) {
	if h, ok := c.errorHandlers[statusCode]; ok {
		h(writer, err)
		return
	}

	writer.WriteHeader(statusCode)
}

// localRedirect gives a Moved Permanently response.
// It does not convert relative paths to absolute paths like Redirect does.
func (c *compressorFileServer) localRedirect(writer http.ResponseWriter, request *http.Request, newPath string) {
	if q := request.URL.RawQuery; q != "" {
		newPath += "?" + q
	}
	writer.Header().Set("Location", newPath)
	writer.WriteHeader(http.StatusMovedPermanently)
}

// checkPreconditions evaluates request preconditions and reports whether a precondition
// resulted in sending StatusNotModified or StatusPreconditionFailed.
func (c *compressorFileServer) checkPreconditions(writer http.ResponseWriter, request *http.Request, modtime time.Time) (done bool, rangeHeader string) {
	// This function carefully follows RFC 7232 section 6.
	ch := c.checkIfMatch(writer, request)
	if ch == condNone {
		ch = checkIfUnmodifiedSince(request, modtime)
	}
	if ch == condFalse {
		writer.WriteHeader(http.StatusPreconditionFailed)
		return true, ""
	}
	switch checkIfNoneMatch(writer, request) {
	case condFalse:
		if request.Method == "GET" || request.Method == "HEAD" {
			writeNotModified(writer)
			return true, ""
		} else {
			writer.WriteHeader(http.StatusPreconditionFailed)
			return true, ""
		}
	case condNone:
		if checkIfModifiedSince(request, modtime) == condFalse {
			writeNotModified(writer)
			return true, ""
		}
	}

	rangeHeader = request.Header.Get("Range")
	if rangeHeader != "" && checkIfRange(writer, request, modtime) == condFalse {
		rangeHeader = ""
	}
	return false, rangeHeader
}

// serveContent serves content (based on http.ServeContent code).
func (c *compressorFileServer) serveContent(writer http.ResponseWriter, request *http.Request, name string, modtime time.Time, size int64, content io.ReadSeeker) {
	if !isZeroTime(modtime) {
		writer.Header().Set("Last-Modified", modtime.UTC().Format(http.TimeFormat))
	}

	done, rangeReq := c.checkPreconditions(writer, request, modtime)
	if done {
		return
	}

	code := http.StatusOK

	// If Content-Type isn't set, use the file's extension to find it, but
	// if the Content-Type is unset explicitly, do not sniff the type.
	ctypes, haveType := writer.Header()["Content-Type"]
	var ctype string
	if !haveType {
		ctype = mime.TypeByExtension(filepath.Ext(name))
		if ctype == "" {
			// read a chunk to decide between utf-8 text and binary
			// read 512 bytes
			var buf [512]byte
			n, _ := io.ReadFull(content, buf[:])
			ctype = http.DetectContentType(buf[:n])
			_, err := content.Seek(0, io.SeekStart) // rewind to output whole file
			if err != nil {
				c.error(writer, http.StatusInternalServerError, err)
				return
			}
		}
		writer.Header().Set("Content-Type", ctype)
	} else if len(ctypes) > 0 {
		ctype = ctypes[0]
	}

	// handle Content-Range header.
	sendSize := size
	var sendContent io.Reader = content
	ranges, err := parseRange(rangeReq, size)
	switch {
	case err == nil:
	case errors.Is(err, errNoOverlap):
		if size == 0 {
			// Some clients add a Range header to all requests to
			// limit the size of the response. If the file is empty,
			// ignore the range header and respond with a 200 rather
			// than a 416.
			ranges = nil
			break
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		fallthrough
	default:
		c.error(writer, http.StatusRequestedRangeNotSatisfiable, err)
		return
	}

	if sumRangesSize(ranges) > size {
		// The total number of bytes in all the ranges
		// is larger than the size of the file by
		// itself, so this is probably an attack, or a
		// dumb client. Ignore the range request.
		ranges = nil
	}
	switch {
	case len(ranges) == 1:
		// RFC 7233, Section 4.1:
		// "If a single part is being transferred, the server
		// generating the 206 response MUST generate a
		// Content-Range header field, describing what range
		// of the selected representation is enclosed, and a
		// payload consisting of the range.
		// ...
		// A server MUST NOT generate a multipart response to
		// a request for a single range, since a client that
		// does not request multiple parts might not support
		// multipart responses."
		ra := ranges[0]
		if _, err := content.Seek(ra.start, io.SeekStart); err != nil {
			c.error(writer, http.StatusRequestedRangeNotSatisfiable, err)
			return
		}
		sendSize = ra.length
		code = http.StatusPartialContent
		writer.Header().Set("Content-Range", ra.contentRange(size))
	case len(ranges) > 1:
		sendSize = rangesMIMESize(ranges, ctype, size)
		code = http.StatusPartialContent

		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		writer.Header().Set("Content-Type", "multipart/byteranges; boundary="+mw.Boundary())
		sendContent = pr
		defer pr.Close() // cause writing goroutine to fail and exit if CopyN doesn't finish.
		go func() {
			for _, ra := range ranges {
				part, err := mw.CreatePart(ra.mimeHeader(ctype, size))
				if err != nil {
					_ = pw.CloseWithError(err)
					return
				}
				if _, err := content.Seek(ra.start, io.SeekStart); err != nil {
					_ = pw.CloseWithError(err)
					return
				}
				if _, err := io.CopyN(part, content, ra.length); err != nil {
					_ = pw.CloseWithError(err)
					return
				}
			}
			_ = mw.Close()
			_ = pw.Close()
		}()
	}

	writer.Header().Set("Accept-Ranges", "bytes")
	if writer.Header().Get("Content-Encoding") == "" {
		writer.Header().Set("Content-Length", strconv.FormatInt(sendSize, 10))
	}

	writer.WriteHeader(code)

	if request.Method != "HEAD" {
		_, _ = io.CopyN(writer, sendContent, sendSize)
	}
}

// handleServeContent handles serving files (copied from http.ServeFile implementation).
// However, this one does not support directory listing and supports different error handler and configurable
// index files.
func (c *compressorFileServer) handleServeContent(writer http.ResponseWriter, request *http.Request) {
	upath := request.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
		request.URL.Path = upath
	}

	name := path.Clean(upath)

	f, err := c.root.Open(name)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, fs.ErrNotExist) {
			code = http.StatusNotFound
		} else if errors.Is(err, fs.ErrPermission) {
			code = http.StatusForbidden
		}

		c.error(writer, code, err)
		return
	}
	defer f.Close()

	d, err := f.Stat()
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, fs.ErrNotExist) {
			code = http.StatusNotFound
		} else if errors.Is(err, fs.ErrPermission) {
			code = http.StatusForbidden
		}

		c.error(writer, code, err)
		return
	}

	// redirect to canonical path: / at end of directory url
	// r.URL.Path always begins with /
	url := request.URL.Path
	if d.IsDir() {
		if url[len(url)-1] != '/' {
			c.localRedirect(writer, request, path.Base(url)+"/")
			return
		}
	} else {
		if url[len(url)-1] == '/' {
			c.localRedirect(writer, request, "../"+path.Base(url))
			return
		}
	}

	var ff http.File
	if d.IsDir() {
		url := request.URL.Path
		// redirect if the directory name doesn't end in a slash
		if url == "" || url[len(url)-1] != '/' {
			c.localRedirect(writer, request, path.Base(url)+"/")
			return
		}

		for _, indexPage := range c.indexFiles {
			// use contents of index.html for directory, if present
			index := strings.TrimSuffix(name, "/") + indexPage
			ff, err = c.root.Open(index)
			if err == nil {
				dd, err := ff.Stat()
				if err == nil {
					d = dd
					f = ff
					break
				}
			}
		}
	}
	if ff != nil {
		defer ff.Close()
	}

	// Still a directory? (we didn't find an index.html file)
	if d.IsDir() {
		c.error(writer, http.StatusNotFound, fs.ErrNotExist)
		return
	}

	c.serveContent(writer, request, d.Name(), d.ModTime(), d.Size(), f)
}

func (c *compressorFileServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	acceptEncoding := request.Header.Get("Accept-Encoding")
	rangeHeader := request.Header.Get("Range")
	if acceptEncoding == "" || rangeHeader != "" {
		c.handleServeContent(writer, request)
		return
	}

	algs := make([]string, 0)
	em := map[string]float64{}
	rawEncodings := strings.Split(strings.ToLower(strings.TrimSpace(acceptEncoding)), ",")
	if len(rawEncodings) == 0 {
		c.handleServeContent(writer, request)
		return
	}

	for _, re := range rawEncodings {
		vals := strings.Split(strings.TrimSpace(re), ";")
		q := 1.0
		var err error
		alg := strings.TrimSpace(vals[0])
		if len(vals) == 2 {
			q, err = strconv.ParseFloat(strings.TrimSpace(vals[1]), 32)
			if err != nil {
				q = 1.0
			}
		}

		if _, ok := em[alg]; !ok {
			em[alg] = q
			algs = append(algs, alg)
		}
	}

	slices.SortStableFunc(algs, func(a, b string) int {
		return -cmp.Compare(em[a], em[b])
	})

	usedAlg := algs[0]
	if c.config.EnableBrotli && usedAlg == "br" {
		bw := NewBrotliResponseWriter(writer, c.config.BrotliEncoder, c.config.MinLength, c.config.ContentTypes)
		defer bw.Close()

		c.handleServeContent(bw, request)
		return
	} else if c.config.EnableLzw && usedAlg == "compress" {
		lw := NewLzwResponseWriter(writer, c.config.MinLength, c.config.ContentTypes)
		defer lw.Close()

		c.handleServeContent(lw, request)
		return
	} else if c.config.EnableZlib && usedAlg == "deflate" {
		zw := NewZlibResponseWriter(writer, c.config.ZlibQuality, c.config.MinLength, c.config.ContentTypes)
		defer zw.Close()

		c.handleServeContent(zw, request)
		return
	} else if c.config.EnableGzip && usedAlg == "gzip" {
		gw := NewGzipResponseWriter(writer, c.config.GzipQuality, c.config.MinLength, c.config.ContentTypes)
		defer gw.Close()

		c.handleServeContent(gw, request)
		return
	} else {
		c.handleServeContent(writer, request)
		return
	}
}

// GzipResponseWriter is a http.ResponseWriter which supports writing content
// with gzip compression. This response writer does not care about the Accept-Encoding header,
// and will write Content-Encoding header immediately just before the first call of Write.
//
// Compression headers are only sent before first call to write to be able to disable compression in case
// content type and length are outside the defined parameters. The response writer checks if Content-Type and
// Content-Length headers were set before the first call to Write to see the type and length of the content sent.
//
// This response writer requires closing, so it must be closed after all writes have been completed.
type GzipResponseWriter struct {
	http.ResponseWriter
	contentTypes map[string]struct{}
	minLength    int
	gw           *gzip.Writer
	once         *sync.Once
	level        int
}

// NewGzipResponseWriter creates a new compression response writer.
func NewGzipResponseWriter(writer http.ResponseWriter, level, minLength int, contentTypes []string) *GzipResponseWriter {
	ctm := make(map[string]struct{})
	for _, ct := range contentTypes {
		ctm[ct] = struct{}{}
	}
	return &GzipResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: minLength, contentTypes: ctm, level: level}
}

func (grw *GzipResponseWriter) writeHeader() {
	grw.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	grw.ResponseWriter.Header().Del("Content-Length")
}

func (grw *GzipResponseWriter) createWriter() {
	grw.gw, _ = gzip.NewWriterLevel(grw.ResponseWriter, grw.level)
}

func (grw *GzipResponseWriter) enable() {
	if (grw.contentTypes == nil || len(grw.contentTypes) == 0) && grw.minLength <= 0 {
		grw.writeHeader()
		grw.createWriter()
		return
	}

	h := grw.ResponseWriter.Header()
	cl := h.Get("Content-Length")
	length, _ := strconv.Atoi(cl)
	ct := h.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil && length > grw.minLength {
		grw.writeHeader()
		grw.createWriter()
		return
	}

	if _, ok := grw.contentTypes[mt]; ok && length > grw.minLength {
		grw.writeHeader()
		grw.createWriter()
		return
	}
}

func (grw *GzipResponseWriter) WriteHeader(statusCode int) {
	grw.once.Do(grw.enable)
	grw.ResponseWriter.WriteHeader(statusCode)
}

// Write writes data to response with compression (if compression can be enabled).
// First call to Write (or WriteHeader) will check if compression can be enabled. If not it will behave just like a normal
// http.ResponseWriter.
func (grw *GzipResponseWriter) Write(data []byte) (int, error) {
	grw.once.Do(grw.enable)

	if grw.gw == nil {
		return grw.ResponseWriter.Write(data)
	}

	return grw.gw.Write(data)
}

func (grw *GzipResponseWriter) Flush() {
	if grw.gw == nil {
		return
	}

	_ = grw.gw.Flush()

	if fw, ok := grw.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
}

// Close closes this writer by flushing and sending the compression footer bytes.
func (grw *GzipResponseWriter) Close() error {
	if grw.gw == nil {
		return nil
	}

	return grw.gw.Close()
}

func (grw *GzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := grw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}

	return nil, nil, fmt.Errorf("http.Hijacker interface is not supported")
}

// BrotliCompressorEncoder is a custom compressor.
// This is used to supply a custom Brotli encoder. It is designed this way so go-httputil does not
// depend on the cbrotli library which requires Brotli C headers and cgo.
type BrotliCompressorEncoder interface {
	io.WriteCloser
	Flush() error
}

// BrotliResponseWriter is a http.ResponseWriter which supports writing content
// with Brotli compression. This response writer does not care about the Accept-Encoding header,
// and will write Content-Encoding header immediately just before the first call of Write.
//
// Compression headers are only sent before first call to write to be able to disable compression in case
// content type and length are outside the defined parameters. The response writer checks if Content-Type and
// Content-Length headers were set before the first call to Write to see the type and length of the content sent.
//
// This response writer requires closing, so it must be closed after all writes have been completed.
type BrotliResponseWriter struct {
	http.ResponseWriter
	contentTypes map[string]struct{}
	minLength    int
	encoder      BrotliCompressorEncoder
	bw           BrotliCompressorEncoder
	once         *sync.Once
	quality      int
}

// NewBrotliResponseWriter creates a new compression response writer.
func NewBrotliResponseWriter(writer http.ResponseWriter, brotliEncoder BrotliCompressorEncoder, minLength int, contentTypes []string) *BrotliResponseWriter {
	ctm := make(map[string]struct{})
	for _, ct := range contentTypes {
		ctm[ct] = struct{}{}
	}
	return &BrotliResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: minLength, contentTypes: ctm, encoder: brotliEncoder}
}

func (brw *BrotliResponseWriter) writeHeader() {
	brw.ResponseWriter.Header().Set("Content-Encoding", "br")
	brw.ResponseWriter.Header().Del("Content-Length")
}

func (brw *BrotliResponseWriter) createWriter() {
	brw.bw = brw.encoder
}

func (brw *BrotliResponseWriter) enable() {
	if (brw.contentTypes == nil || len(brw.contentTypes) == 0) && brw.minLength <= 0 {
		brw.writeHeader()
		brw.createWriter()
		return
	}

	h := brw.ResponseWriter.Header()
	cl := h.Get("Content-Length")
	length, _ := strconv.Atoi(cl)
	ct := h.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil && length > brw.minLength {
		brw.writeHeader()
		brw.createWriter()
		return
	}

	if _, ok := brw.contentTypes[mt]; ok && length > brw.minLength {
		brw.writeHeader()
		brw.createWriter()
		return
	}
}

func (brw *BrotliResponseWriter) WriteHeader(statusCode int) {
	brw.once.Do(brw.enable)
	brw.ResponseWriter.WriteHeader(statusCode)
}

// Write writes data to response with compression (if compression can be enabled).
// First call to Write (or WriteHeader) will check if compression can be enabled. If not it will behave just like a normal
// http.ResponseWriter.
func (brw *BrotliResponseWriter) Write(data []byte) (int, error) {
	brw.once.Do(brw.enable)

	if brw.bw == nil {
		return brw.ResponseWriter.Write(data)
	}

	return brw.bw.Write(data)
}

func (brw *BrotliResponseWriter) Flush() {
	if brw.bw == nil {
		return
	}

	_ = brw.bw.Flush()

	if fw, ok := brw.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
}

// Close closes this writer by flushing and sending the compression footer bytes.
func (brw *BrotliResponseWriter) Close() error {
	if brw.bw == nil {
		return nil
	}

	return brw.bw.Close()
}

func (brw *BrotliResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := brw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}

	return nil, nil, fmt.Errorf("http.Hijacker interface is not supported")
}

// LzwResponseWriter is a http.ResponseWriter which supports writing content
// with LZW ("compress") compression. This response writer does not care about the Accept-Encoding header,
// and will write Content-Encoding header immediately just before the first call of Write.
//
// Compression headers are only sent before first call to write to be able to disable compression in case
// content type and length are outside the defined parameters. The response writer checks if Content-Type and
// Content-Length headers were set before the first call to Write to see the type and length of the content sent.
//
// This response writer requires closing, so it must be closed after all writes have been completed.
type LzwResponseWriter struct {
	http.ResponseWriter
	contentTypes map[string]struct{}
	minLength    int
	lw           io.WriteCloser
	once         *sync.Once
}

// NewLzwResponseWriter creates a new compression response writer.
func NewLzwResponseWriter(writer http.ResponseWriter, minLength int, contentTypes []string) *LzwResponseWriter {
	ctm := make(map[string]struct{})
	for _, ct := range contentTypes {
		ctm[ct] = struct{}{}
	}
	return &LzwResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: minLength, contentTypes: ctm}
}

func (lrw *LzwResponseWriter) writeHeader() {
	lrw.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	lrw.ResponseWriter.Header().Del("Content-Length")
}

func (lrw *LzwResponseWriter) createWriter() {
	lrw.lw = lzw.NewWriter(lrw.ResponseWriter, lzw.LSB, 8)
}

func (lrw *LzwResponseWriter) enable() {
	if (lrw.contentTypes == nil || len(lrw.contentTypes) == 0) && lrw.minLength <= 0 {
		lrw.writeHeader()
		lrw.createWriter()
		return
	}

	h := lrw.ResponseWriter.Header()
	cl := h.Get("Content-Length")
	length, _ := strconv.Atoi(cl)
	ct := h.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil && length > lrw.minLength {
		lrw.writeHeader()
		lrw.createWriter()
		return
	}

	if _, ok := lrw.contentTypes[mt]; ok && length > lrw.minLength {
		lrw.writeHeader()
		lrw.createWriter()
		return
	}
}

func (lrw *LzwResponseWriter) WriteHeader(statusCode int) {
	lrw.once.Do(lrw.enable)
	lrw.ResponseWriter.WriteHeader(statusCode)
}

// Write writes data to response with compression (if compression can be enabled).
// First call to Write (or WriteHeader) will check if compression can be enabled. If not it will behave just like a normal
// http.ResponseWriter.
func (lrw *LzwResponseWriter) Write(data []byte) (int, error) {
	lrw.once.Do(lrw.enable)

	if lrw.lw == nil {
		return lrw.ResponseWriter.Write(data)
	}

	return lrw.lw.Write(data)
}

func (lrw *LzwResponseWriter) Flush() {
	if fw, ok := lrw.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
}

// Close closes this writer by flushing and sending the compression footer bytes.
func (lrw *LzwResponseWriter) Close() error {
	if lrw.lw == nil {
		return nil
	}

	return lrw.lw.Close()
}

func (lrw *LzwResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := lrw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}

	return nil, nil, fmt.Errorf("http.Hijacker interface is not supported")
}

// ZlibResponseWriter is a http.ResponseWriter which supports writing content
// with zlib ("deflate") compression. This response writer does not care about the Accept-Encoding header,
// and will write Content-Encoding header immediately just before the first call of Write.
//
// Compression headers are only sent before first call to write to be able to disable compression in case
// content type and length are outside the defined parameters. The response writer checks if Content-Type and
// Content-Length headers were set before the first call to Write to see the type and length of the content sent.
//
// This response writer requires closing, so it must be closed after all writes have been completed.
type ZlibResponseWriter struct {
	http.ResponseWriter
	contentTypes map[string]struct{}
	minLength    int
	zw           *zlib.Writer
	once         *sync.Once
	level        int
}

func NewZlibResponseWriter(writer http.ResponseWriter, level, minLength int, contentTypes []string) *ZlibResponseWriter {
	ctm := make(map[string]struct{})
	for _, ct := range contentTypes {
		ctm[ct] = struct{}{}
	}
	return &ZlibResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: minLength, contentTypes: ctm, level: level}
}

func (zrw *ZlibResponseWriter) writeHeader() {
	zrw.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	zrw.ResponseWriter.Header().Del("Content-Length")
}

func (zrw *ZlibResponseWriter) createWriter() {
	zrw.zw, _ = zlib.NewWriterLevel(zrw.ResponseWriter, zrw.level)
}

func (zrw *ZlibResponseWriter) enable() {
	if (zrw.contentTypes == nil || len(zrw.contentTypes) == 0) && zrw.minLength <= 0 {
		zrw.writeHeader()
		zrw.createWriter()
		return
	}

	h := zrw.ResponseWriter.Header()
	cl := h.Get("Content-Length")
	length, _ := strconv.Atoi(cl)
	ct := h.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil && length > zrw.minLength {
		zrw.writeHeader()
		zrw.createWriter()
		return
	}

	if _, ok := zrw.contentTypes[mt]; ok && length > zrw.minLength {
		zrw.writeHeader()
		zrw.createWriter()
		return
	}
}

func (zrw *ZlibResponseWriter) WriteHeader(statusCode int) {
	zrw.once.Do(zrw.enable)
	zrw.ResponseWriter.WriteHeader(statusCode)
}

// Write writes data to response with compression (if compression can be enabled).
// First call to Write (or WriteHeader) will check if compression can be enabled. If not it will behave just like a normal
// http.ResponseWriter.
func (zrw *ZlibResponseWriter) Write(data []byte) (int, error) {
	zrw.once.Do(zrw.enable)

	if zrw.zw == nil {
		return zrw.ResponseWriter.Write(data)
	}

	return zrw.zw.Write(data)
}

func (zrw *ZlibResponseWriter) Flush() {
	if zrw.zw == nil {
		return
	}

	_ = zrw.zw.Flush()

	if fw, ok := zrw.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
}

// Close closes this writer by flushing and sending the compression footer bytes.
func (zrw *ZlibResponseWriter) Close() error {
	if zrw.zw == nil {
		return nil
	}

	return zrw.zw.Close()
}

func (zrw *ZlibResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := zrw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}

	return nil, nil, fmt.Errorf("http.Hijacker interface is not supported")
}
