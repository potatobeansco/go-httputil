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
	EnableBrotli  bool
	EnableGzip    bool
	EnableZlib    bool
	EnableLzw     bool
	BrotliQuality int
	GzipQuality   int
	ZlibQuality   int
	ContentTypes  []string
	MinLength     int
}

type compressorFileServer struct {
	config         *CompressorFileServerConfig
	root           http.FileSystem
	defaultHandler http.Handler
}

func CompressorFileServer(config *CompressorFileServerConfig, root http.FileSystem) http.Handler {
	return &compressorFileServer{root: root, config: config, defaultHandler: http.FileServer(root)}
}

func (c compressorFileServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	acceptEncoding := request.Header.Get("Accept-Encoding")
	if acceptEncoding == "" {
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

type GzipResponseWriter struct {
	http.ResponseWriter
	contentTypes map[string]struct{}
	minLength    int
	gw           *gzip.Writer
	once         *sync.Once
	level        int
}

func NewGzipResponseWriter(writer http.ResponseWriter, level, minLength int, contentTypes []string) *GzipResponseWriter {
	ctm := make(map[string]struct{})
	for _, ct := range contentTypes {
		ctm[ct] = struct{}{}
	}
	return &GzipResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: minLength, contentTypes: ctm, level: level}
}

func (grw *GzipResponseWriter) writeHeader() {
	grw.ResponseWriter.Header().Set("Content-Encoding", "gzip")
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

type BrotliResponseWriter struct {
	http.ResponseWriter
	contentTypes map[string]struct{}
	minLength    int
	bw           *cbrotli.Writer
	once         *sync.Once
	quality      int
}

func NewBrotliResponseWriter(writer http.ResponseWriter, quality, minLength int, contentTypes []string) *BrotliResponseWriter {
	ctm := make(map[string]struct{})
	for _, ct := range contentTypes {
		ctm[ct] = struct{}{}
	}
	return &BrotliResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: minLength, contentTypes: ctm, quality: quality}
}

func (brw *BrotliResponseWriter) writeHeader() {
	brw.ResponseWriter.Header().Set("Content-Encoding", "br")
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

type LzwResponseWriter struct {
	http.ResponseWriter
	contentTypes map[string]struct{}
	minLength    int
	lw           io.WriteCloser
	once         *sync.Once
}

func NewLzwResponseWriter(writer http.ResponseWriter, minLength int, contentTypes []string) *LzwResponseWriter {
	ctm := make(map[string]struct{})
	for _, ct := range contentTypes {
		ctm[ct] = struct{}{}
	}
	return &LzwResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: minLength, contentTypes: ctm}
}

func (lrw *LzwResponseWriter) writeHeader() {
	lrw.ResponseWriter.Header().Set("Content-Encoding", "gzip")
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
