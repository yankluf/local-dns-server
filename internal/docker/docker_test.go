package docker

import (
	"reflect"
	"testing"

	"github.com/yankluf/local-dns-server/internal/config"
)

var cfg = config.Docker{Target: "10.0.0.5", AllowedTargets: []string{"10.0.0.5", "10.0.0.9"}, Zones: []string{"home.lan"}}

func TestHostsFromRule(t *testing.T) {
	cases := map[string][]string{
		"Host(`app.home.lan`)":                                   {"app.home.lan"},
		"Host(`a.home.lan`) || Host(`B.home.lan`)":               {"a.home.lan", "b.home.lan"},
		"Host(`a.home.lan`, `b.home.lan`) && PathPrefix(`/api`)": {"a.home.lan", "b.home.lan"},
		"HostRegexp(`{sub:[a-z]+}.home.lan`)":                    nil,
		"HostSNI(`db.home.lan`)":                                 {"db.home.lan"},
		"HostSNI(`*`)":                                           nil,
		`Host("quoted.home.lan")`:                                {"quoted.home.lan"},
		"PathPrefix(`/x`)":                                       nil,
	}
	for rule, want := range cases {
		if got := HostsFromRule(rule); !reflect.DeepEqual(got, want) {
			t.Errorf("HostsFromRule(%q) = %v, want %v", rule, got, want)
		}
	}
}

func records(cs []Container) map[string]string {
	got := map[string]string{}
	for _, r := range RecordsFromContainers(cs, cfg) {
		got[r.Name] = r.Type + " " + r.Value + "@" + r.Container
	}
	return got
}

func TestRecordsFromContainers(t *testing.T) {
	got := records([]Container{
		{Names: []string{"/grafana"}, Labels: map[string]string{"traefik.http.routers.grafana.rule": "Host(`grafana.home.lan`)"}},
		{Names: []string{"/off"}, Labels: map[string]string{"traefik.enable": "false", "traefik.http.routers.off.rule": "Host(`off.home.lan`)"}},
		{Names: []string{"/skipme"}, Labels: map[string]string{"lds.enable": "false", "traefik.http.routers.s.rule": "Host(`skip.home.lan`)"}},
		{Names: []string{"/other"}, Labels: map[string]string{
			"traefik.http.routers.o.rule": "Host(`other.home.lan`) || Host(`grafana.home.lan`)",
			"lds.target":                  "10.0.0.9",
			"lds.hosts":                   "extra.home.lan, ",
		}},
	})
	want := map[string]string{
		"grafana.home.lan": "A 10.0.0.5@grafana",
		"other.home.lan":   "A 10.0.0.9@other",
		"extra.home.lan":   "A 10.0.0.9@other",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Finding 4: labels baked into an image can't hijack DNS.
func TestMaliciousLabels(t *testing.T) {
	got := records([]Container{
		{Names: []string{"/innocent-image"}, Labels: map[string]string{
			"lds.hosts":                   "*.com, *.home.lan, github.com",
			"traefik.http.routers.x.rule": "Host(`login.microsoftonline.com`) || Host(`ok.home.lan`)",
			"lds.target":                  "6.6.6.6", // not an allowed target
		}},
		{Names: []string{"/inject"}, Labels: map[string]string{
			"lds.hosts":  "y.home.lan",
			"lds.target": "fe80::1%z\naddress=/#/6.6.6.6",
		}},
	})
	want := map[string]string{
		"ok.home.lan": "A 10.0.0.5@innocent-image",
		"y.home.lan":  "A 10.0.0.5@inject",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Finding 21: plain TCP to a remote Docker host is refused.
func TestInsecureDockerHostRefused(t *testing.T) {
	c := cfg
	c.Host = "tcp://192.168.1.50:2375"
	if _, err := NewWatcher(c, nil); err == nil {
		t.Error("unencrypted remote DOCKER_HOST accepted")
	}
	c.Host = "tcp://127.0.0.1:2375" // the socket proxy on this machine
	if _, err := NewWatcher(c, nil); err != nil {
		t.Errorf("loopback socket proxy refused: %v", err)
	}
	c.Host, c.TLSVerify = "tcp://192.168.1.50:2376", true
	if _, err := NewWatcher(c, nil); err == nil {
		t.Error("TLS without certificates accepted")
	}
}
