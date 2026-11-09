package config

import (
	"strings"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	base := map[string]string{"LDS_TOKEN": "0123456789abcdef0123", "LDS_ZONES": "home.lan"}
	for k, v := range kv {
		base[k] = v
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
}

func TestValidConfig(t *testing.T) {
	setEnv(t, nil)
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !c.DNSmasq.DNSSEC || !c.DNSmasq.StopRebind || c.Zones[0] != "home.lan" {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]map[string]string{
		"token too short":        {"LDS_TOKEN": "short"},
		"whole TLD zone":         {"LDS_ZONES": "com"},
		"bad zone":               {"LDS_ZONES": "home lan\nx"},
		"upstream injection":     {"LDS_UPSTREAMS": "1.1.1.1\nconf-file=/etc/shadow"},
		"upstream domain form":   {"LDS_UPSTREAMS": "/corp/10.0.0.1"},
		"upstream zone":          {"LDS_UPSTREAMS": "fe80::1%eth0"},
		"listen not an IP":       {"LDS_DNS_LISTEN": "eth0"},
		"unsafe extra dir":       {"LDS_DNSMASQ_EXTRA_DIR": "/etc/dnsmasq.d\nconf-file=/x"},
		"unsafe data dir":        {"LDS_DATA_DIR": "/data,x"},
		"docker target zone":     {"LDS_DOCKER_TARGET": "fe80::1%x"},
		"half TLS":               {"LDS_TLS_CERT": "/c.pem"},
		"bad port":               {"LDS_DNS_PORT": "99999"},
		"replica without scheme": {"LDS_REPLICA_OF": "dns1:5380"},
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			setEnv(t, kv)
			if _, err := FromEnv(); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestInsecureNoAuth(t *testing.T) {
	setEnv(t, map[string]string{"LDS_TOKEN": "", "LDS_INSECURE_NO_AUTH": "true"})
	if _, err := FromEnv(); err != nil {
		t.Fatal(err)
	}
}

// Zero config: no token and no zones still starts.
func TestZeroConfig(t *testing.T) {
	setEnv(t, map[string]string{"LDS_TOKEN": "", "LDS_ZONES": ""})
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !c.TokenAuto || len(c.Zones) != 0 {
		t.Errorf("TokenAuto=%v zones=%v", c.TokenAuto, c.Zones)
	}
	setEnv(t, map[string]string{"LDS_TOKEN": "", "LDS_REPLICA_OF": "http://dns1:5380"})
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "primary's token") {
		t.Errorf("a replica needs an explicit token: %v", err)
	}
}

func TestWithOverrides(t *testing.T) {
	setEnv(t, map[string]string{"LDS_DOCKER_TARGET": "10.0.0.5"})
	base, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	zones, ups, empty, off := []string{"Lab.Example.", "lab.example"}, []string{"9.9.9.9#53"}, "", false
	c, err := base.WithOverrides(Overrides{Zones: &zones, Upstreams: &ups, DockerTarget: &empty, DNSSEC: &off})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Zones) != 1 || c.Zones[0] != "lab.example" || c.DNSmasq.Zones[0] != "lab.example" || c.Docker.Zones[0] != "lab.example" {
		t.Errorf("zones not normalized/propagated: %v", c.Zones)
	}
	if c.Docker.Enabled || c.DNSmasq.DNSSEC || c.DNSmasq.Upstreams[0] != "9.9.9.9#53" {
		t.Errorf("overrides not applied: %+v", c)
	}
	if base.Zones[0] != "home.lan" || !base.Docker.Enabled {
		t.Error("WithOverrides changed the base config")
	}
	for name, o := range map[string]Overrides{
		"TLD":          {Zones: &[]string{"com"}},
		"injection":    {Upstreams: &[]string{"1.1.1.1\nport=0"}},
		"no upstreams": {Upstreams: &[]string{}},
		"bad target":   {DockerTarget: ptr("fe80::1%x")},
		"cache":        {CacheSize: ptr(-5)},
		"limit":        {MaxRecords: ptr(0)},
	} {
		if _, err := base.WithOverrides(o); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func ptr[T any](v T) *T { return &v }
