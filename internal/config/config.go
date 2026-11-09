// Package config reads the server settings from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds the settings for `localdns serve`.
type Config struct {
	HTTPListen string // LOCALDNS_HTTP_LISTEN
	DataDir    string // LOCALDNS_DATA_DIR
	Token      string // LOCALDNS_TOKEN: if set, required for changes

	DNSmasq DNSmasq
	Docker  Docker
}

// DNSmasq configures the managed dnsmasq process.
type DNSmasq struct {
	Enabled    bool     // LOCALDNS_DNSMASQ_ENABLED
	Binary     string   // LOCALDNS_DNSMASQ_BIN
	Port       int      // LOCALDNS_DNS_PORT
	Listen     []string // LOCALDNS_DNS_LISTEN: listen addresses; empty = all
	Upstreams  []string // LOCALDNS_UPSTREAMS
	CacheSize  int      // LOCALDNS_CACHE_SIZE
	LogQueries bool     // LOCALDNS_LOG_QUERIES
	ExtraDir   string   // LOCALDNS_DNSMASQ_EXTRA_DIR: included with conf-dir if it exists
	ConfigPath string   // generated config; defaults to <data>/dnsmasq.conf
}

// Docker configures container discovery.
type Docker struct {
	Enabled bool   // true when Target is set
	Host    string // DOCKER_HOST
	Target  string // LOCALDNS_DOCKER_TARGET: IP that Traefik listens on
}

// FromEnv builds the config from environment variables.
func FromEnv() (Config, error) {
	c := Config{
		HTTPListen: env("LOCALDNS_HTTP_LISTEN", ":5380"),
		DataDir:    env("LOCALDNS_DATA_DIR", "/data"),
		Token:      os.Getenv("LOCALDNS_TOKEN"),
	}
	var err error
	if c.DNSmasq.Enabled, err = envBool("LOCALDNS_DNSMASQ_ENABLED", true); err != nil {
		return c, err
	}
	c.DNSmasq.Binary = env("LOCALDNS_DNSMASQ_BIN", "dnsmasq")
	if c.DNSmasq.Port, err = envInt("LOCALDNS_DNS_PORT", 53); err != nil {
		return c, err
	}
	c.DNSmasq.Listen = envList("LOCALDNS_DNS_LISTEN", "")
	c.DNSmasq.Upstreams = envList("LOCALDNS_UPSTREAMS", "1.1.1.1,1.0.0.1,9.9.9.9")
	if c.DNSmasq.CacheSize, err = envInt("LOCALDNS_CACHE_SIZE", 10000); err != nil {
		return c, err
	}
	if c.DNSmasq.LogQueries, err = envBool("LOCALDNS_LOG_QUERIES", false); err != nil {
		return c, err
	}
	c.DNSmasq.ExtraDir = env("LOCALDNS_DNSMASQ_EXTRA_DIR", "/etc/dnsmasq.d")
	c.DNSmasq.ConfigPath = env("LOCALDNS_DNSMASQ_CONFIG", strings.TrimSuffix(c.DataDir, "/")+"/dnsmasq.conf")

	c.Docker.Host = env("DOCKER_HOST", "unix:///var/run/docker.sock")
	c.Docker.Target = strings.TrimSpace(os.Getenv("LOCALDNS_DOCKER_TARGET"))
	c.Docker.Enabled = c.Docker.Target != ""
	return c, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envList(key, def string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(env(key, def), func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, p)
	}
	return out
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}
