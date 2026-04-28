package mono

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

type HandlerFunc func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error

func (fn HandlerFunc) Endpoint(endpoints Endpoints) error {
	endpoints["/"] = fn
	return nil
}

var _ Endpoint = (*HandlerFunc)(nil)

func New(config ...Config) *Monotonic {
	result := &Monotonic{
		server:    http.NewServeMux(),
		endpoints: map[string]HandlerFunc{},
	}
	result.baseContext, result.baseContextCancel = context.WithCancel(context.Background())
	robots := ""
	var headers []string
	for _, cfg := range config {
		result.port = cfg.Port
		result.tls = cfg.Tls
		result.quiet = cfg.Quiet
		result.timeout = cfg.Timeout
		result.onError = cfg.OnError
		result.tlsEmail = cfg.TlsContactEmail
		result.tlsCacheDir = cfg.TlsCacheDir
		result.tlsDomains = cfg.Domains
		if cfg.RobotsTxt != "" {
			robots = cfg.RobotsTxt
		}
		if cfg.Headers != nil {
			headers = append(headers, cfg.Headers...)
		}
	}
	if robots == "" {
		robots = `User-agent: *
Disallow: /_monotonic_*/
Allow: /`
	}
	if robots != "nil" {
		result.Endpoint(HandlerFunc(func(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
			_, err := rw.Write([]byte(robots))
			return err
		}), "/robots.txt")
	}
	if headers == nil {
		headers = []string{
			"X-Frame-Options", "DENY", // prevent clickjacking
			"Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'", // XSS
			"Referrer-Policy", "strict-origin-when-cross-origin",
		}
	}
	if len(headers) > 0 {
		result.Middleware(MiddlewareHeaders(headers...))
	}

	if result.timeout == 0 {
		result.timeout = time.Minute
	}
	if result.onError == nil {
		result.onError = func(err error) {}
	}

	return result.Middleware(middlewareStripTrailingSlash)
}

type Config struct {
	Port      int64
	Quiet     bool
	Timeout   time.Duration
	OnError   func(err error)
	RobotsTxt string
	Headers   []string

	Tls             bool
	Domains         []string
	TlsContactEmail string
	TlsCacheDir     string
}

type Monotonic struct {
	port        int64
	tls         bool
	tlsCacheDir string
	tlsDomains  []string
	tlsEmail    string
	quiet       bool
	timeout     time.Duration
	onError     func(err error)

	server          *http.ServeMux
	buildLock       sync.Mutex
	middlewareFuncs []MiddlewareFunc
	errors          []error

	endpoints     Endpoints
	endpointsLock sync.Mutex

	baseContext       context.Context
	baseContextCancel context.CancelFunc
}

type Endpoints map[string]HandlerFunc

type Endpoint interface {
	Endpoint(endpoints Endpoints) error
}

func (mono *Monotonic) Param(param string, value any) *Monotonic {
	switch param {
	case "port":
		mono.port = value.(int64)
	case "quiet":
		mono.quiet = value.(bool)
	case "timeout":
		mono.timeout = value.(time.Duration)
	case "onError":
		mono.onError = value.(func(err error))
	case "tls":
		mono.tls = value.(bool)
	case "on_error":
		mono.onError = value.(func(err error))
	default:
		slog.Error("unknown param", "param", param)
	}
	return mono
}

func (mono *Monotonic) Endpoint(endpoint Endpoint, prefix ...string) *Monotonic {
	endpoints := map[string]HandlerFunc{}
	pref, err := url.JoinPath("/", prefix...)
	if err != nil {
		mono.errors = append(mono.errors, err)
		return mono
	}
	if err := endpoint.Endpoint(endpoints); err != nil {
		mono.errors = append(mono.errors, err)
		return mono
	}
	for path, endpoint := range endpoints {
		mono.newEndpoint(filepath.Clean(pref+"/"+path), endpoint)
	}
	return mono
}

func (mono *Monotonic) Middleware(fn ...MiddlewareFunc) *Monotonic {
	mono.middlewareFuncs = append(mono.middlewareFuncs, fn...)
	return mono
}

func (mono *Monotonic) Start() error {
	if err := mono.build(); err != nil {
		return err
	}
	addr := fmt.Sprintf(":%d", mono.port)
	if !mono.quiet {
		link := fmt.Sprintf("%s://localhost%s",
			func() string {
				if mono.tls {
					return "https"
				} else {
					return "http"
				}
			}(),
			addr,
		)
		fmt.Printf("Errors: %v\n", mono.errors)
		fmt.Printf(">> Listening on %s\n", link)
		endpointsList := []string{}
		for endpoint := range mono.endpoints {
			endpointsList = append(endpointsList, fmt.Sprintf("%s%s", link, endpoint))
		}
		sort.Strings(endpointsList)
		fmt.Printf("%s", strings.Join(endpointsList, "\n"))
	}
	if !mono.tls {
		return http.ListenAndServe(addr, mono.server)
	}
	return mono.listenAndServeTLS(addr)
}

func (mono *Monotonic) newEndpoint(path string, handler HandlerFunc) {
	mono.endpointsLock.Lock()
	defer mono.endpointsLock.Unlock()
	if path != "/" {
		path = strings.TrimSuffix(path, "/")
	}
	mono.endpoints[path] = handler
}

func (mono *Monotonic) buildHandler(handler HandlerFunc) http.HandlerFunc {
	result := handler
	for _, middleware := range mono.middlewareFuncs {
		result = middleware(result)
	}
	return func(rw http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(mono.baseContext, mono.timeout)
		defer cancel()
		if err := result(ctx, rw, req); err != nil {
			mono.onError(err)
		}
	}
}

func (mono *Monotonic) build() error {
	mono.buildLock.Lock()
	defer mono.buildLock.Unlock()
	if mono.port == 0 {
		if mono.tls {
			mono.port = 443
		} else {
			mono.port = Port.Add(1) - 1
		}
	}
	if mono.tlsCacheDir == "" {
		mono.tlsCacheDir = filepath.Join(os.TempDir(), "monotonic")
	}
	for endpoint, handler := range mono.endpoints {
		mono.server.HandleFunc(endpoint, mono.buildHandler(handler))
	}
	return nil
}

func (mono *Monotonic) listenAndServeTLS(addr string) error {
	if len(mono.tlsDomains) == 0 {
		return fmt.Errorf("no domains specified, no tls")
	}
	certManager := &autocert.Manager{
		Cache:      autocert.DirCache(mono.tlsCacheDir),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domainsWithWWW(mono.tlsDomains)...),
		Email:      mono.tlsEmail,
	}
	server := &http.Server{
		Addr:    addr,
		Handler: mono.server,
		TLSConfig: &tls.Config{
			GetCertificate: certManager.GetCertificate,
			NextProtos:     []string{"h2", "http/1.1", "acme-tls/1"},
			MinVersion:     tls.VersionTLS13,
			ServerName:     mono.tlsDomains[0],
		},
	}
	return server.ListenAndServeTLS("", "")
}

func domainsWithWWW(domains []string) []string {
	result := []string{}
	for _, domain := range domains {
		if !strings.HasPrefix(domain, "www.") {
			result = append(result, "www."+domain)
		}
		result = append(result, domain)
	}
	return result
}
