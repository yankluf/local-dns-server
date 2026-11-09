// Package server exposes the JSON API and the web UI.
//
// Protections, outermost first:
//   - security headers on every response (CSP, no framing, no sniffing)
//   - Host header allowlist, which defeats DNS rebinding
//   - for changes: same-origin check and a JSON-only body, which defeat CSRF
//   - bearer token. After too many wrong tokens an address is slowed to one
//     try per second, so guessing is hopeless but the right token still gets
//     in (someone sharing the address can't lock the admin out)
//   - a global rate limit on changes
//
// Every change is written to the log with the address it came from.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yankluf/local-dns-server/internal/dnsmasq"
	"github.com/yankluf/local-dns-server/internal/docker"
	"github.com/yankluf/local-dns-server/internal/names"
	"github.com/yankluf/local-dns-server/internal/settings"
	"github.com/yankluf/local-dns-server/internal/store"
)

//go:embed ui
var uiFiles embed.FS

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Server serves the API and UI.
type Server struct {
	Store        *store.Store
	DNSmasq      *dnsmasq.Manager
	Docker       *docker.Watcher   // nil when discovery is off
	Replica      func() any        // nil unless this is a replica
	Settings     *settings.Manager // current config and dashboard changes
	Token        string            // empty only with LDS_INSECURE_NO_AUTH
	PublicRead   bool
	AllowedHosts []string
	TLS          bool
	ReadOnly     bool

	failures failureLimiter
	writes   *bucket
	initOnce sync.Once

	healthMu  sync.Mutex
	healthAt  time.Time
	healthErr error
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	s.initOnce.Do(func() {
		s.failures = failureLimiter{max: 10, window: 5 * time.Minute, hits: map[string]*failure{}}
		s.writes = newBucket(20, 5) // burst of 20 changes, then 5 per second
	})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("GET /api/records", s.read(s.list))
	mux.HandleFunc("POST /api/records", s.write(s.add))
	mux.HandleFunc("DELETE /api/records/{key}", s.write(s.remove))
	mux.HandleFunc("GET /api/check", s.read(s.check))
	mux.HandleFunc("GET /api/status", s.read(s.statusHandler))
	mux.HandleFunc("GET /api/settings", s.read(s.getSettings))
	mux.HandleFunc("PUT /api/settings", s.write(s.putSettings))
	mux.HandleFunc("GET /healthz", s.healthz)
	ui, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))
	return s.headers(s.hostCheck(mux))
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.TLS {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// hostCheck only serves requests addressed to this server by IP, localhost,
// a name inside our zones, or a name in LDS_ALLOWED_HOSTS. A DNS rebinding
// attack arrives with the attacker's own domain in Host and is refused.
func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeError(w, http.StatusMisdirectedRequest, errors.New("unknown Host; add it to LDS_ALLOWED_HOSTS"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil || host == "localhost" {
		return true
	}
	if names.InZones(host, s.Store.Zones()) {
		return true
	}
	for _, a := range s.AllowedHosts {
		if host == a {
			return true
		}
	}
	return false
}

// read guards endpoints that return records or status.
func (s *Server) read(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.PublicRead && !s.authorized(w, r) {
			return
		}
		next(w, r)
	}
}

// write guards endpoints that change records.
func (s *Server) write(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, errors.New("cross-site request refused"))
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, errors.New("send the body as Content-Type: application/json"))
				return
			}
		}
		if !s.authorized(w, r) {
			return
		}
		if s.ReadOnly {
			writeError(w, http.StatusConflict, store.ErrReadOnly)
			return
		}
		if !s.writes.take() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, errors.New("too many changes; slow down"))
			return
		}
		next(w, r)
	}
}

// sameOrigin rejects requests a browser marks as coming from another site.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser cross-origin request (CLI, curl)
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.Token == "" {
		return true // LDS_INSECURE_NO_AUTH
	}
	ip := clientIP(r)
	header := r.Header.Get("Authorization")
	if header != "" && !s.failures.allow(ip) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, errors.New("too many wrong tokens from this address; slow down"))
		return false
	}
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
		if header != "" && s.failures.add(ip) {
			log.Printf("audit: %s sent too many wrong tokens; its tries are now limited to one per second", ip)
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="lds"`)
		writeError(w, http.StatusUnauthorized, errors.New("missing or wrong token"))
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// info tells the web UI what it needs before signing in. No secrets.
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required":       s.Token != "",
		"read_requires_token": s.Token != "" && !s.PublicRead,
		"read_only":           s.ReadOnly,
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"records": s.Store.List()})
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Comment string `json:"comment"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("body must be one JSON object with name, value and optional comment"))
		return
	}
	rec, err := store.New(in.Name, in.Value, in.Comment, s.Store.Zones())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rec, err = s.Store.Add(rec)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	log.Printf("audit: %s added %s %s -> %s", clientIP(r), rec.Type, rec.Name, rec.Value)
	// Return the view so the caller sees any warning about the new record.
	for _, v := range s.Store.List() {
		if v.ID == rec.ID {
			writeJSON(w, http.StatusCreated, v)
			return
		}
	}
	writeJSON(w, http.StatusCreated, store.View{Record: rec, Active: true})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	removed, err := s.Store.Remove(r.PathValue("key"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	for _, rec := range removed {
		log.Printf("audit: %s removed %s %s -> %s", clientIP(r), rec.Type, rec.Name, rec.Value)
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// check reports what dnsmasq answers for a name and which records decide it.
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	name := names.Normalize(r.URL.Query().Get("name"))
	if err := names.ValidHostname(name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	matched, inZone := s.Store.Explain(name)
	out := map[string]any{"name": name, "in_zone": inZone, "answers": []string{}, "records": []store.Record{}}
	if matched != nil {
		out["records"] = matched
	}
	if !inZone {
		// Not ours: looking it up would turn this endpoint into a way to
		// send queries to the upstream servers.
		out["note"] = "outside this server's domains, so it is passed on to the upstream DNS servers"
		writeJSON(w, http.StatusOK, out)
		return
	}
	if s.DNSmasq != nil {
		answers, err := s.DNSmasq.Lookup(r.Context(), name)
		switch {
		case err != nil:
			out["error"] = err.Error()
		case len(answers) > 0:
			out["answers"] = answers
		}
	}
	if len(matched) == 0 {
		out["note"] = "no record matches this name"
	}
	writeJSON(w, http.StatusOK, out)
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, store.ErrExists), errors.Is(err, store.ErrReadOnly), errors.Is(err, store.ErrFull):
		return http.StatusConflict
	case errors.Is(err, store.ErrInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusBadRequest
	}
}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"zones":     s.Store.Zones(),
		"docker":    docker.Status{},
		"read_only": s.ReadOnly,
	}
	if s.DNSmasq != nil {
		out["dnsmasq"] = s.DNSmasq.Status()
	}
	if s.Docker != nil {
		out["docker"] = s.Docker.Status()
	}
	if s.Replica != nil {
		out["replica"] = s.Replica()
	}
	if s.Settings != nil {
		out["upstreams"] = s.Settings.Current().DNSmasq.Upstreams
		if e := s.Settings.LoadError(); e != "" {
			out["settings_error"] = e
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		writeError(w, http.StatusNotFound, errors.New("settings aren't available"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fields": s.Settings.Fields(), "error": s.Settings.LoadError()})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	if s.Settings == nil {
		writeError(w, http.StatusNotFound, errors.New("settings aren't available"))
		return
	}
	var ch settings.Change
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ch); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New(`body must be one JSON object: {"set": {...}, "reset": [...], "dry_run": false}`))
		return
	}
	res, err := s.Settings.Update(ch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !ch.DryRun {
		log.Printf("audit: %s changed settings: %s", clientIP(r), names.CleanText(strings.Join(ch.Keys(), ", "), 500))
	}
	writeJSON(w, http.StatusOK, res)
}

// healthz answers "ok" only if dnsmasq actually answers a DNS query. The
// result is reused for a few seconds, so hammering this open endpoint can't
// flood dnsmasq with test queries.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if s.DNSmasq != nil {
		s.healthMu.Lock()
		if time.Since(s.healthAt) > 3*time.Second {
			s.healthErr = s.DNSmasq.Probe(context.WithoutCancel(r.Context()))
			s.healthAt = time.Now()
		}
		err := s.healthErr
		s.healthMu.Unlock()
		if err != nil {
			http.Error(w, "dns not answering", http.StatusServiceUnavailable)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": names.CleanText(err.Error(), 500)})
}

// failureLimiter slows down a client IP after too many wrong tokens.
type failureLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*failure
}

type failure struct {
	count int
	start time.Time
	last  time.Time // last try let through while slowed down
}

// allow reports whether a token from ip may be checked now. An address over
// the limit gets one try per second instead of none, so the real admin
// (who may share the address with whoever is guessing) still gets in.
func (l *failureLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.hits[ip]
	if !ok {
		return true
	}
	now := time.Now()
	if now.Sub(f.start) > l.window {
		delete(l.hits, ip)
		return true
	}
	if f.count < l.max {
		return true
	}
	if now.Sub(f.last) < time.Second {
		return false
	}
	f.last = now
	return true
}

// add counts a wrong token and reports whether ip just reached the limit.
func (l *failureLimiter) add(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 { // keep memory bounded
		for k, f := range l.hits {
			if time.Since(f.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	f, ok := l.hits[ip]
	if !ok && len(l.hits) > 10000 {
		return false // still full of recent offenders; the token length makes guessing hopeless anyway
	}
	if !ok || time.Since(f.start) > l.window {
		f = &failure{start: time.Now()}
		l.hits[ip] = f
	}
	f.count++
	return f.count == l.max
}

// bucket is a token-bucket rate limiter.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // per second
	last   time.Time
}

func newBucket(burst, perSecond float64) *bucket {
	return &bucket{tokens: burst, max: burst, rate: perSecond, last: time.Now()}
}

func (b *bucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.max, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
