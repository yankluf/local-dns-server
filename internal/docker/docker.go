// Package docker discovers hostnames from the Traefik router rules in the
// labels of running containers, and keeps them in sync as containers start
// and stop.
package docker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yankluf/local-dns-server/internal/config"
	"github.com/yankluf/local-dns-server/internal/names"
	"github.com/yankluf/local-dns-server/internal/store"
)

// Labels that control discovery on a container.
const (
	LabelEnable = "lds.enable" // "false" skips the container
	LabelTarget = "lds.target" // overrides the IP; must be in LDS_DOCKER_ALLOWED_TARGETS
	LabelHosts  = "lds.hosts"  // extra comma-separated hostnames
)

// Status describes the watcher.
type Status struct {
	Enabled   bool      `json:"enabled"`
	Target    string    `json:"target,omitempty"`
	Connected bool      `json:"connected"`
	LastSync  time.Time `json:"last_sync,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// Watcher syncs Docker records into the store.
type Watcher struct {
	store  *store.Store
	client *http.Client
	base   string
	wake   chan struct{}

	mu     sync.Mutex
	cfg    config.Docker
	status Status
}

// SetConfig changes the target IPs and zones (from the dashboard) and
// triggers a fresh sync. An empty target turns discovery off.
func (w *Watcher) SetConfig(cfg config.Docker) {
	w.mu.Lock()
	cfg.Host, cfg.TLSVerify, cfg.CertPath, cfg.AllowInsecure = w.cfg.Host, w.cfg.TLSVerify, w.cfg.CertPath, w.cfg.AllowInsecure
	w.cfg = cfg
	w.status.Enabled, w.status.Target = cfg.Enabled, cfg.Target
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Watcher) config() config.Docker {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg
}

// NewWatcher builds a watcher that talks to cfg.Host.
func NewWatcher(cfg config.Docker, st *store.Store) (*Watcher, error) {
	w := &Watcher{cfg: cfg, store: st, wake: make(chan struct{}, 1), status: Status{Enabled: cfg.Enabled, Target: cfg.Target}}
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("DOCKER_HOST: %w", err)
	}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		w.base = "http://docker"
		w.client = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		}}
	case "tcp", "http", "https":
		if cfg.TLSVerify || u.Scheme == "https" {
			tlsCfg, err := dockerTLS(cfg.CertPath)
			if err != nil {
				return nil, err
			}
			w.base = "https://" + u.Host
			w.client = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
			break
		}
		if !config.IsLoopback(u.Host) && !cfg.AllowInsecure {
			return nil, fmt.Errorf("DOCKER_HOST %s is unencrypted and not on this machine; "+
				"set DOCKER_TLS_VERIFY=1 and DOCKER_CERT_PATH, or LDS_DOCKER_ALLOW_INSECURE=true on a trusted network", cfg.Host)
		}
		w.base = "http://" + u.Host
		w.client = &http.Client{}
	default:
		return nil, fmt.Errorf("DOCKER_HOST scheme %q is not supported (use unix:// or tcp://)", u.Scheme)
	}
	return w, nil
}

// dockerTLS loads ca.pem, cert.pem and key.pem from dir, like the docker CLI.
func dockerTLS(dir string) (*tls.Config, error) {
	if dir == "" {
		return nil, errors.New("DOCKER_TLS_VERIFY needs DOCKER_CERT_PATH (a folder with ca.pem, cert.pem, key.pem)")
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("DOCKER_CERT_PATH/ca.pem has no certificates")
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, err
	}
	return &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// Status returns the watcher state.
func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *Watcher) setStatus(fn func(*Status)) {
	w.mu.Lock()
	fn(&w.status)
	w.mu.Unlock()
}

// Run syncs immediately, then on every container event and once a minute,
// reconnecting on errors, until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	events := make(chan struct{}, 1)
	go w.watchEvents(ctx, events)

	var debounce <-chan time.Time
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	w.sync(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
			w.sync(ctx)
		case <-events:
			// Containers often emit several events in a row; wait for them to settle.
			debounce = time.After(time.Second)
		case <-debounce:
			debounce = nil
			w.sync(ctx)
		case <-tick.C:
			w.sync(ctx)
		}
	}
}

func (w *Watcher) sync(ctx context.Context) {
	if !w.config().Enabled {
		if changed, _ := w.store.SetDocker(nil); changed {
			log.Print("docker discovery off: removed Docker records")
		}
		w.setStatus(func(s *Status) { s.Connected = false; s.LastError = "" })
		return
	}
	records, err := w.discover(ctx)
	if err != nil {
		log.Printf("docker sync: %v", err)
		w.setStatus(func(s *Status) { s.LastError = err.Error(); s.Connected = false })
		return
	}
	changed, err := w.store.SetDocker(records)
	if err != nil {
		log.Printf("docker sync: keeping the previous Docker records: %v", err)
		w.setStatus(func(s *Status) { s.LastError = err.Error(); s.Connected = true })
		return
	}
	if changed {
		log.Printf("docker sync: %d record(s)", len(records))
	}
	w.setStatus(func(s *Status) { s.LastSync = time.Now().UTC(); s.LastError = ""; s.Connected = true })
}

// Container is the subset of the Docker container listing that discovery uses.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
}

func (w *Watcher) discover(ctx context.Context) ([]store.Record, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.base+"/containers/json", nil)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list containers: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var containers []Container
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&containers); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	return RecordsFromContainers(containers, w.config()), nil
}

// RecordsFromContainers turns container labels into DNS records. Only plain
// hostnames inside the configured zones become records (never wildcards),
// and lds.target is honored only for IPs listed in LDS_DOCKER_ALLOWED_TARGETS.
// When two containers claim the same hostname, the first one (by container
// name) wins.
func RecordsFromContainers(containers []Container, cfg config.Docker) []store.Record {
	sort.Slice(containers, func(i, j int) bool { return name(containers[i]) < name(containers[j]) })
	allowed := map[string]bool{}
	for _, t := range cfg.AllowedTargets {
		allowed[t] = true
	}
	seen := map[string]bool{}
	var out []store.Record
	for _, c := range containers {
		cname := name(c)
		if strings.EqualFold(c.Labels[LabelEnable], "false") || strings.EqualFold(c.Labels["traefik.enable"], "false") {
			continue
		}
		target := cfg.Target
		if t := strings.TrimSpace(c.Labels[LabelTarget]); t != "" {
			ip, err := names.ParseIP(t)
			switch {
			case err != nil:
				log.Printf("docker: container %s: ignoring invalid %s", cname, LabelTarget)
			case !allowed[ip.String()]:
				log.Printf("docker: container %s: ignoring %s=%s (not in LDS_DOCKER_ALLOWED_TARGETS)", cname, LabelTarget, ip)
			default:
				target = ip.String()
			}
		}
		var hosts []string
		for k, v := range c.Labels {
			if strings.HasPrefix(k, "traefik.") && strings.HasSuffix(k, ".rule") {
				hosts = append(hosts, HostsFromRule(v)...)
			}
		}
		for _, h := range strings.Split(c.Labels[LabelHosts], ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
		sort.Strings(hosts)
		for _, h := range hosts {
			if strings.Contains(h, "*") {
				log.Printf("docker: container %s: skipping wildcard %q (add wildcards by hand)", cname, names.CleanText(h, 100))
				continue
			}
			r, err := store.New(h, target, "", cfg.Zones)
			if err != nil {
				log.Printf("docker: container %s: skipping %q: %v", cname, names.CleanText(h, 100), err)
				continue
			}
			if r.Type != store.TypeA && r.Type != store.TypeAAAA {
				continue
			}
			if seen[r.Name] {
				continue
			}
			seen[r.Name] = true
			r.Container = cname
			out = append(out, r)
		}
	}
	return out
}

func name(c Container) string {
	n := c.ID
	if len(c.Names) > 0 {
		n = strings.TrimPrefix(c.Names[0], "/")
	} else if len(n) > 12 {
		n = n[:12]
	}
	return names.CleanText(n, 128)
}

var (
	hostMatcher = regexp.MustCompile(`\bHost(?:SNI)?\(([^)]*)\)`)
	quoted      = regexp.MustCompile("`([^`]*)`|\"([^\"]*)\"")
)

// HostsFromRule extracts hostnames from a Traefik rule such as
// "Host(`a.home.lan`) || Host(`b.home.lan`) && PathPrefix(`/api`)".
// HostRegexp matchers and the HostSNI(`*`) catch-all are ignored.
func HostsFromRule(rule string) []string {
	var hosts []string
	for _, m := range hostMatcher.FindAllStringSubmatch(rule, -1) {
		for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
			h := strings.TrimSpace(q[1] + q[2])
			if h != "" && h != "*" {
				hosts = append(hosts, strings.ToLower(h))
			}
		}
	}
	return hosts
}

func (w *Watcher) watchEvents(ctx context.Context, events chan<- struct{}) {
	filters := url.QueryEscape(`{"type":["container"],"event":["start","die","destroy","rename","update"]}`)
	for ctx.Err() == nil {
		if !w.config().Enabled { // discovery off: don't connect, check again soon
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		err := w.streamEvents(ctx, filters, events)
		if ctx.Err() != nil {
			return
		}
		log.Printf("docker events: %v (reconnecting in 5s)", err)
		w.setStatus(func(s *Status) { s.Connected = false; s.LastError = "events: " + err.Error() })
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		// Catch up on anything missed while disconnected.
		select {
		case events <- struct{}{}:
		default:
		}
	}
}

func (w *Watcher) streamEvents(ctx context.Context, filters string, events chan<- struct{}) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.base+"/events?filters="+filters, nil)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev json.RawMessage
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return fmt.Errorf("event stream closed")
			}
			return err
		}
		select {
		case events <- struct{}{}:
		default:
		}
	}
}
