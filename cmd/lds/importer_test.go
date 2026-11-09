package main

import (
	"reflect"
	"testing"
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
