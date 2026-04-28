package mono

import (
	"compress/gzip"
	"context"
	"fmt"
	"golang.org/x/time/rate"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type MiddlewareFunc = func(HandlerFunc) HandlerFunc

func MiddlewareRpsLimitPerIP(rps float64, burst int) MiddlewareFunc {
	lock := sync.RWMutex{}
	states := map[string]*rate.Limiter{}
	lastCleanup := time.Now()
	const cleanupInterval = time.Minute

	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
			start := time.Now()
			if start.Sub(lastCleanup) > cleanupInterval {
				lock.Lock()
				if start.Sub(lastCleanup) > cleanupInterval {
					for ip, limiter := range states {
						if math.Abs(limiter.Tokens()-float64(burst)) <= 1e-6 {
							delete(states, ip)
						}
					}
					lastCleanup = start
				}
				lock.Unlock()
			}

			ip, _, err := net.SplitHostPort(req.RemoteAddr)
			if err != nil {
				ip = req.RemoteAddr
			}

			lock.RLock()
			limiter, ok := states[ip]
			lock.RUnlock()
			if !ok {
				lock.Lock()
				limiter, ok = states[ip]
				if !ok {
					limiter = rate.NewLimiter(rate.Limit(rps), burst)
					states[ip] = limiter
				}
				lock.Unlock()
			}

			weight := 1
			if isMediaExt(filepath.Ext(req.URL.Path)) {
				weight = 5
			}

			if !limiter.AllowN(start, weight) {
				http.Error(rw, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
				return nil
			}

			return next(ctx, rw, req)
		}
	}
}

func MiddlewareCachePolicy(timeout ...time.Duration) MiddlewareFunc {
	var cacheDuration time.Duration
	if len(timeout) > 0 {
		cacheDuration = timeout[0]
	}

	mediaCacheDuration := time.Hour
	if len(timeout) > 1 {
		mediaCacheDuration = timeout[1]
	}

	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
			duration := cacheDuration
			if mediaCacheDuration > 0 && isMediaExt(req.URL.Path) {
				duration = mediaCacheDuration
			}

			if duration == 0 {
				rw.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
				rw.Header().Set("Pragma", "no-cache")
				rw.Header().Set("Expires", "0")
			} else {
				rw.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(duration.Seconds())))
				rw.Header().Set("Expires", time.Now().UTC().Add(duration).Format(http.TimeFormat))
			}

			return next(ctx, rw, req)
		}
	}
}

func MiddlewareHeaders(headers ...string) MiddlewareFunc {
	if len(headers)%2 != 0 {
		panic("MiddlewareHeaders: not even number of headers")
	}
	return func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
			for header := range slices.Chunk(headers, 2) {
				rw.Header().Set(header[0], header[1])
			}
			return next(ctx, rw, req)
		}
	}
}

func MiddlewareCompressor(handlerFunc HandlerFunc) HandlerFunc {
	return func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
		if strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") && !isMediaExt(req.URL.Path) {
			gz, err := gzip.NewWriterLevel(rw, gzip.BestSpeed)
			if err != nil {
				return handlerFunc(ctx, rw, req)
			}
			defer gz.Close()

			rw.Header().Set("Content-Encoding", "gzip")
			rw.Header().Del("Content-Length") // length changes after compression

			return handlerFunc(ctx, &gzipResponseWriter{
				ResponseWriter: rw,
				writer:         gz,
			}, req)
		}

		return handlerFunc(ctx, rw, req)
	}
}

func isMediaExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".ico", ".bmp", ".tiff":
		return true
	case ".mp4", ".webm", ".mkv", ".avi", ".mov", ".flv", ".wmv":
		return true
	case ".mp3", ".ogg", ".aac", ".flac", ".wav", ".wma", ".opus":
		return true
	case ".woff", ".woff2", ".eot", ".ttf", ".otf":
		return true
	case ".zip", ".gz", ".br", ".zst", ".bz2", ".xz", ".7z", ".rar", ".tar":
		return true
	case ".pdf", ".docx", ".xlsx", ".pptx":
		return true
	default:
		return false
	}
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.writer.Write(b)
}

func (g *gzipResponseWriter) Flush() {
	g.writer.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return g.ResponseWriter
}

func middlewareStripTrailingSlash(handlerFunc HandlerFunc) HandlerFunc {
	return func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
		if len(req.URL.Path) > 1 && req.URL.Path[len(req.URL.Path)-1] == '/' {
			req.URL.Path = strings.TrimRight(req.URL.Path, "/")
		}
		return handlerFunc(ctx, rw, req)
	}
}
