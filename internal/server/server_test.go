package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yankluf/local-dns-server/internal/config"
	"github.com/yankluf/local-dns-server/internal/settings"
	"github.com/yankluf/local-dns-server/internal/store"
)

const token = "0123456789abcdef0123"

func newServer(t *testing.T, mod func(*Server)) http.Handler {
	t.Helper()
	t.Setenv("LDS_TOKEN", token)
	t.Setenv("LDS_ZONES", "home.lan")
	zones := []string{"home.lan"}
	st, err := store.Open(filepath.Join(t.TempDir(), "r.json"), store.Options{Zones: zones})
	if err != nil {
		t.Fatal(err)
	}
	base, err := config.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	sm := settings.Load(filepath.Join(t.TempDir(), "settings.json"), base)
	sm.Apply = func(c config.Config) { st.SetRules(c.Zones, c.MaxRecords) }
	sm.Records = st
	s := &Server{Store: st, Token: token, Settings: sm}
	if mod != nil {
		mod(s)
	}
	return s.Handler()
}

type req struct {
	method, path, body string
	headers            map[string]string
}

func do(h http.Handler, r req) *httptest.ResponseRecorder {
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	hr.Host = "192.168.1.5:5380"
	hr.RemoteAddr = "192.168.1.77:40000"
	for k, v := range r.headers {
		if k == "Host" {
			hr.Host = v
			continue
		}
		hr.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, hr)
	return w
}

func authJSON(extra map[string]string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func TestAddWorks(t *testing.T) {
	h := newServer(t, nil)
	w := do(h, req{"POST", "/api/records", `{"name":"nas.home.lan","value":"10.0.0.10"}`, authJSON(nil)})
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	w = do(h, req{"GET", "/api/records", "", map[string]string{"Authorization": "Bearer " + token}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "nas.home.lan") {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
}

// Finding 2: CSRF. A web page can only send "simple" cross-site requests
// (text/plain bodies, no custom headers) without a preflight.
func TestCSRFBlocked(t *testing.T) {
	h := newServer(t, func(s *Server) { s.Token = ""; s.PublicRead = true }) // even with auth off
	cases := map[string]req{
		"text/plain body":  {"POST", "/api/records", `{"name":"a.home.lan","value":"1.2.3.4"}`, map[string]string{"Content-Type": "text/plain"}},
		"form body":        {"POST", "/api/records", `{"name":"a.home.lan","value":"1.2.3.4"}`, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		"foreign Origin":   {"POST", "/api/records", `{"name":"a.home.lan","value":"1.2.3.4"}`, map[string]string{"Content-Type": "application/json", "Origin": "https://evil.example"}},
		"cross-site fetch": {"POST", "/api/records", `{"name":"a.home.lan","value":"1.2.3.4"}`, map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}},
		"foreign DELETE":   {"DELETE", "/api/records/a.home.lan", "", map[string]string{"Origin": "https://evil.example"}},
	}
	for name, r := range cases {
		if w := do(h, r); w.Code < 400 {
			t.Errorf("%s: got %d", name, w.Code)
		}
	}
	ok := do(h, req{"POST", "/api/records", `{"name":"a.home.lan","value":"1.2.3.4"}`, map[string]string{"Content-Type": "application/json", "Origin": "http://192.168.1.5:5380"}})
	if ok.Code != http.StatusCreated {
		t.Errorf("same-origin request refused: %d %s", ok.Code, ok.Body)
	}
}

// Finding 2/11: DNS rebinding arrives with the attacker's domain in Host.
func TestRebindingBlocked(t *testing.T) {
	h := newServer(t, func(s *Server) { s.AllowedHosts = []string{"dns.example.org"} })
	for host, want := range map[string]int{
		"attacker.example:5380": http.StatusMisdirectedRequest,
		"192.168.1.5:5380":      http.StatusOK,
		"[fd00::5]:5380":        http.StatusOK,
		"localhost:5380":        http.StatusOK,
		"dns.home.lan":          http.StatusOK,
		"dns.example.org":       http.StatusOK,
		"home.lan.evil.com":     http.StatusMisdirectedRequest,
	} {
		w := do(h, req{"GET", "/api/records", "", map[string]string{"Host": host, "Authorization": "Bearer " + token}})
		if w.Code != want {
			t.Errorf("Host %s: got %d, want %d", host, w.Code, want)
		}
	}
	if w := do(h, req{"GET", "/", "", map[string]string{"Host": "attacker.example"}}); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("web page served to a rebinding Host: %d", w.Code)
	}
}

// Finding 2/11: everything except /api/info and /healthz needs the token.
func TestTokenRequired(t *testing.T) {
	h := newServer(t, nil)
	for _, r := range []req{
		{"GET", "/api/records", "", nil},
		{"GET", "/api/status", "", nil},
		{"POST", "/api/records", `{"name":"a.home.lan","value":"1.2.3.4"}`, map[string]string{"Content-Type": "application/json"}},
		{"DELETE", "/api/records/a.home.lan", "", nil},
		{"GET", "/api/records", "", map[string]string{"Authorization": "Bearer wrong"}},
		{"GET", "/api/records", "", map[string]string{"Authorization": token}}, // missing "Bearer"
	} {
		if w := do(h, r); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s %v: got %d", r.method, r.path, r.headers, w.Code)
		}
	}
	if w := do(h, req{"GET", "/api/info", "", nil}); w.Code != 200 || strings.Contains(w.Body.String(), token) {
		t.Errorf("/api/info: %d %s", w.Code, w.Body)
	}
}

// Brute force: after 10 wrong tokens an address gets one try per second.
// The right token still gets in, so a guesser sharing the admin's address
// can't lock the admin out.
func TestTokenGuessingLimited(t *testing.T) {
	h := newServer(t, nil)
	guess := map[string]string{"Authorization": "Bearer guess"}
	for i := 0; i < 10; i++ {
		if w := do(h, req{"GET", "/api/records", "", guess}); w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: got %d", i, w.Code)
		}
	}
	if w := do(h, req{"GET", "/api/records", "", map[string]string{"Authorization": "Bearer " + token}}); w.Code != http.StatusOK {
		t.Errorf("right token after 10 failures: got %d", w.Code)
	}
	for i := 0; i < 5; i++ {
		if w := do(h, req{"GET", "/api/records", "", guess}); w.Code != http.StatusTooManyRequests {
			t.Errorf("guess inside the same second was checked: got %d", w.Code)
		}
	}
}

// One name can hold an IPv4 and an IPv6 address; removing by name removes both.
func TestDualStackAndRemove(t *testing.T) {
	h := newServer(t, nil)
	for body, want := range map[string]int{
		`{"name":"nas.home.lan","value":"10.0.0.10"}`: http.StatusCreated,
		`{"name":"nas.home.lan","value":"fd00::10"}`:  http.StatusCreated,
	} {
		if w := do(h, req{"POST", "/api/records", body, authJSON(nil)}); w.Code != want {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	for _, body := range []string{
		`{"name":"nas.home.lan","value":"10.0.0.11"}`,
		`{"name":"nas.home.lan","value":"other.home.lan"}`,
	} {
		if w := do(h, req{"POST", "/api/records", body, authJSON(nil)}); w.Code != http.StatusConflict {
			t.Errorf("%s: got %d", body, w.Code)
		}
	}
	w := do(h, req{"DELETE", "/api/records/nas.home.lan", "", authJSON(nil)})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "10.0.0.10") || !strings.Contains(w.Body.String(), "fd00::10") {
		t.Errorf("remove: %d %s", w.Code, w.Body)
	}
}

func TestCheck(t *testing.T) {
	h := newServer(t, nil)
	auth := map[string]string{"Authorization": "Bearer " + token}
	for _, body := range []string{
		`{"name":"*.home.lan","value":"10.0.0.5"}`,
		`{"name":"nas.home.lan","value":"10.0.0.10"}`,
		`{"name":"files.home.lan","value":"nas.home.lan"}`,
	} {
		do(h, req{"POST", "/api/records", body, authJSON(nil)})
	}
	for path, want := range map[string]string{
		"/api/check?name=files.home.lan":  `"value": "10.0.0.10"`,
		"/api/check?name=Other.home.lan.": `"name": "*.home.lan"`,
		"/api/check?name=example.com":     `"in_zone": false`,
	} {
		if w := do(h, req{"GET", path, "", auth}); w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if w := do(h, req{"GET", "/api/check?name=bad%0Aname", "", auth}); w.Code != http.StatusBadRequest {
		t.Errorf("bad name: %d", w.Code)
	}
	if w := do(h, req{"GET", "/api/check?name=nas.home.lan", "", nil}); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
}

// Findings 1 and 3 through the API.
func TestInjectionAndPublicDomainsRejected(t *testing.T) {
	h := newServer(t, nil)
	for _, body := range []string{
		`{"name":"x.home.lan","value":"fe80::1%z\naddress=/#/6.6.6.6"}`,
		`{"name":"*.com","value":"6.6.6.7"}`,
		`{"name":"login.microsoftonline.com","value":"6.6.6.7"}`,
		`{"name":"a.home.lan","value":"1.2.3.4","extra":"field"}`,
		`{"name":"a.home.lan","value":"1.2.3.4"} trailing`,
		`{"name":"a.home.lan","value":"` + strings.Repeat("1", 5000) + `"}`,
	} {
		if w := do(h, req{"POST", "/api/records", body, authJSON(nil)}); w.Code != http.StatusBadRequest {
			t.Errorf("%.60s: got %d %s", body, w.Code, w.Body)
		}
	}
}

// Finding 15: security headers on every response.
func TestSecurityHeaders(t *testing.T) {
	h := newServer(t, func(s *Server) { s.TLS = true })
	for _, path := range []string{"/", "/app.js", "/api/info"} {
		w := do(h, req{"GET", path, "", nil})
		for k, want := range map[string]string{
			"Content-Security-Policy":   "frame-ancestors 'none'",
			"X-Frame-Options":           "DENY",
			"X-Content-Type-Options":    "nosniff",
			"Strict-Transport-Security": "max-age=",
			"Referrer-Policy":           "no-referrer",
		} {
			if !strings.Contains(w.Header().Get(k), want) {
				t.Errorf("%s: %s = %q", path, k, w.Header().Get(k))
			}
		}
	}
	w := do(h, req{"GET", "/", "", nil})
	if strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "<style>") || strings.Contains(w.Body.String(), "googleapis") {
		t.Error("page has inline script/style or third-party resources")
	}
}

// Finding 9: changes are rate limited.
func TestWriteRateLimit(t *testing.T) {
	h := newServer(t, nil)
	limited := false
	for i := 0; i < 40; i++ {
		w := do(h, req{"DELETE", "/api/records/none.home.lan", "", map[string]string{"Authorization": "Bearer " + token}})
		if w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("40 rapid changes were never rate limited")
	}
}

func TestReadOnlyReplica(t *testing.T) {
	h := newServer(t, func(s *Server) { s.ReadOnly = true })
	if w := do(h, req{"POST", "/api/records", `{"name":"a.home.lan","value":"1.2.3.4"}`, authJSON(nil)}); w.Code != http.StatusConflict {
		t.Errorf("replica accepted a change: %d", w.Code)
	}
}

func TestSettings(t *testing.T) {
	h := newServer(t, nil)
	auth := map[string]string{"Authorization": "Bearer " + token}
	if w := do(h, req{"GET", "/api/settings", "", nil}); w.Code != http.StatusUnauthorized {
		t.Errorf("settings readable without token: %d", w.Code)
	}
	w := do(h, req{"GET", "/api/settings", "", auth})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"zones"`) || strings.Contains(w.Body.String(), token) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}

	// Add a record, then preview moving to another domain: a warning, no change.
	do(h, req{"POST", "/api/records", `{"name":"nas.home.lan","value":"10.0.0.10"}`, authJSON(nil)})
	w = do(h, req{"PUT", "/api/settings", `{"set":{"zones":["lab.example"]},"dry_run":true}`, authJSON(nil)})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "stop being served") || strings.Contains(w.Body.String(), `"applied": true`) {
		t.Fatalf("dry run: %d %s", w.Code, w.Body)
	}
	if w := do(h, req{"POST", "/api/records", `{"name":"x.lab.example","value":"10.0.0.1"}`, authJSON(nil)}); w.Code != 400 {
		t.Errorf("dry run changed the zones: %d", w.Code)
	}

	// Apply it.
	w = do(h, req{"PUT", "/api/settings", `{"set":{"zones":["home.lan","lab.example"],"upstreams":["9.9.9.9"]}}`, authJSON(nil)})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"applied": true`) || !strings.Contains(w.Body.String(), `"dashboard"`) {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
	if w := do(h, req{"POST", "/api/records", `{"name":"x.lab.example","value":"10.0.0.1"}`, authJSON(nil)}); w.Code != http.StatusCreated {
		t.Errorf("new zone not in effect: %d %s", w.Code, w.Body)
	}

	// Bad values and read-only settings are refused.
	for _, body := range []string{
		`{"set":{"zones":["com"]}}`,
		`{"set":{"upstreams":["1.1.1.1\nconf-file=/etc/shadow"]}}`,
		`{"set":{"upstreams":[]}}`,
		`{"set":{"docker_target":"fe80::1%z"}}`,
		`{"set":{"cache_size":-1}}`,
		`{"set":{"token":"x"}}`,
		`{"set":{"http_listen":":80"}}`,
		`{"reset":["token"]}`,
	} {
		if w := do(h, req{"PUT", "/api/settings", body, authJSON(nil)}); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s", body, w.Code, w.Body)
		}
	}

	// Reset goes back to the docker-compose value.
	w = do(h, req{"PUT", "/api/settings", `{"reset":["zones"]}`, authJSON(nil)})
	if w.Code != 200 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if w := do(h, req{"POST", "/api/records", `{"name":"y.lab.example","value":"10.0.0.2"}`, authJSON(nil)}); w.Code != 400 {
		t.Errorf("reset didn't restore the zones: %d", w.Code)
	}

	// Same protections as other changes.
	if w := do(h, req{"PUT", "/api/settings", `{"set":{"zones":["evil.example"]}}`, map[string]string{"Content-Type": "text/plain", "Authorization": "Bearer " + token}}); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain settings change: %d", w.Code)
	}
	if w := do(h, req{"PUT", "/api/settings", `{"set":{"zones":["evil.example"]}}`, authJSON(map[string]string{"Origin": "https://evil.example"})}); w.Code != http.StatusForbidden {
		t.Errorf("cross-site settings change: %d", w.Code)
	}
}
