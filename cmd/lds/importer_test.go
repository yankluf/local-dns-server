package main

import (
	"reflect"
	"testing"

	"github.com/yankluf/local-dns-server/internal/store"
)

func TestParseImportCustomList(t *testing.T) {
	in := `# Pi-hole v5 custom.list
192.168.1.10 nas.lan
192.168.1.11 printer.lan scanner.lan
cname=files.lan,nas.lan
cname=a.lan,b.lan,nas.lan,300
host-record=router.lan,192.168.1.1
address=/home.lan/192.168.1.5
garbage line
`
	want := [][2]string{
		{"nas.lan", "192.168.1.10"},
		{"printer.lan", "192.168.1.11"},
		{"scanner.lan", "192.168.1.11"},
		{"router.lan", "192.168.1.1"},
		{"*.home.lan", "192.168.1.5"},
		{"files.lan", "nas.lan"},
		{"a.lan", "nas.lan"},
		{"b.lan", "nas.lan"},
	}
	if got := ParseImport(in, false); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

func TestParseImportPiholeTOML(t *testing.T) {
	in := `[dns]
  upstreams = [ "8.8.8.8" ]
  hosts = [
    "192.168.1.10 nas.lan",
    "192.168.1.20 tv.lan"
  ] ### CHANGED
  cnameRecords = [ "files.lan,nas.lan", "x.lan,tv.lan,60" ]
`
	want := [][2]string{
		{"nas.lan", "192.168.1.10"},
		{"tv.lan", "192.168.1.20"},
		{"files.lan", "nas.lan"},
		{"x.lan", "tv.lan"},
	}
	if got := ParseImport(in, true); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

// An export must import back to the same records, comments included.
func TestExportRoundTrip(t *testing.T) {
	view := func(typ, name, value, comment, source string) store.View {
		return store.View{Record: store.Record{Type: typ, Name: name, Value: value, Comment: comment, Source: source}, Active: true}
	}
	records := []store.View{
		view(store.TypeCNAME, "files.home.lan", "nas.home.lan", "", store.SourceManual),
		view(store.TypeA, "nas.home.lan", "10.0.0.10", "the NAS # in the closet", store.SourceManual),
		view(store.TypeAAAA, "nas.home.lan", "fd00::10", "", store.SourceManual),
		view(store.TypeWildcard, "*.home.lan", "10.0.0.5", "Traefik", store.SourceManual),
		view(store.TypeA, "app.home.lan", "10.0.0.5", "", store.SourceDocker),
		view(store.TypeA, "evil.home.lan\naddress=/#/6.6.6.6", "10.0.0.5", "", store.SourceManual),
	}
	want := [][3]string{
		{"nas.home.lan", "10.0.0.10", "the NAS # in the closet"},
		{"nas.home.lan", "fd00::10", ""},
		{"*.home.lan", "10.0.0.5", "Traefik"},
		{"files.home.lan", "nas.home.lan", ""},
	}
	if got := parseImport(exportText(records, false), false); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v\nfile:\n%s", got, want, exportText(records, false))
	}
	if got := parseImport(exportText(records, true), false); len(got) != len(want)+1 {
		t.Errorf("--all: got %v", got)
	}
}
