package store

import (
	"path/filepath"
	"testing"
)

func TestNewInfersType(t *testing.T) {
	cases := []struct{ name, value, typ, wantName, wantValue string }{
		{"*.Home.LAN.", "192.168.1.5", TypeWildcard, "*.home.lan", "192.168.1.5"},
		{"nas.home.lan", "192.168.1.10", TypeA, "nas.home.lan", "192.168.1.10"},
		{"nas.home.lan", "fd00::10", TypeAAAA, "nas.home.lan", "fd00::10"},
		{"grafana.home.lan", "Traefik.home.lan.", TypeCNAME, "grafana.home.lan", "traefik.home.lan"},
	}
	for _, c := range cases {
		r, err := New(c.name, c.value, "")
		if err != nil {
			t.Fatalf("New(%q, %q): %v", c.name, c.value, err)
		}
		if r.Type != c.typ || r.Name != c.wantName || r.Value != c.wantValue {
			t.Errorf("New(%q, %q) = %s %s %s, want %s %s %s", c.name, c.value, r.Type, r.Name, r.Value, c.typ, c.wantName, c.wantValue)
		}
	}
	for _, bad := range [][2]string{
		{"*.home.lan", "traefik.home.lan"}, // wildcard needs an IP
		{"bad name", "1.2.3.4"},
		{"-x.lan", "1.2.3.4"},
		{"a.lan", "a.lan"},
		{"", "1.2.3.4"},
	} {
		if _, err := New(bad[0], bad[1], ""); err == nil {
			t.Errorf("New(%q, %q) succeeded, want error", bad[0], bad[1])
		}
	}
}

func TestStorePersistsAndShadows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	s, _ := Open(path)
	changes := 0
	s.OnChange(func() { changes++ })

	a, _ := New("nas.home.lan", "10.0.0.10", "the NAS")
	if _, err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(a); err == nil {
		t.Fatal("duplicate name accepted")
	}
	c, _ := New("files.home.lan", "nowhere.example.com", "")
	s.Add(c)

	d1, _ := New("nas.home.lan", "10.0.0.5", "")
	d2, _ := New("app.home.lan", "10.0.0.5", "")
	d1.Container, d2.Container = "nas", "app"
	if !s.SetDocker([]Record{d1, d2}) {
		t.Fatal("SetDocker reported no change")
	}
	if s.SetDocker([]Record{d2, d1}) {
		t.Fatal("same docker records reported as a change")
	}

	views := map[string][]View{}
	for _, v := range s.List() {
		views[v.Name] = append(views[v.Name], v)
	}
	for _, v := range views["nas.home.lan"] {
		if v.Source == SourceDocker && v.Active {
			t.Error("docker record not shadowed by manual record")
		}
	}
	if views["files.home.lan"][0].Warning == "" {
		t.Error("expected warning for CNAME to a non-local target")
	}
	if n := len(s.Active()); n != 3 {
		t.Errorf("Active() = %d records, want 3", n)
	}

	if _, err := s.Remove("app.home.lan"); err == nil {
		t.Error("removing a docker record should fail")
	}
	if _, err := s.Remove("NAS.home.lan"); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s2.List()); got != 1 {
		t.Errorf("reopened store has %d records, want 1", got)
	}
	if changes != 4 {
		t.Errorf("got %d change callbacks, want 4", changes)
	}
}
