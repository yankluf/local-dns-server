// Package docker discovers hostnames from the Traefik router rules in the
// labels of running containers, and keeps them in sync as containers start
// and stop.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yankluf/local-dns-server/internal/config"
	"github.com/yankluf/local-dns-server/internal/store"
)

// Labels that control discovery on a container.
const (
	LabelEnable = "localdns.enable" // "false" skips the container
	LabelTarget = "localdns.target" // overrides the IP for this container's hosts
	LabelHosts  = "localdns.hosts"  // extra comma-separated hostnames
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
	cfg    config.Docker
	store  *store.Store
	client *http.Client
	base   string

	mu     sync.Mutex
	status Status
}

// NewWatcher builds a watcher that talks to cfg.Host.
func NewWatcher(cfg config.Docker, st *store.Store) (*Watcher, error) {
	w := &Watcher{cfg: cfg, store: st, status: Status{Enabled: cfg.Enabled, Target: cfg.Target}}
	if _, err := netip.ParseAddr(cfg.Target); err != nil {
		return nil, fmt.Errorf("LOCALDNS_DOCKER_TARGET must be an IP address: %w", err)
	}
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
	case "tcp", "http":
		w.base = "http://" + u.Host
		w.client = &http.Client{}
	default:
		return nil, fmt.Errorf("DOCKER_HOST scheme %q is not supported (use unix:// or tcp://)", u.Scheme)
	}
	return w, nil
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
	records, err := w.discover(ctx)
	if err != nil {
		log.Printf("docker sync: %v", err)
		w.setStatus(func(s *Status) { s.LastError = err.Error(); s.Connected = false })
		return
	}
	if w.store.SetDocker(records) {
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
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	return RecordsFromContainers(containers, w.cfg.Target), nil
}

// RecordsFromContainers turns container labels into DNS records. When two
// containers claim the same hostname, the first one (by container name) wins.
func RecordsFromContainers(containers []Container, defaultTarget string) []store.Record {
	sort.Slice(containers, func(i, j int) bool { return name(containers[i]) < name(containers[j]) })
	seen := map[string]bool{}
	var out []store.Record
	for _, c := range containers {
		if strings.EqualFold(c.Labels[LabelEnable], "false") || strings.EqualFold(c.Labels["traefik.enable"], "false") {
			continue
		}
		target := defaultTarget
		if t := strings.TrimSpace(c.Labels[LabelTarget]); t != "" {
			if _, err := netip.ParseAddr(t); err == nil {
				target = t
			} else {
				log.Printf("docker: container %s: ignoring invalid %s=%q", name(c), LabelTarget, t)
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
			r, err := store.New(h, target, "")
			if err != nil {
				log.Printf("docker: container %s: skipping %q: %v", name(c), h, err)
				continue
			}
			if seen[r.Name] {
				continue
			}
			seen[r.Name] = true
			r.Container = name(c)
			out = append(out, r)
		}
	}
	return out
}

func name(c Container) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
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
