package webapp

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// installStack builds the router and the middleware chain.
//
// The order is fixed and meaningful:
//
//	tracker         records whether a response began, first for that reason
//	RequestID       correlation ID, so everything after can log it
//	client IP       the address to attribute the request to
//	Recoverer       panic to 500, before any handler can fail
//	request logger  one line per request, sees status and byte count
//	body limit      reject oversized state-changing bodies
//	Compress        content negotiation
//	Heartbeat       liveness, answered without routing or the database
//	SecureHeaders   response headers, set on the way out
//
// Authentication, CSRF, and profile middleware join this chain in the
// authentication milestone, after SecureHeaders and before the router, so
// that public surfaces can be declared by structure rather than by
// convention inside handlers.
//
// The chain wraps the router rather than being installed on it with
// chi's Use. Two reasons, both learned the hard way. chi's Use panics once
// a route exists, which makes a middleware stack that any component can
// extend impossible to assemble in any order. And a mux with no routes
// short-circuits to its not-found handler without running any of its
// middleware, so a completely configured application with nothing mounted
// would serve 404s with no correlation ID, no logging, no security
// headers, and a live liveness endpoint silently missing.
//
// The tracker goes outside everything, including RequestID, because the
// error path needs to know whether a response had already begun, and a
// writer installed inside the chain is invisible to middleware further
// out.
func (a *App) installStack() {
	mux := chi.NewRouter()
	mux.NotFound(a.notFound)
	mux.MethodNotAllowed(a.methodNotAllowed)
	a.mux = mux

	a.rebuildChain()
}

// chain assembles the middleware chain around the router, including anything
// appended with Use.
//
// It is a method rather than a one-time assignment because the chain has to be
// rebuildable: authentication middleware is installed after construction, once
// a database handle exists, and chi's Use is unavailable for the reasons above.
// Rebuilding is cheap — it is a closure composition, done once per install, not
// per request — and it means the chain's order is stated in exactly one place.
func (a *App) chain() http.Handler {
	entries := append(coreChainEntries(a), a.extensions...)
	return handlerChain(entries, a.mux)
}

// rebuildChain recomputes Handler from the built-in chain plus whatever has
// been appended with Use.
func (a *App) rebuildChain() {
	a.Handler = a.chain()
}

// coreChainEntries returns the built-in half of the chain, outermost first.
func coreChainEntries(a *App) []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{
		a.trackWrites,
		requestID,
		a.clientIP,
		a.recoverer,
		a.requestLogger,
		a.limitBody,
		middleware.Compress(5),
		middleware.Heartbeat(HeartbeatPath),
		a.secureHeaders,
	}
}

// handlerChain composes middleware around a final handler, applying the
// first entry outermost.
func handlerChain(middleware []func(http.Handler) http.Handler, final http.Handler) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		final = middleware[i](final)
	}
	return final
}

// limitBody bounds the size of a request body.
//
// Only state-changing methods are limited. A GET has no body in any
// application's intended use, and wrapping it would defeat the response
// cache of anything sitting in front for requests that carry a large URL
// and no body.
//
// Without a limit, a client can make the server buffer an arbitrarily
// large form simply by posting it, and the cost is paid per concurrent
// request rather than per request that is actually interesting.
func (a *App) limitBody(next http.Handler) http.Handler {
	max := a.Config.MaxBodyBytes
	if max == 0 {
		max = DefaultMaxBodyBytes
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			// Passing the writer lets net/http close the connection once
			// the limit is exceeded instead of reading the rest of a body
			// nobody wants.
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// DefaultMaxBodyBytes bounds a state-changing request body. The number is
// generous for a text-heavy form and small enough that a hostile client
// cannot make the server buffer hundreds of megabytes per request.
// Applications that accept uploads set MaxBodyBytes for the route that
// needs it.
const DefaultMaxBodyBytes int64 = 1 << 20

// errMethodNotAllowed distinguishes "this path exists but not for that
// verb" from "nothing matches", which is the difference between a client
// bug worth seeing in logs and a scanner.
var errMethodNotAllowed = errors.New("method not allowed")

// notFound renders the not-found page through the normal error path, so
// the status is a real 404 rather than the 200 that a hand-rolled error
// template so often ends up returning.
func (a *App) notFound(w http.ResponseWriter, r *http.Request) {
	a.Fail(w, r, http.StatusNotFound, ErrNotFound)
}

// methodNotAllowed renders 405 and sets Allow.
//
// "405 with no Allow" is a puzzle rather than an answer: a client that
// posted to an endpoint expecting a different verb has to guess. chi
// records the allowed methods internally but exposes no accessor, so the
// application's own route table is consulted, keyed by pattern and matched
// against the concrete path, because a 405 means nothing matched exactly
// and the matched pattern is therefore unavailable from the request.
func (a *App) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	if allowed := a.allowedMethods(r.URL.Path); len(allowed) > 0 {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
	}
	a.Fail(w, r, http.StatusMethodNotAllowed, errMethodNotAllowed)
}

// allowedMethods returns, sorted and de-duplicated, the methods registered
// for routes whose pattern matches path.
func (a *App) allowedMethods(path string) []string {
	table := a.buildMethodTable()

	seen := map[string]struct{}{}
	for pattern, methods := range table {
		if !patternMatches(pattern, path) {
			continue
		}
		for _, m := range methods {
			seen[m] = struct{}{}
		}
	}
	// A route registered for GET also answers HEAD, which is what lets a
	// client check existence without transferring the body.
	if _, ok := seen[http.MethodGet]; ok {
		seen[http.MethodHead] = struct{}{}
	}

	allowed := make([]string, 0, len(seen))
	for m := range seen {
		allowed = append(allowed, m)
	}
	sort.Strings(allowed)
	return allowed
}

// buildMethodTable snapshots the registered routes as pattern -> methods.
//
// It runs on a 405, which is a rare and usually automated outcome; 404s,
// which scanners produce instead, never reach here because nothing matches
// a pattern. Caching it would mean tracking every route registration,
// including on router handles an application kept hold of, and that
// bookkeeping is more likely to go stale than the walk is likely to be
// hot.
func (a *App) buildMethodTable() map[string][]string {
	table := map[string][]string{}
	err := chi.Walk(a.mux, func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
		table[route] = append(table[route], method)
		return nil
	})
	if err != nil {
		a.log().ErrorContext(context.Background(), "could not read the route table for Allow headers", "error", err)
	}
	return table
}

// patternMatches reports whether a chi pattern covers a concrete path.
// It exists because chi records the allowed methods for a request but
// exposes no accessor for them, so the route table has to be consulted by
// pattern and matched against the path that actually arrived.
//
// The rules mirror chi's routing tree rather than being invented: a
// literal segment must match exactly, a placeholder matches exactly one
// non-empty segment, and a trailing wildcard matches the remainder but
// still requires the separator before it, which is why /assets/* covers
// /assets/ and /assets/a but not /assets.
//
// One asymmetry is chi's own: a route mounted as "/" inside a sub-router is
// walked as "/things/" but answers both /things and /things/. A pattern's
// final separator is therefore optional, because the walk cannot tell that
// route apart from one registered with an explicit trailing slash, and a
// 405 with no Allow is the worse error than an Allow that is generous by one
// slash.
func patternMatches(pattern, path string) bool {
	patSegs := splitPath(pattern)
	pathSegs := splitPath(path)

	for i, seg := range patSegs {
		switch {
		case strings.HasPrefix(seg, "*"):
			// The wildcard swallows everything left over, but only if the
			// path actually has a segment position for it.
			return len(pathSegs) >= len(patSegs)
		case i >= len(pathSegs):
			return len(patSegs)-i == 1 && seg == ""
		case strings.HasPrefix(seg, "{"):
			if pathSegs[i] == "" {
				return false
			}
		case seg != pathSegs[i]:
			return false
		}
	}
	return len(patSegs) == len(pathSegs)
}

// splitPath splits a path into segments on "/" keeping empty ones, because
// the trailing separator is significant to chi: /things and /things/ are
// different paths, and only the mounted form of a route answers both.
func splitPath(path string) []string {
	return strings.Split(path, "/")
}

// trackWrites wraps the response writer so that the error path can tell
// "nothing sent yet" from "a body is already on its way".
//
// The standard library exposes no way to ask a ResponseWriter whether it
// has written, and the distinction matters: calling WriteHeader after a
// body started produces a truncated response plus a confusing net/http
// log line, which is worse than leaving the stream cut short.
func (a *App) trackWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&trackingWriter{ResponseWriter: w}, r)
	})
}

// trackingWriter records the status code the moment one is written.
type trackingWriter struct {
	http.ResponseWriter
	status int
}

func (t *trackingWriter) WriteHeader(status int) {
	if t.status == 0 {
		t.status = status
	}
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	return t.ResponseWriter.Write(p)
}

// Unwrap lets middleware further in the chain, and net/http's own
// flushing and hijacking helpers, reach the writer underneath. Without
// this, wrapping the outermost writer would break streaming responses
// and websockets.
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// Flush forwards to the underlying writer when it supports flushing.
func (t *trackingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer when it supports hijacking.
func (t *trackingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errNotHijackable
	}
	return h.Hijack()
}

// errNotHijackable is reported when nothing beneath the wrapper supports
// hijacking.
var errNotHijackable = errors.New("webapp: response writer does not support hijacking")

// started reports whether a response has already begun, by walking down
// the chain of wrapping writers to the tracker. The walk is necessary
// because the compression and status-recording middlewares wrap the
// tracker rather than replacing it.
//
// When no tracker is found the answer is "not started": that leaves the
// caller to attempt an honest error response, which net/http degrades
// with a log line rather than corrupting what the client receives.
func started(w http.ResponseWriter) bool {
	for {
		switch t := w.(type) {
		case *trackingWriter:
			return t.status != 0
		case interface{ Unwrap() http.ResponseWriter }:
			w = t.Unwrap()
			if w == nil {
				return false
			}
		default:
			return false
		}
	}
}

// clientIP determines the address of the client that made the request.
//
// chi's RealIP middleware is not used, and not merely because it is
// deprecated: it takes the leftmost X-Forwarded-For entry, or
// True-Client-IP / X-Real-IP, unconditionally. Leading XFF entries are
// chosen by the client, so any request can claim any address, and the
// result is exactly what rate limiters, audit logs, and IP-based
// blocklists are written against.
//
// The default therefore trusts nothing but the connection. An application
// behind a proxy names the header its proxy overwrites, or enumerates the
// proxy networks or hop count, and only then is a forwarded header read.
// The configuration is validated at startup, so a typo in a CIDR is a boot
// error rather than a silently ignored trust setting.
//
// Trusted proxy CIDRs are a gate, not merely a skip list. chi's own
// XFF middleware reads X-Forwarded-For whenever it is configured with any
// prefix at all, and leaves connection-source checks to a firewall. That is
// a defensible choice for an application whose operator knows the topology,
// and the wrong default for a framework whose operator does not: with the
// gate, a request that did not arrive over a trusted connection cannot
// influence its own recorded address no matter what the deployment looks
// like, and moving the server off the network boundary becomes a
// configuration change rather than a silent security regression.
func (a *App) clientIP(next http.Handler) http.Handler {
	prefixes := trustedPrefixes(a.Config.TrustedProxyCIDRs)

	var forwarded func(http.Handler) http.Handler
	switch {
	case a.Config.ClientIPHeader != "":
		forwarded = middleware.ClientIPFromHeader(a.Config.ClientIPHeader)
	case a.Config.TrustedProxyCount > 0:
		forwarded = middleware.ClientIPFromXFFTrustedProxies(a.Config.TrustedProxyCount)
	case len(prefixes) > 0:
		forwarded = middleware.ClientIPFromXFF(a.Config.TrustedProxyCIDRs...)
	}
	fromConn := middleware.ClientIPFromRemoteAddr

	// No trust configured means the connection is the only source, which
	// is the common case and worth a branch of its own: wrapping the
	// handler twice for nothing is how a hot path gets slow.
	if forwarded == nil {
		return fromConn(next)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(prefixes) > 0 && !addrInPrefixes(connAddr(r), prefixes) {
			fromConn(next).ServeHTTP(w, r)
			return
		}
		forwarded(next).ServeHTTP(w, r)
	})
}

// trustedPrefixes parses the configured proxy networks. Invalid entries are
// skipped rather than fatal here because Config.Validate already rejects
// them at startup; this keeps a hand-mutated Config from taking a request
// down.
func trustedPrefixes(cidrs []string) []netip.Prefix {
	if len(cidrs) == 0 {
		return nil
	}
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		if p, err := netip.ParsePrefix(cidr); err == nil {
			prefixes = append(prefixes, p)
		}
	}
	return prefixes
}

// connAddr is the address of whoever opened the connection, normalized so
// that an IPv4 client on a dual-stack listener compares the same way a v4
// prefix does.
func connAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ip.Unmap()
}

// addrInPrefixes reports whether ip falls inside any prefix. An invalid ip
// is inside none of them, which is the fail-closed answer: an address that
// could not be determined is not evidence of a trusted proxy.
func addrInPrefixes(ip netip.Addr, prefixes []netip.Prefix) bool {
	if !ip.IsValid() {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP returns the client address as determined by the framework's
// configuration, or the empty string when it could not be determined.
func ClientIP(r *http.Request) string {
	return middleware.GetClientIP(r.Context())
}

// clientAddr is the address to attribute a log line to, falling back to
// the connection address when no client IP could be determined.
func clientAddr(r *http.Request) string {
	if ip := ClientIP(r); ip != "" {
		return ip
	}
	return r.RemoteAddr
}
