package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var zones = []string{"home.lan"}

func open(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "records.json")
	if opts.Zones == nil {
		opts.Zones = zones
	}
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func mustNew(t *testing.T, name, value string) Record {
	t.Helper()
	r, err := New(name, value, "", zones)
	if err != nil {
		t.Fatalf("New(%q, %q): %v", name, value, err)
	}
	return r
}

func TestNewInfersType(t *testing.T) {
	cases := []struct{ name, value, typ, wantName, wantValue string }{
		{"*.Home.LAN.", "192.168.1.5", TypeWildcard, "*.home.lan", "192.168.1.5"},
		{"nas.home.lan", "192.168.1.10", TypeA, "nas.home.lan", "192.168.1.10"},
		{"nas.home.lan", "fd00::10", TypeAAAA, "nas.home.lan", "fd00::10"},
		{"nas.home.lan", "::ffff:10.0.0.1", TypeA, "nas.home.lan", "10.0.0.1"},
		{"grafana.home.lan", "Traefik.home.lan.", TypeCNAME, "grafana.home.lan", "traefik.home.lan"},
	}
	for _, c := range cases {
		r := mustNew(t, c.name, c.value)
		if r.Type != c.typ || r.Name != c.wantName || r.Value != c.wantValue {
			t.Errorf("New(%q, %q) = %s %s %s", c.name, c.value, r.Type, r.Name, r.Value)
		}
	}
}

// Finding 1 (config injection) and 3 (public domains).
func TestNewRejectsAttacks(t *testing.T) {
	bad := map[string][2]string{
		"IPv6 zone injection":      {"x.home.lan", "fe80::1%z\naddress=/#/6.6.6.6"},
		"zone injection, wildcard": {"*.home.lan", "fe80::1%z\nport=0"},
		"plain zone":               {"x.home.lan", "fe80::1%eth0"},
		"newline in name":          {"x.home.lan\naddress=/#/1.1.1.1", "1.2.3.4"},
		"TLD wildcard":             {"*.com", "6.6.6.6"},
		"public domain":            {"login.microsoftonline.com", "6.6.6.6"},
		"parent of zone":           {"*.lan", "6.6.6.6"},
		"lookalike zone":           {"x.evilhome.lan", "6.6.6.6"},
		"CNAME to outside":         {"x.home.lan", "example.com"},
		"CNAME to self":            {"x.home.lan", "x.home.lan"},
		"wildcard to hostname":     {"*.home.lan", "traefik.home.lan"},
		"comma in CNAME target":    {"x.home.lan", "a.home.lan,b"},
		"bare star":                {"*", "1.2.3.4"},
	}
	for name, in := range bad {
		if _, err := New(in[0], in[1], "", zones); err == nil {
			t.Errorf("%s: New(%q, %q) accepted", name, in[0], in[1])
		}
	}
}

func TestCommentsAreCleaned(t *testing.T) {
	r, err := New("a.home.lan", "1.2.3.4", "hi\n\x1b[2Jaddress=/#/1.1.1.1"+strings.Repeat("x", 500), zones)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(r.Comment, "\n\x1b") || len([]rune(r.Comment)) > MaxCommentLength {
		t.Errorf("comment not cleaned: %q", r.Comment)
	}
}

func TestPersistShadowAndRemove(t *testing.T) {
	s, path := open(t, Options{})
	changes := 0
	s.OnChange(func() { changes++ })
	if _, err := s.Add(mustNew(t, "nas.home.lan", "10.0.0.10")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(mustNew(t, "nas.home.lan", "10.0.0.11")); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	d1, d2 := mustNew(t, "nas.home.lan", "10.0.0.5"), mustNew(t, "app.home.lan", "10.0.0.5")
	d1.Container, d2.Container = "nas", "app"
	if ch, err := s.SetDocker([]Record{d1, d2}); !ch || err != nil {
		t.Fatalf("SetDocker: %v %v", ch, err)
	}
	if ch, _ := s.SetDocker([]Record{d2, d1}); ch {
		t.Fatal("same docker records reported as a change")
	}
	for _, v := range s.List() {
		if v.Name == "nas.home.lan" && v.Source == SourceDocker && v.Active {
			t.Error("docker record not shadowed")
		}
	}
	if _, err := s.Remove("app.home.lan"); err == nil {
		t.Error("removing a docker record should fail")
	}
	if _, err := s.Remove("NAS.home.lan"); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, Options{Zones: zones})
	if err != nil || len(s2.List()) != 0 {
		t.Fatalf("reopen: %v %d", err, len(s2.List()))
	}
	if changes != 3 {
		t.Errorf("changes = %d, want 3", changes)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("records.json mode = %v, want 0600", st.Mode().Perm())
	}
}

// Finding 5: a CNAME loop must be rejected before it's saved.
func TestCNAMELoopRejected(t *testing.T) {
	s, path := open(t, Options{})
	if _, err := s.Add(mustNew(t, "a.home.lan", "b.home.lan")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Add(mustNew(t, "b.home.lan", "a.home.lan"))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("loop accepted: %v", err)
	}
	s2, _ := Open(path, Options{Zones: zones})
	if len(s2.List()) != 1 {
		t.Error("rejected record was saved")
	}
	// Long chains too.
	prev := "a.home.lan"
	for i := 0; i < MaxCNAMEChain+2; i++ {
		name := "c" + string(rune('a'+i)) + ".home.lan"
		if _, err := s.Add(mustNew(t, name, prev)); err != nil {
			if !strings.Contains(err.Error(), "longer") {
				t.Fatalf("unexpected: %v", err)
			}
			return
		}
		prev = name
	}
	t.Error("overlong CNAME chain accepted")
}

// Finding 5: the Validate hook (dnsmasq --test) can veto a change.
func TestValidateHookVetoes(t *testing.T) {
	s, path := open(t, Options{Validate: func(a []Record) error {
		for _, r := range a {
			if r.Name == "bad.home.lan" {
				return errors.New("nope")
			}
		}
		return nil
	}})
	if _, err := s.Add(mustNew(t, "bad.home.lan", "1.2.3.4")); err == nil {
		t.Fatal("vetoed record accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("vetoed record was written to disk")
	}
}

// Finding 9: record limit.
func TestMaxRecords(t *testing.T) {
	s, _ := open(t, Options{MaxRecords: 2})
	s.Add(mustNew(t, "a.home.lan", "1.1.1.1"))
	s.Add(mustNew(t, "b.home.lan", "1.1.1.1"))
	if _, err := s.Add(mustNew(t, "c.home.lan", "1.1.1.1")); !errors.Is(err, ErrFull) {
		t.Errorf("limit not enforced: %v", err)
	}
}

// Finding 10: a damaged records file falls back to the backup.
func TestBackupRecovery(t *testing.T) {
	s, path := open(t, Options{})
	s.Add(mustNew(t, "a.home.lan", "1.1.1.1"))
	s.Add(mustNew(t, "b.home.lan", "1.1.1.1")) // a.home.lan alone is now in .bak
	os.WriteFile(path, []byte("{truncated"), 0o600)
	s2, err := Open(path, Options{Zones: zones})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s2.List()); n != 1 {
		t.Errorf("recovered %d records from backup, want 1", n)
	}
	os.WriteFile(path+".bak", []byte("{also bad"), 0o600)
	if _, err := Open(path, Options{Zones: zones}); err == nil {
		t.Error("both files damaged should be an error, not silent data loss")
	}
}

// Finding 4: Docker records can only be plain A/AAAA inside the zones.
func TestDockerRecordsRestricted(t *testing.T) {
	s, _ := open(t, Options{})
	w := Record{Type: TypeWildcard, Name: "*.home.lan", Value: "6.6.6.6"}
	out := Record{Type: TypeA, Name: "github.com", Value: "6.6.6.6"}
	inj := Record{Type: TypeAAAA, Name: "x.home.lan", Value: "fe80::1%z\naddress=/#/6.6.6.6"}
	ok := Record{Type: TypeA, Name: "ok.home.lan", Value: "10.0.0.5", Container: "c\x1b[2J"}
	s.SetDocker([]Record{w, out, inj, ok})
	views := s.List()
	if len(views) != 1 || views[0].Name != "ok.home.lan" {
		t.Fatalf("unexpected docker records: %+v", views)
	}
	if strings.Contains(views[0].Container, "\x1b") {
		t.Error("container name not cleaned")
	}
}

// Records saved before a zone change (or edited on disk) are not served.
func TestStoredRecordsRevalidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	os.WriteFile(path, []byte(`{"records":[
	  {"id":"1","type":"A","name":"old.other.lan","value":"1.2.3.4"},
	  {"id":"2","type":"AAAA","name":"x.home.lan","value":"fe80::1%z\naddress=/#/6.6.6.6"},
	  {"id":"3","type":"A","name":"ok.home.lan","value":"1.2.3.4"}]}`), 0o600)
	s, err := Open(path, Options{Zones: zones})
	if err != nil {
		t.Fatal(err)
	}
	active := s.Active()
	if len(active) != 1 || active[0].Name != "ok.home.lan" {
		t.Errorf("active = %+v", active)
	}
}

func TestReadOnlyReplica(t *testing.T) {
	s, _ := open(t, Options{ReadOnly: true})
	if _, err := s.Add(mustNew(t, "a.home.lan", "1.1.1.1")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("replica accepted a change: %v", err)
	}
	if ch, err := s.SetReplica([]Record{mustNew(t, "a.home.lan", "1.1.1.1"), {Type: TypeA, Name: "evil.com", Value: "6.6.6.6"}}); !ch || err != nil {
		t.Fatalf("SetReplica: %v %v", ch, err)
	}
	if v := s.List(); len(v) != 1 || v[0].Source != SourceReplica {
		t.Errorf("replica records = %+v", v)
	}
}

func TestIDs(t *testing.T) {
	s, _ := open(t, Options{})
	r, _ := s.Add(mustNew(t, "a.home.lan", "1.1.1.1"))
	if len(r.ID) != 16 {
		t.Errorf("ID %q should be 64 bits", r.ID)
	}
}

// A loop written straight into the file is switched off, not served.
func TestLoopOnDiskNotServed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.json")
	os.WriteFile(path, []byte(`{"records":[
	  {"id":"1","type":"CNAME","name":"l1.home.lan","value":"l2.home.lan"},
	  {"id":"2","type":"CNAME","name":"l2.home.lan","value":"l1.home.lan"},
	  {"id":"3","type":"A","name":"ok.home.lan","value":"1.2.3.4"}]}`), 0o600)
	s, err := Open(path, Options{Zones: zones})
	if err != nil {
		t.Fatal(err)
	}
	if a := s.Active(); len(a) != 1 || a[0].Name != "ok.home.lan" {
		t.Errorf("active = %+v", a)
	}
	// Unrelated changes still work, and so does removing a loop member.
	if _, err := s.Add(mustNew(t, "new.home.lan", "1.2.3.5")); err != nil {
		t.Errorf("unrelated add blocked by the old loop: %v", err)
	}
	if _, err := s.Remove("l1.home.lan"); err != nil {
		t.Errorf("can't remove loop member: %v", err)
	}
}
