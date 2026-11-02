package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

var gzipWriterPool = sync.Pool{
	New: func() any {
		return gzip.NewWriter(io.Discard)
	},
}

// gzipResponseWriter 延迟发送响应头:先缓存状态码,
// 到首个 Write 时根据 Content-Type 决定是否压缩,再真正 WriteHeader。
// 直接透传 WriteHeader 会导致之后设置的 Content-Encoding 无法生效。
type gzipResponseWriter struct {
	http.ResponseWriter
	gw          *gzip.Writer
	status      int
	wroteHeader bool
	decided     bool
	compress    bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	// 不透传,等 decide() 里统一发。
}

func (w *gzipResponseWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	ct := w.Header().Get("Content-Type")
	if compressibleType(ct) && w.Header().Get("Content-Encoding") == "" {
		w.compress = true
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		w.Header().Add("Vary", "Accept-Encoding")
		w.gw = gzipWriterPool.Get().(*gzip.Writer)
		w.gw.Reset(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(w.status)
}

func compressibleType(ct string) bool {
	if ct == "" {
		return false
	}
	ct = strings.ToLower(ct)
	switch {
	case strings.HasPrefix(ct, "text/"),
		strings.Contains(ct, "javascript"),
		strings.Contains(ct, "json"),
		strings.Contains(ct, "image/svg"):
		return true
	}
	return false
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	w.decide()
	if w.compress {
		return w.gw.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *gzipResponseWriter) close() {
	if w.gw != nil {
		_ = w.gw.Close()
		gzipWriterPool.Put(w.gw)
		w.gw = nil
		return
	}
	if !w.decided {
		// handler 没有写过 body(例如纯 302),仍要把缓存的响应头发出去。
		w.decide()
	}
}

// gzipMiddleware 对可压缩的响应体做 gzip,客户端不支持时透传。
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		grw := &gzipResponseWriter{ResponseWriter: w}
		defer grw.close()
		next.ServeHTTP(grw, r)
	})
}
