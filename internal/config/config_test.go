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
		"token required":         {"LDS_TOKEN": ""},
		"token too short":        {"LDS_TOKEN": "short"},
		"zones required":         {"LDS_ZONES": ""},
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

func TestErrorMentionsFix(t *testing.T) {
	setEnv(t, map[string]string{"LDS_TOKEN": ""})
	_, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "openssl rand") {
		t.Errorf("error should explain how to make a token: %v", err)
	}
}
