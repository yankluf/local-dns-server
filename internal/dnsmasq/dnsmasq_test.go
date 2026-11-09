package dnsmasq

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yankluf/local-dns-server/internal/config"
	"github.com/yankluf/local-dns-server/internal/store"
)

var zones = []string{"home.lan"}

func recs(t *testing.T, pairs ...string) []store.Record {
	t.Helper()
	var out []store.Record
	for i := 0; i < len(pairs); i += 2 {
		r, err := store.New(pairs[i], pairs[i+1], "note\nwith newline", zones)
		if err != nil {
			t.Fatal(err)
		}
		r.Source = store.SourceManual
		out = append(out, r)
	}
	return out
}

func testCfg(t *testing.T) config.DNSmasq {
	dir := t.TempDir()
	// dnsmasq drops to an unprivileged user that must be able to read hosts.d.
	for d := dir; d != "/" && d != os.TempDir(); d = filepath.Dir(d) {
		os.Chmod(d, 0o755)
	}
	return config.DNSmasq{
		Enabled: true, Binary: "dnsmasq", Port: freePort(t), Listen: []string{"127.0.0.1"},
		Upstreams: []string{"127.0.0.1#1"}, CacheSize: 100, Zones: zones, StopRebind: true, DNSSEC: true,
		ConfigPath: filepath.Join(dir, "dnsmasq.conf"), HostsDir: filepath.Join(dir, "hosts.d"),
		RestartSpacing: 300 * time.Millisecond,
	}
}

func freePort(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestRender(t *testing.T) {
	cfg := testCfg(t)
	cfg.Listen = nil
	f, skipped := Render(cfg, recs(t, "*.home.lan", "10.0.0.5", "nas.home.lan", "10.0.0.10", "v6.home.lan", "fd00::1", "files.home.lan", "nas.home.lan"))
	if len(skipped) != 0 {
		t.Fatal(skipped)
	}
	for _, want := range []string{
		"address=/home.lan/10.0.0.5\n", "cname=files.home.lan,nas.home.lan\n", "local=/home.lan/\n",
		"local-service\n", "dnssec\n", "trust-anchor=.,38696,", "stop-dns-rebind\n", "rebind-domain-ok=/home.lan/\n",
		"hostsdir=" + cfg.HostsDir + "\n",
	} {
		if !strings.Contains(f.Conf, want) {
			t.Errorf("config missing %q", want)
		}
	}
	for _, want := range []string{"10.0.0.10 nas.home.lan\n", "fd00::1 v6.home.lan\n"} {
		if !strings.Contains(f.Hosts, want) {
			t.Errorf("hosts missing %q", want)
		}
	}
	if strings.Contains(f.Conf+f.Hosts, "note\nwith") || !strings.Contains(f.Hosts, "# manual, note with newline\n10.0.0.10 nas.home.lan\n") {
		t.Error("comment newline leaked")
	}
	if _, err := exec.LookPath("dnsmasq"); err == nil {
		if err := NewManager(cfg).Validate(f); err != nil {
			t.Fatalf("dnsmasq --test: %v", err)
		}
	}
}

// Finding 1, last line of defense: a tampered record that skipped
// validation still can't add lines to the config.
func TestRenderRefusesTamperedRecords(t *testing.T) {
	bad := []store.Record{
		{Type: store.TypeAAAA, Name: "x.home.lan", Value: "fe80::1%z\naddress=/#/6.6.6.6"},
		{Type: store.TypeA, Name: "x.home.lan\nport=0", Value: "1.2.3.4"},
		{Type: store.TypeCNAME, Name: "y.home.lan", Value: "a.home.lan\nconf-file=/etc/shadow"},
		{Type: store.TypeWildcard, Name: "*.com", Value: "6.6.6.6"},
		{Type: store.TypeA, Name: "github.com", Value: "6.6.6.6"},
		{Type: "TXT", Name: "t.home.lan", Value: "x"},
	}
	f, skipped := Render(testCfg(t), bad)
	if len(skipped) != len(bad) {
		t.Errorf("skipped %d of %d bad records", len(skipped), len(bad))
	}
	for _, s := range []string{"6.6.6.6", "port=0", "shadow", "fe80"} {
		if strings.Contains(f.Conf+f.Hosts, s) {
			t.Errorf("%q reached the config", s)
		}
	}
}

func lookup(cfg config.DNSmasq, name string) []string {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort("127.0.0.1", itoa(cfg.Port)))
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	addrs, _ := r.LookupHost(ctx, name)
	return addrs
}

func itoa(n int) string { return strconv.Itoa(n) }
func waitFor(t *testing.T, desc string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Findings 5 and 8 against a real dnsmasq.
func TestManagerLive(t *testing.T) {
	if _, err := exec.LookPath("dnsmasq"); err != nil {
		t.Skip("dnsmasq not installed")
	}
	cfg := testCfg(t)
	cfg.DNSSEC = false // no upstream in the sandbox
	m := NewManager(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); time.Sleep(100 * time.Millisecond) }()
	go m.Run(ctx)

	render := func(r []store.Record) Files { f, _ := Render(cfg, r); return f }
	m.Start(render(recs(t, "*.home.lan", "10.0.0.5", "nas.home.lan", "10.0.0.10")), render(nil))
	waitFor(t, "first answer", func() bool { return has(lookup(cfg, "nas.home.lan"), "10.0.0.10") })
	if err := m.Probe(context.Background()); err != nil {
		t.Errorf("probe: %v", err)
	}
	restarts := m.Status().LastRestart

	// Host-only change: picked up live, no restart.
	if err := m.Apply(render(recs(t, "*.home.lan", "10.0.0.5", "nas.home.lan", "10.0.0.10", "new.home.lan", "10.0.0.11"))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hot-reloaded host", func() bool { return has(lookup(cfg, "new.home.lan"), "10.0.0.11") })
	if !m.Status().LastRestart.Equal(restarts) {
		t.Error("a host-only change restarted dnsmasq")
	}

	// Config change: restart, but spaced out.
	start := time.Now()
	m.Apply(render(recs(t, "*.home.lan", "10.0.0.6")))
	m.Apply(render(recs(t, "*.home.lan", "10.0.0.7")))
	if time.Since(start) < cfg.RestartSpacing {
		t.Error("restarts not spaced out")
	}
	waitFor(t, "new wildcard", func() bool { return has(lookup(cfg, "zz.home.lan"), "10.0.0.7") })

	// A config dnsmasq rejects is never applied; the old one keeps serving.
	bad := render(recs(t, "*.home.lan", "10.0.0.8"))
	bad.Conf += "cname=a.home.lan,b.home.lan\ncname=b.home.lan,a.home.lan\n"
	if err := m.Apply(bad); err == nil {
		t.Error("rejected config applied")
	}
	if !has(lookup(cfg, "zz.home.lan"), "10.0.0.7") {
		t.Error("old config stopped serving after a rejected change")
	}
}

// Finding 5: if the records can't be applied at startup, dnsmasq still
// starts, from the last working config or with no records.
func TestStartFallsBack(t *testing.T) {
	if _, err := exec.LookPath("dnsmasq"); err != nil {
		t.Skip("dnsmasq not installed")
	}
	cfg := testCfg(t)
	cfg.DNSSEC = false
	render := func(r []store.Record) Files { f, _ := Render(cfg, r); return f }
	poisoned := render(recs(t, "ok.home.lan", "10.0.0.1"))
	poisoned.Conf += "cname=a.home.lan,b.home.lan\ncname=b.home.lan,a.home.lan\n"

	// No saved config: start with no records, forwarding still works.
	m := NewManager(cfg)
	m.Start(poisoned, render(nil))
	if !m.Status().Running || m.Status().Degraded == "" {
		t.Fatalf("not running degraded: %+v", m.Status())
	}
	if err := m.Probe(context.Background()); err != nil {
		t.Errorf("probe: %v", err)
	}
	// Apply a good config so it's saved as last-known-good, then restart
	// with poison: the good config comes back.
	if err := m.Apply(render(recs(t, "good.home.lan", "10.0.0.2"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx) // stops dnsmasq

	m2 := NewManager(cfg)
	m2.Start(poisoned, render(nil))
	defer func() { ctx, c := context.WithCancel(context.Background()); c(); m2.Run(ctx) }()
	waitFor(t, "last good config", func() bool { return has(lookup(cfg, "good.home.lan"), "10.0.0.2") })
	if m2.Status().Degraded == "" {
		t.Error("fallback not reported")
	}
}
