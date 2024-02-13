package httputil

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"compress/lzw"
	"compress/zlib"
	"fmt"
	"github.com/google/brotli/go/cbrotli"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type CompressorFileServerConfig struct {
	// Enables the "br" compression.
	EnableBrotli bool
	// Enables the "gzip" compression.
	EnableGzip bool
	// Enables the "deflate" compression.
	EnableZlib bool
	// Enables the "compress" compression.
	EnableLzw bool
	// Brotli quality level: 0 to 11.
	BrotliQuality int
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
	MinLength int
}

type compressorFileServer struct {
	config         *CompressorFileServerConfig
	root           http.FileSystem
	defaultHandler http.Handler
}

// CompressorFileServer creates another variant of http.FileServer which supports compression.
// It does not however support byte range (Accept-Ranges header), in which it will just use the normal
// http.FileServer and compression is disabled altogether.
//
// When compression is enabled, Content-Length header will be removed.
func CompressorFileServer(config *CompressorFileServerConfig, root http.FileSystem) http.Handler {
	return &compressorFileServer{root: root, config: config, defaultHandler: http.FileServer(root)}
}

func (c compressorFileServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	acceptEncoding := request.Header.Get("Accept-Encoding")
	acceptRanges := request.Header.Get("Accept-Ranges")
	if acceptEncoding == "" || acceptRanges != "" {
		c.defaultHandler.ServeHTTP(writer, request)
		return
	}

	algs := make([]string, 0)
	em := map[string]float64{}
	rawEncodings := strings.Split(strings.ToLower(strings.TrimSpace(acceptEncoding)), ",")
	if len(rawEncodings) == 0 {
		c.defaultHandler.ServeHTTP(writer, request)
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
		bw := NewBrotliResponseWriter(writer, c.config.BrotliQuality, c.config.MinLength, c.config.ContentTypes)
		defer bw.Close()

		c.defaultHandler.ServeHTTP(bw, request)
		return
	} else if c.config.EnableLzw && usedAlg == "compress" {
		lw := NewLzwResponseWriter(writer, c.config.MinLength, c.config.ContentTypes)
		defer lw.Close()

		c.defaultHandler.ServeHTTP(lw, request)
		return
	} else if c.config.EnableZlib && usedAlg == "deflate" {
		zw := NewZlibResponseWriter(writer, c.config.ZlibQuality, c.config.MinLength, c.config.ContentTypes)
		defer zw.Close()

		c.defaultHandler.ServeHTTP(zw, request)
		return
	} else if c.config.EnableGzip && usedAlg == "gzip" {
		gw := NewGzipResponseWriter(writer, c.config.GzipQuality, c.config.MinLength, c.config.ContentTypes)
		defer gw.Close()

		c.defaultHandler.ServeHTTP(gw, request)
		return
	} else {
		c.defaultHandler.ServeHTTP(writer, request)
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
	bw           *cbrotli.Writer
	once         *sync.Once
	quality      int
}

// NewBrotliResponseWriter creates a new compression response writer.
func NewBrotliResponseWriter(writer http.ResponseWriter, quality, minLength int, contentTypes []string) *BrotliResponseWriter {
	ctm := make(map[string]struct{})
	for _, ct := range contentTypes {
		ctm[ct] = struct{}{}
	}
	return &BrotliResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: minLength, contentTypes: ctm, quality: quality}
}

func (brw *BrotliResponseWriter) writeHeader() {
	brw.ResponseWriter.Header().Set("Content-Encoding", "br")
	brw.ResponseWriter.Header().Del("Content-Length")
}

func (brw *BrotliResponseWriter) createWriter() {
	brw.bw = cbrotli.NewWriter(brw.ResponseWriter, cbrotli.WriterOptions{Quality: brw.quality})
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
