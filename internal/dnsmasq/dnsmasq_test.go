package dnsmasq

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yankluf/local-dns-server/internal/config"
	"github.com/yankluf/local-dns-server/internal/store"
)

func TestRender(t *testing.T) {
	var recs []store.Record
	for _, p := range [][2]string{{"*.home.lan", "10.0.0.5"}, {"nas.home.lan", "10.0.0.10"}, {"v6.home.lan", "fd00::1"}, {"files.home.lan", "nas.home.lan"}} {
		r, err := store.New(p[0], p[1], "note")
		if err != nil {
			t.Fatal(err)
		}
		r.Source = store.SourceManual
		recs = append(recs, r)
	}
	cfg := config.DNSmasq{Port: 5353, Upstreams: []string{"1.1.1.1"}, CacheSize: 100, Listen: []string{"127.0.0.1"}}
	conf := Render(cfg, recs)
	for _, want := range []string{
		"address=/home.lan/10.0.0.5\n",
		"host-record=nas.home.lan,10.0.0.10\n",
		"host-record=v6.home.lan,fd00::1\n",
		"cname=files.home.lan,nas.home.lan\n",
		"server=1.1.1.1\n",
		"listen-address=127.0.0.1\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config missing %q:\n%s", want, conf)
		}
	}

	bin, err := exec.LookPath("dnsmasq")
	if err != nil {
		t.Skip("dnsmasq not installed; skipping syntax check")
	}
	path := filepath.Join(t.TempDir(), "dnsmasq.conf")
	os.WriteFile(path, []byte(conf), 0o644)
	if out, err := exec.Command(bin, "--test", "--conf-file="+path).CombinedOutput(); err != nil {
		t.Fatalf("dnsmasq --test failed: %v\n%s", err, out)
	}
}
