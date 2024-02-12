package httputil

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"fmt"
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
	EnableDeflate bool
	EnableLzw     bool
	ContentTypes  []string
}

type compressorFileServer struct {
	config               *CompressorFileServerConfig
	acceptedContentTypes map[string]struct{}
	root                 http.FileSystem
}

func (c compressorFileServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	acceptEncoding := request.Header.Get("Accept-Encoding")
	if acceptEncoding == "" {
		http.FileServer(c.root).ServeHTTP(writer, request)
		return
	}

	algs := make([]string, 0)
	em := map[string]float64{}
	rawEncodings := strings.Split(strings.ToLower(strings.TrimSpace(acceptEncoding)), ",")
	if len(rawEncodings) == 0 {
		http.FileServer(c.root).ServeHTTP(writer, request)
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
		http.FileServer(c.root).ServeHTTP(writer, request)
		return
	} else if c.config.EnableLzw && usedAlg == "compress" {
		http.FileServer(c.root).ServeHTTP(writer, request)
		return
	} else if c.config.EnableDeflate && usedAlg == "deflate" {
		http.FileServer(c.root).ServeHTTP(writer, request)
		return
	} else if c.config.EnableGzip && usedAlg == "gzip" {
		gw := NewGzipResponseWriter(writer)
		defer gw.Close()

		http.FileServer(c.root).ServeHTTP(gw, request)
		return
	} else {
		http.FileServer(c.root).ServeHTTP(writer, request)
		return
	}
}

type GzipResponseWriter struct {
	http.ResponseWriter
	contentTypes map[string]struct{}
	minLength    int
	gw           *gzip.Writer
	once         *sync.Once
}

func NewGzipResponseWriter(writer http.ResponseWriter) *GzipResponseWriter {
	return &GzipResponseWriter{ResponseWriter: writer, once: &sync.Once{}, minLength: 1024, contentTypes: make(map[string]struct{})}
}

func (grw *GzipResponseWriter) writeHeader() {
	grw.ResponseWriter.Header().Set("Content-Encoding", "gzip")
}

func (grw *GzipResponseWriter) enable() {
	if (grw.contentTypes == nil || len(grw.contentTypes) == 0) && grw.minLength <= 0 {
		grw.writeHeader()
		grw.gw = gzip.NewWriter(grw.ResponseWriter)
		return
	}

	h := grw.ResponseWriter.Header()
	cl := h.Get("Content-Length")
	length, _ := strconv.Atoi(cl)
	ct := h.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil && length > grw.minLength {
		grw.writeHeader()
		grw.gw = gzip.NewWriter(grw.ResponseWriter)
		return
	}

	if _, ok := grw.contentTypes[mt]; ok && length > grw.minLength {
		grw.writeHeader()
		grw.gw = gzip.NewWriter(grw.ResponseWriter)
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

func CompressorFileServer(config *CompressorFileServerConfig, root http.FileSystem) http.Handler {
	cts := make(map[string]struct{})
	for _, ct := range config.ContentTypes {
		cts[ct] = struct{}{}
	}
	return &compressorFileServer{root: root, config: config, acceptedContentTypes: cts}
}
