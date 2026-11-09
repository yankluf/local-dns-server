// Package config reads and validates the server settings from environment
// variables. Every value that ends up in the dnsmasq config is checked here,
// so a bad setting stops LDS at startup instead of producing a broken or
// injectable config.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yankluf/local-dns-server/internal/names"
)

// MinTokenLength is the shortest accepted API token.
const MinTokenLength = 16

// Config holds the settings for `lds serve`.
type Config struct {
	HTTPListen     string        // LDS_HTTP_LISTEN
	DataDir        string        // LDS_DATA_DIR
	Token          string        // LDS_TOKEN or the contents of LDS_TOKEN_FILE
	TokenAuto      bool          // no token given: lds serve generates one in the data folder
	InsecureNoAuth bool          // LDS_INSECURE_NO_AUTH: allow running without a token
	PublicRead     bool          // LDS_PUBLIC_READ: allow reading records without the token
	AllowedHosts   []string      // LDS_ALLOWED_HOSTS: extra Host header values accepted by the API
	TLSCert        string        // LDS_TLS_CERT
	TLSKey         string        // LDS_TLS_KEY
	Zones          []string      // LDS_ZONES: the only domains records may use (editable)
	AllowTLDZones  bool          // LDS_ALLOW_TLD_ZONES
	MaxRecords     int           // LDS_MAX_RECORDS (editable)
	ReplicaOf      string        // LDS_REPLICA_OF: primary's URL; makes this server a read-only copy
	ReplicaEvery   time.Duration // LDS_REPLICA_INTERVAL

	DNSmasq DNSmasq
	Docker  Docker
}

// DNSmasq configures the managed dnsmasq process.
type DNSmasq struct {
	Enabled        bool     // LDS_DNSMASQ_ENABLED
	Binary         string   // LDS_DNSMASQ_BIN
	Port           int      // LDS_DNS_PORT
	Listen         []string // LDS_DNS_LISTEN: listen addresses; empty = all, local networks only
	Upstreams      []string // LDS_UPSTREAMS
	CacheSize      int      // LDS_CACHE_SIZE
	LogQueries     bool     // LDS_LOG_QUERIES
	DNSSEC         bool     // LDS_DNSSEC
	StopRebind     bool     // LDS_STOP_DNS_REBIND
	ExtraDir       string   // LDS_DNSMASQ_EXTRA_DIR: included with conf-dir if it exists
	Zones          []string // copied from Config.Zones
	ConfigPath     string   // <data>/dnsmasq.conf
	HostsDir       string   // <data>/hosts.d, reloaded by dnsmasq without a restart
	RestartSpacing time.Duration
}

// Docker configures container discovery.
type Docker struct {
	Enabled        bool     // true when Target is set
	Host           string   // DOCKER_HOST
	TLSVerify      bool     // DOCKER_TLS_VERIFY
	CertPath       string   // DOCKER_CERT_PATH
	AllowInsecure  bool     // LDS_DOCKER_ALLOW_INSECURE: plain tcp:// to a non-loopback host
	Target         string   // LDS_DOCKER_TARGET: IP that Traefik listens on
	ExtraTargets   []string // LDS_DOCKER_ALLOWED_TARGETS: other IPs a container may pick with lds.target
	AllowedTargets []string // Target plus ExtraTargets
	Zones          []string // copied from Config.Zones
}

// FromEnv builds and validates the config from environment variables.
func FromEnv() (Config, error) {
	var errs []error
	c := Config{
		HTTPListen: env("LDS_HTTP_LISTEN", ":5380"),
		DataDir:    filepath.Clean(env("LDS_DATA_DIR", "/data")),
		TLSCert:    os.Getenv("LDS_TLS_CERT"),
		TLSKey:     os.Getenv("LDS_TLS_KEY"),
		ReplicaOf:  strings.TrimSpace(os.Getenv("LDS_REPLICA_OF")),
	}
	b := func(key string, def bool) bool {
		v, err := envBool(key, def)
		errs = append(errs, err)
		return v
	}
	i := func(key string, def, min, max int) int {
		v, err := envInt(key, def, min, max)
		errs = append(errs, err)
		return v
	}

	// Auth.
	c.Token = os.Getenv("LDS_TOKEN")
	if f := os.Getenv("LDS_TOKEN_FILE"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, fmt.Errorf("LDS_TOKEN_FILE: %w", err))
		}
		c.Token = strings.TrimSpace(string(data))
	}
	c.InsecureNoAuth = b("LDS_INSECURE_NO_AUTH", false)
	c.PublicRead = b("LDS_PUBLIC_READ", false)
	switch {
	case c.Token == "" && !c.InsecureNoAuth:
		c.TokenAuto = true // lds serve creates and saves a random token
	case c.Token != "" && len(c.Token) < MinTokenLength:
		errs = append(errs, fmt.Errorf("LDS_TOKEN must be at least %d characters", MinTokenLength))
	}
	for _, h := range envList("LDS_ALLOWED_HOSTS", "") {
		c.AllowedHosts = append(c.AllowedHosts, strings.ToLower(h))
	}
	if _, _, err := net.SplitHostPort(c.HTTPListen); err != nil {
		errs = append(errs, fmt.Errorf("LDS_HTTP_LISTEN: %w", err))
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, errors.New("set both LDS_TLS_CERT and LDS_TLS_KEY, or neither"))
	}

	// Zones (optional at startup: they can be set from the dashboard).
	c.AllowTLDZones = b("LDS_ALLOW_TLD_ZONES", false)
	c.Zones = envList("LDS_ZONES", "")
	c.MaxRecords = i("LDS_MAX_RECORDS", 5000, 1, 1_000_000)

	// Replica.
	c.ReplicaEvery = time.Duration(i("LDS_REPLICA_INTERVAL", 30, 5, 3600)) * time.Second
	if c.ReplicaOf != "" {
		u, err := url.Parse(c.ReplicaOf)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, errors.New("LDS_REPLICA_OF must be a URL like https://dns1.home.lan:5380"))
		}
		if c.TokenAuto {
			errs = append(errs, errors.New("LDS_REPLICA_OF needs LDS_TOKEN or LDS_TOKEN_FILE set to the primary's token"))
		}
	}

	// dnsmasq.
	d := &c.DNSmasq
	d.Enabled = b("LDS_DNSMASQ_ENABLED", true)
	d.Binary = env("LDS_DNSMASQ_BIN", "dnsmasq")
	d.Port = i("LDS_DNS_PORT", 53, 1, 65535)
	d.CacheSize = i("LDS_CACHE_SIZE", 10000, 0, 100000)
	d.LogQueries = b("LDS_LOG_QUERIES", false)
	d.DNSSEC = b("LDS_DNSSEC", true)
	d.StopRebind = b("LDS_STOP_DNS_REBIND", true)
	d.ExtraDir = env("LDS_DNSMASQ_EXTRA_DIR", "/etc/dnsmasq.d")
	d.RestartSpacing = time.Duration(i("LDS_RESTART_SPACING_MS", 2000, 0, 60000)) * time.Millisecond
	for _, l := range envList("LDS_DNS_LISTEN", "") {
		if _, err := names.ParseIP(l); err != nil {
			errs = append(errs, fmt.Errorf("LDS_DNS_LISTEN: %w", err))
			continue
		}
		d.Listen = append(d.Listen, l)
	}
	d.Upstreams = envList("LDS_UPSTREAMS", "1.1.1.1,1.0.0.1,9.9.9.9")
	if err := names.SafePath(c.DataDir); err != nil {
		errs = append(errs, fmt.Errorf("LDS_DATA_DIR: %w", err))
	}
	if d.ExtraDir != "" {
		if err := names.SafePath(d.ExtraDir); err != nil {
			errs = append(errs, fmt.Errorf("LDS_DNSMASQ_EXTRA_DIR: %w", err))
		}
	}
	d.ConfigPath = filepath.Join(c.DataDir, "dnsmasq.conf")
	d.HostsDir = filepath.Join(c.DataDir, "hosts.d")

	// Docker.
	k := &c.Docker
	k.Host = env("DOCKER_HOST", "unix:///var/run/docker.sock")
	k.TLSVerify = os.Getenv("DOCKER_TLS_VERIFY") != ""
	k.CertPath = os.Getenv("DOCKER_CERT_PATH")
	k.AllowInsecure = b("LDS_DOCKER_ALLOW_INSECURE", false)
	k.Target = strings.TrimSpace(os.Getenv("LDS_DOCKER_TARGET"))
	k.ExtraTargets = envList("LDS_DOCKER_ALLOWED_TARGETS", "")

	errs = append(errs, c.finish()...)
	return c, errors.Join(errs...)
}

// finish validates and normalizes the settings that can also be changed from
// the dashboard, and fills in the values derived from them.
func (c *Config) finish() []error {
	var errs []error
	var zones []string
	for _, z := range c.Zones {
		z = names.Normalize(z)
		if err := names.ValidHostname(z); err != nil {
			errs = append(errs, fmt.Errorf("zones: %w", err))
			continue
		}
		if !strings.Contains(z, ".") && !c.AllowTLDZones {
			errs = append(errs, fmt.Errorf("zones: %q is a whole top-level domain; use something like home.%s (or set LDS_ALLOW_TLD_ZONES=true)", z, z))
			continue
		}
		dup := false
		for _, have := range zones {
			dup = dup || have == z
		}
		if !dup {
			zones = append(zones, z)
		}
	}
	c.Zones = zones

	var ups []string
	for _, u := range c.DNSmasq.Upstreams {
		if err := validUpstream(u); err != nil {
			errs = append(errs, fmt.Errorf("upstreams: %w", err))
			continue
		}
		ups = append(ups, u)
	}
	c.DNSmasq.Upstreams = ups
	if len(ups) == 0 {
		errs = append(errs, errors.New("upstreams: at least one upstream DNS server is required"))
	}
	if c.DNSmasq.CacheSize < 0 || c.DNSmasq.CacheSize > 100000 {
		errs = append(errs, errors.New("cache size must be from 0 to 100000"))
	}
	if c.MaxRecords < 1 || c.MaxRecords > 1_000_000 {
		errs = append(errs, errors.New("record limit must be from 1 to 1000000"))
	}

	k := &c.Docker
	k.Enabled = k.Target != ""
	k.AllowedTargets = nil
	if k.Enabled {
		ip, err := names.ParseIP(k.Target)
		if err != nil {
			errs = append(errs, fmt.Errorf("Docker target: %w", err))
		} else {
			k.Target = ip.String()
			k.AllowedTargets = []string{k.Target}
		}
	}
	var extra []string
	for _, t := range k.ExtraTargets {
		ip, err := names.ParseIP(t)
		if err != nil {
			errs = append(errs, fmt.Errorf("allowed Docker targets: %w", err))
			continue
		}
		extra = append(extra, ip.String())
		k.AllowedTargets = append(k.AllowedTargets, ip.String())
	}
	k.ExtraTargets = extra

	c.DNSmasq.Zones = c.Zones
	k.Zones = c.Zones
	return errs
}

// Overrides are settings changed from the dashboard. A nil field means "use
// the value from the environment (docker-compose.yml) or the default".
type Overrides struct {
	Zones                *[]string `json:"zones,omitempty"`
	Upstreams            *[]string `json:"upstreams,omitempty"`
	DockerTarget         *string   `json:"docker_target,omitempty"`
	DockerAllowedTargets *[]string `json:"docker_allowed_targets,omitempty"`
	DNSSEC               *bool     `json:"dnssec,omitempty"`
	StopRebind           *bool     `json:"stop_dns_rebind,omitempty"`
	CacheSize            *int      `json:"cache_size,omitempty"`
	LogQueries           *bool     `json:"log_queries,omitempty"`
	MaxRecords           *int      `json:"max_records,omitempty"`
}

// WithOverrides returns c with the dashboard overrides applied and validated.
func (c Config) WithOverrides(o Overrides) (Config, error) {
	c.Zones = append([]string(nil), c.Zones...)
	c.DNSmasq.Upstreams = append([]string(nil), c.DNSmasq.Upstreams...)
	c.Docker.ExtraTargets = append([]string(nil), c.Docker.ExtraTargets...)
	if o.Zones != nil {
		c.Zones = append([]string(nil), *o.Zones...)
	}
	if o.Upstreams != nil {
		c.DNSmasq.Upstreams = append([]string(nil), *o.Upstreams...)
	}
	if o.DockerTarget != nil {
		c.Docker.Target = strings.TrimSpace(*o.DockerTarget)
	}
	if o.DockerAllowedTargets != nil {
		c.Docker.ExtraTargets = append([]string(nil), *o.DockerAllowedTargets...)
	}
	if o.DNSSEC != nil {
		c.DNSmasq.DNSSEC = *o.DNSSEC
	}
	if o.StopRebind != nil {
		c.DNSmasq.StopRebind = *o.StopRebind
	}
	if o.CacheSize != nil {
		c.DNSmasq.CacheSize = *o.CacheSize
	}
	if o.LogQueries != nil {
		c.DNSmasq.LogQueries = *o.LogQueries
	}
	if o.MaxRecords != nil {
		c.MaxRecords = *o.MaxRecords
	}
	return c, errors.Join(c.finish()...)
}

// validUpstream accepts "IP" or "IP#port".
func validUpstream(s string) error {
	host, port, hasPort := strings.Cut(s, "#")
	if _, err := names.ParseIP(host); err != nil {
		return err
	}
	if hasPort {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q: bad port", s)
		}
	}
	return nil
}

// IsLoopback reports whether a host (optionally with port) is a loopback address.
func IsLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envList(key, def string) []string {
	return strings.FieldsFunc(env(key, def), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

func envInt(key string, def, min, max int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min || n > max {
		return def, fmt.Errorf("%s must be a number from %d to %d", key, min, max)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s must be true or false", key)
	}
	return b, nil
}
