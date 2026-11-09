package docker

import (
	"reflect"
	"testing"
)

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

func TestRecordsFromContainers(t *testing.T) {
	cs := []Container{
		{Names: []string{"/grafana"}, Labels: map[string]string{
			"traefik.http.routers.grafana.rule": "Host(`grafana.home.lan`)",
		}},
		{Names: []string{"/off"}, Labels: map[string]string{
			"traefik.enable":                "false",
			"traefik.http.routers.off.rule": "Host(`off.home.lan`)",
		}},
		{Names: []string{"/skipme"}, Labels: map[string]string{
			"lds.enable":                       "false",
			"traefik.http.routers.skipme.rule": "Host(`skip.home.lan`)",
		}},
		{Names: []string{"/other"}, Labels: map[string]string{
			"traefik.http.routers.o.rule": "Host(`other.home.lan`) || Host(`grafana.home.lan`)",
			"lds.target":                  "10.0.0.9",
			"lds.hosts":                   "extra.home.lan, ",
		}},
	}
	got := map[string]string{}
	for _, r := range RecordsFromContainers(cs, "10.0.0.5") {
		got[r.Name] = r.Value + "@" + r.Container
	}
	want := map[string]string{
		"grafana.home.lan": "10.0.0.5@grafana",
		"other.home.lan":   "10.0.0.9@other",
		"extra.home.lan":   "10.0.0.9@other",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
