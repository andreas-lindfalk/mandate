// Package authlimit limits failed sign-ins and token checks per client
// address, in one server's memory. Tokens are 32 random bytes, so guessing
// one is not the risk; the load of checking a stream of bad ones is.
//
// A failure is an answer of 401 to a request that carried a credential (a
// bearer token or the session cookie), or a failed sign-in callback that
// carried the sign-in flow's cookie. A request without either is not a
// failure: a page that is signed out asks whether it is signed in, and a
// page elsewhere can make a browser fetch the callback without the flow's
// cookie, which must not lock anyone out. Past the limit, every request
// from that address to a place that checks credentials is answered 429
// until the window ends, a valid token's included, so the limit cannot
// tell which tokens are valid.
//
// gRPC and gRPC-Web are refused on those places: they report a refusal as
// HTTP 200, which would go uncounted. pgmandate's clients speak Connect.
package authlimit

import (
	"bufio"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// Failures from one address in one window.
	DefaultMax    = 20
	DefaultWindow = time.Minute
	// maxEntries bounds the addresses tracked at once. Past it a new
	// address is not tracked until a sweep, at most once a window, frees
	// room: a flood from many addresses cannot grow the map or slow every
	// check down.
	maxEntries = 65536
)

// Options name what the limiter reads from a request.
type Options struct {
	// Header names the header a trusted reverse proxy writes the client's
	// address in; empty uses the connection's address.
	Header string
	// SessionCookie counts as a credential.
	SessionCookie string
	// FlowCookie marks a sign-in callback a browser started; only such a
	// callback counts when it fails.
	FlowCookie string
}

type Limiter struct {
	mu        sync.Mutex
	max       int
	window    time.Duration
	options   Options
	entries   map[string]*entry
	lastSweep time.Time
	capacity  int
	now       func() time.Time
}

type entry struct {
	start    time.Time
	failures int
}

// New limits DefaultMax failures per DefaultWindow per client address. The
// address is the connection's, or, with Options.Header, the last entry of
// that header's last line, as a trusted reverse proxy writes it
// (X-Forwarded-For). IPv6 addresses count by their /64, which one client
// usually holds whole.
func New(options Options) *Limiter {
	return &Limiter{max: DefaultMax, window: DefaultWindow, options: options, entries: map[string]*entry{}, capacity: maxEntries, now: time.Now}
}

// guarded says whether a path checks credentials: the people's API, the
// agent contract, MCP and sign-in.
func guarded(path string) bool {
	return strings.HasPrefix(path, "/mandate.") || path == "/mcp" || strings.HasPrefix(path, "/mcp/") || strings.HasPrefix(path, "/auth/")
}

// Handler answers 429 to a limited address and counts the failures of the
// rest.
func (l *Limiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !guarded(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if grpc(r) {
			http.Error(w, "gRPC and gRPC-Web are not served here; use the Connect protocol, JSON or protobuf over HTTP", http.StatusUnsupportedMediaType)
			return
		}
		address := l.ClientAddress(r)
		if wait, limited := l.limited(address); limited {
			refuse(w, r, wait)
			return
		}
		recorder := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		if l.failed(r, recorder.status) {
			l.fail(address)
		}
	})
}

// grpc says the request speaks gRPC or gRPC-Web.
func grpc(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/grpc")
}

func (l *Limiter) failed(r *http.Request, status int) bool {
	if strings.HasPrefix(r.URL.Path, "/auth/oidc/callback") {
		return (status == http.StatusBadRequest || status == http.StatusUnauthorized) && l.hasCookie(r, l.options.FlowCookie)
	}
	if status != http.StatusUnauthorized {
		return false
	}
	return r.Header.Get("Authorization") != "" || l.hasCookie(r, l.options.SessionCookie)
}

func (l *Limiter) hasCookie(r *http.Request, name string) bool {
	if name == "" {
		return false
	}
	_, err := r.Cookie(name)
	return err == nil
}

// ClientAddress is the address the limit counts by.
func (l *Limiter) ClientAddress(r *http.Request) string {
	address := ""
	if l.options.Header != "" {
		// The last line, and its last entry: what the proxy wrote, whether it
		// appended to the client's line or added a line of its own.
		if lines := r.Header.Values(l.options.Header); len(lines) > 0 {
			parts := strings.Split(lines[len(lines)-1], ",")
			address = strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if address == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		address = host
	}
	if ip := net.ParseIP(address); ip != nil && ip.To4() == nil {
		return (&net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
	}
	return address
}

func (l *Limiter) limited(address string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[address]
	if !ok {
		return 0, false
	}
	left := e.start.Add(l.window).Sub(l.now())
	if left <= 0 {
		delete(l.entries, address)
		return 0, false
	}
	return left, e.failures >= l.max
}

func (l *Limiter) fail(address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[address]
	if !ok && len(l.entries) >= l.capacity && now.Sub(l.lastSweep) >= l.window {
		l.lastSweep = now
		for key, old := range l.entries {
			if now.Sub(old.start) >= l.window {
				delete(l.entries, key)
			}
		}
	}
	if !ok && len(l.entries) >= l.capacity {
		return
	}
	if !ok || now.Sub(e.start) >= l.window {
		e = &entry{start: now}
		l.entries[address] = e
	}
	e.failures++
}

// refuse answers a limited request, in the Connect protocol's own form for
// its paths so that a client reads resource_exhausted.
func refuse(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	seconds := int(math.Ceil(wait.Seconds()))
	w.Header().Set("Retry-After", fmt.Sprint(seconds))
	message := fmt.Sprintf("too many failed sign-ins or token checks from this address; try again in %d seconds", seconds)
	if strings.HasPrefix(r.URL.Path, "/mandate.") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w, `{"code":"resource_exhausted","message":%q}`, message)
		return
	}
	http.Error(w, message, http.StatusTooManyRequests)
}

// recorder remembers the status a handler answered with, and passes on
// what streaming needs.
type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *recorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
