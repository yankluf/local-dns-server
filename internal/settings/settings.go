// Package settings lets the day-to-day settings be changed from the
// dashboard. Changes are stored in <data>/settings.json and take precedence
// over the environment (docker-compose.yml). Settings that protect the server
// itself (token, ports, TLS, data folder, Docker connection) stay read-only
// and can only be changed in docker-compose.yml.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/yankluf/local-dns-server/internal/config"
	"github.com/yankluf/local-dns-server/internal/store"
)

// Field describes one setting for the dashboard.
type Field struct {
	Key      string `json:"key"`
	Group    string `json:"group"`
	Label    string `json:"label"`
	Help     string `json:"help"`
	Env      string `json:"env"`
	Kind     string `json:"kind"` // list, ip, bool, int, text
	Value    any    `json:"value"`
	Source   string `json:"source"` // default, docker-compose, dashboard, generated
	Editable bool   `json:"editable"`
}

// Change is a request to change settings.
type Change struct {
	Set    config.Overrides `json:"set"`
	Reset  []string         `json:"reset"` // keys to return to the docker-compose/default value
	DryRun bool             `json:"dry_run"`
}

// Result is what a change did (or would do).
type Result struct {
	Applied  bool     `json:"applied"`
	Warnings []string `json:"warnings,omitempty"`
	Fields   []Field  `json:"fields"`
}

// Manager holds the environment config and the dashboard overrides.
type Manager struct {
	path string
	base config.Config // from the environment

	// Check is called with a candidate config before it's saved. It returns
	// warnings to show, or an error to reject the change.
	Check func(config.Config) ([]string, error)
	// Apply is called with the new config after it's saved.
	Apply func(config.Config)
	// Records, if set, is used to warn about records a zone change would
	// stop serving.
	Records interface {
		Preview(zones []string) ([]store.Record, []string)
	}

	upd       sync.Mutex // serializes Update calls
	mu        sync.Mutex
	overrides config.Overrides
	current   config.Config
	loadErr   string
}

// Load reads saved overrides from path. If they're unreadable or no longer
// valid, they're ignored (and reported) so DNS still starts.
func Load(path string, base config.Config) *Manager {
	m := &Manager{path: path, base: base, current: base}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m
	}
	var o config.Overrides
	if err == nil {
		err = json.Unmarshal(data, &o)
	}
	if err == nil {
		var cfg config.Config
		if cfg, err = base.WithOverrides(o); err == nil {
			m.overrides, m.current = o, cfg
			return m
		}
	}
	m.loadErr = fmt.Sprintf("dashboard settings in %s were ignored: %v", path, err)
	log.Print(m.loadErr)
	return m
}

// Current returns the effective config.
func (m *Manager) Current() config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// LoadError reports saved settings that couldn't be used, if any.
func (m *Manager) LoadError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadErr
}

// Update validates and (unless DryRun) saves and applies a change.
func (m *Manager) Update(ch Change) (Result, error) {
	m.upd.Lock()
	defer m.upd.Unlock()
	m.mu.Lock()
	next := merge(m.overrides, ch.Set)
	for _, k := range ch.Reset {
		if err := clear(&next, k); err != nil {
			m.mu.Unlock()
			return Result{}, err
		}
	}
	m.mu.Unlock()

	cfg, err := m.base.WithOverrides(next)
	if err != nil {
		return Result{}, err
	}
	var warnings []string
	if m.Check != nil {
		if warnings, err = m.Check(cfg); err != nil {
			return Result{}, err
		}
	}
	if m.Records != nil {
		if _, dropped := m.Records.Preview(cfg.Zones); len(dropped) > 0 {
			shown := dropped
			if len(dropped) > 5 {
				shown = append(append([]string{}, dropped[:5]...), fmt.Sprintf("and %d more", len(dropped)-5))
			}
			warnings = append(warnings, fmt.Sprintf("%d record(s) are outside the new domains and will stop being served (they stay saved): %s",
				len(dropped), strings.Join(shown, ", ")))
		}
	}
	if m.Current().Docker.Enabled && !cfg.Docker.Enabled {
		warnings = append(warnings, "Docker discovery will be turned off, removing all Docker records")
	}
	if ch.DryRun {
		return Result{Warnings: warnings, Fields: describe(m.base, next, cfg)}, nil
	}

	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := store.WriteFileDurable(m.path+".tmp", append(data, '\n'), 0o600); err != nil {
		return Result{}, err
	}
	if err := os.Rename(m.path+".tmp", m.path); err != nil {
		return Result{}, err
	}
	m.mu.Lock()
	m.overrides, m.current, m.loadErr = next, cfg, ""
	m.mu.Unlock()
	if m.Apply != nil {
		m.Apply(cfg)
	}
	log.Printf("settings changed from the dashboard")
	return Result{Applied: true, Warnings: warnings, Fields: describe(m.base, next, cfg)}, nil
}

// Fields describes every setting for the dashboard.
func (m *Manager) Fields() []Field {
	m.mu.Lock()
	defer m.mu.Unlock()
	return describe(m.base, m.overrides, m.current)
}

func merge(o, set config.Overrides) config.Overrides {
	if set.Zones != nil {
		o.Zones = set.Zones
	}
	if set.Upstreams != nil {
		o.Upstreams = set.Upstreams
	}
	if set.DockerTarget != nil {
		o.DockerTarget = set.DockerTarget
	}
	if set.DockerAllowedTargets != nil {
		o.DockerAllowedTargets = set.DockerAllowedTargets
	}
	if set.DNSSEC != nil {
		o.DNSSEC = set.DNSSEC
	}
	if set.StopRebind != nil {
		o.StopRebind = set.StopRebind
	}
	if set.CacheSize != nil {
		o.CacheSize = set.CacheSize
	}
	if set.LogQueries != nil {
		o.LogQueries = set.LogQueries
	}
	if set.MaxRecords != nil {
		o.MaxRecords = set.MaxRecords
	}
	return o
}

func clear(o *config.Overrides, key string) error {
	switch key {
	case "zones":
		o.Zones = nil
	case "upstreams":
		o.Upstreams = nil
	case "docker_target":
		o.DockerTarget = nil
	case "docker_allowed_targets":
		o.DockerAllowedTargets = nil
	case "dnssec":
		o.DNSSEC = nil
	case "stop_dns_rebind":
		o.StopRebind = nil
	case "cache_size":
		o.CacheSize = nil
	case "log_queries":
		o.LogQueries = nil
	case "max_records":
		o.MaxRecords = nil
	default:
		return fmt.Errorf("%q can't be changed from the dashboard", key)
	}
	return nil
}

func envSource(env string) string {
	if v, ok := os.LookupEnv(env); ok && strings.TrimSpace(v) != "" {
		return "docker-compose"
	}
	return "default"
}

func describe(base config.Config, o config.Overrides, c config.Config) []Field {
	src := func(overridden bool, env string) string {
		if overridden {
			return "dashboard"
		}
		return envSource(env)
	}
	list := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}
	tokenValue, tokenSource := "set", envSource("LDS_TOKEN")
	switch {
	case c.InsecureNoAuth && c.Token == "":
		tokenValue = "off (LDS_INSECURE_NO_AUTH)"
	case base.TokenAuto:
		tokenValue, tokenSource = "generated, saved in "+c.DataDir+"/admin-token", "generated"
	case os.Getenv("LDS_TOKEN_FILE") != "":
		tokenValue, tokenSource = "read from "+os.Getenv("LDS_TOKEN_FILE"), "docker-compose"
	}
	dnsListen := "all addresses, local networks only"
	if len(c.DNSmasq.Listen) > 0 {
		dnsListen = strings.Join(c.DNSmasq.Listen, ", ")
	}
	tls := "off"
	if c.TLSCert != "" {
		tls = "on"
	}
	replica := "no (this is a primary)"
	if c.ReplicaOf != "" {
		replica = c.ReplicaOf
	}

	return []Field{
		{Key: "zones", Group: "Records", Label: "Domains", Kind: "list", Env: "LDS_ZONES", Editable: true,
			Value: list(c.Zones), Source: src(o.Zones != nil, "LDS_ZONES"),
			Help: "Domains LDS answers for, like home.lan. Records must be inside them. Names in them never leave your network."},
		{Key: "max_records", Group: "Records", Label: "Record limit", Kind: "int", Env: "LDS_MAX_RECORDS", Editable: true,
			Value: c.MaxRecords, Source: src(o.MaxRecords != nil, "LDS_MAX_RECORDS"),
			Help: "Most records that can be added by hand."},

		{Key: "upstreams", Group: "Internet lookups", Label: "Upstream DNS servers", Kind: "list", Env: "LDS_UPSTREAMS", Editable: true,
			Value: list(c.DNSmasq.Upstreams), Source: src(o.Upstreams != nil, "LDS_UPSTREAMS"),
			Help: "Where lookups for other names go. IP addresses, optionally with #port."},
		{Key: "dnssec", Group: "Internet lookups", Label: "Check DNSSEC signatures", Kind: "bool", Env: "LDS_DNSSEC", Editable: true,
			Value: c.DNSmasq.DNSSEC, Source: src(o.DNSSEC != nil, "LDS_DNSSEC"),
			Help: "Reject forged answers for signed domains. Needs the server's clock to be right."},
		{Key: "stop_dns_rebind", Group: "Internet lookups", Label: "Block rebinding", Kind: "bool", Env: "LDS_STOP_DNS_REBIND", Editable: true,
			Value: c.DNSmasq.StopRebind, Source: src(o.StopRebind != nil, "LDS_STOP_DNS_REBIND"),
			Help: "Drop outside answers that point at private IPs. Turn off if company sites resolve to internal addresses."},
		{Key: "cache_size", Group: "Internet lookups", Label: "Cache size", Kind: "int", Env: "LDS_CACHE_SIZE", Editable: true,
			Value: c.DNSmasq.CacheSize, Source: src(o.CacheSize != nil, "LDS_CACHE_SIZE"),
			Help: "How many answers to remember (0 to 100000)."},
		{Key: "log_queries", Group: "Internet lookups", Label: "Log every lookup", Kind: "bool", Env: "LDS_LOG_QUERIES", Editable: true,
			Value: c.DNSmasq.LogQueries, Source: src(o.LogQueries != nil, "LDS_LOG_QUERIES"),
			Help: "Write each query to the container log. Handy for debugging, noisy otherwise."},

		{Key: "docker_target", Group: "Docker", Label: "Traefik IP", Kind: "ip", Env: "LDS_DOCKER_TARGET", Editable: true,
			Value: c.Docker.Target, Source: src(o.DockerTarget != nil, "LDS_DOCKER_TARGET"),
			Help: "Names from your containers' Traefik rules point here. Leave empty to turn Docker discovery off."},
		{Key: "docker_allowed_targets", Group: "Docker", Label: "Other allowed IPs", Kind: "list", Env: "LDS_DOCKER_ALLOWED_TARGETS", Editable: true,
			Value: list(c.Docker.ExtraTargets), Source: src(o.DockerAllowedTargets != nil, "LDS_DOCKER_ALLOWED_TARGETS"),
			Help: "IPs a container may choose with the lds.target label."},
		{Key: "docker_host", Group: "Docker", Label: "Docker connection", Kind: "text", Env: "DOCKER_HOST",
			Value: c.Docker.Host, Source: envSource("DOCKER_HOST"), Help: "How LDS reaches Docker."},

		{Key: "token", Group: "Server", Label: "Access token", Kind: "text", Env: "LDS_TOKEN_FILE",
			Value: tokenValue, Source: tokenSource, Help: "Needed to sign in and to change anything. Never shown here."},
		{Key: "http_listen", Group: "Server", Label: "Web page address", Kind: "text", Env: "LDS_HTTP_LISTEN",
			Value: c.HTTPListen, Source: envSource("LDS_HTTP_LISTEN"), Help: "Where this page and the API listen."},
		{Key: "tls", Group: "Server", Label: "HTTPS", Kind: "text", Env: "LDS_TLS_CERT",
			Value: tls, Source: envSource("LDS_TLS_CERT"), Help: "Encrypts the token on the network. Set LDS_TLS_CERT and LDS_TLS_KEY."},
		{Key: "allowed_hosts", Group: "Server", Label: "Extra page names", Kind: "list", Env: "LDS_ALLOWED_HOSTS",
			Value: list(c.AllowedHosts), Source: envSource("LDS_ALLOWED_HOSTS"), Help: "Names this page may be opened under, besides IPs and your domains."},
		{Key: "public_read", Group: "Server", Label: "Public read", Kind: "bool", Env: "LDS_PUBLIC_READ",
			Value: c.PublicRead, Source: envSource("LDS_PUBLIC_READ"), Help: "Let anyone see records without the token."},
		{Key: "dns_listen", Group: "Server", Label: "DNS addresses", Kind: "text", Env: "LDS_DNS_LISTEN",
			Value: dnsListen, Source: envSource("LDS_DNS_LISTEN"), Help: "Where DNS answers."},
		{Key: "dns_port", Group: "Server", Label: "DNS port", Kind: "int", Env: "LDS_DNS_PORT",
			Value: c.DNSmasq.Port, Source: envSource("LDS_DNS_PORT"), Help: "Port for DNS."},
		{Key: "replica_of", Group: "Server", Label: "Copy of", Kind: "text", Env: "LDS_REPLICA_OF",
			Value: replica, Source: envSource("LDS_REPLICA_OF"), Help: "Whether this server copies its records from another LDS."},
		{Key: "data_dir", Group: "Server", Label: "Data folder", Kind: "text", Env: "LDS_DATA_DIR",
			Value: c.DataDir, Source: envSource("LDS_DATA_DIR"), Help: "Where records and settings are saved."},
	}
}
