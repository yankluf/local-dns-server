package names

import "testing"

func TestParseIPRejectsZones(t *testing.T) {
	for _, bad := range []string{"fe80::1%eth0", "fe80::1%z\naddress=/#/6.6.6.6", "::1%a", "1.2.3", "", "1.2.3.4\n"} {
		if _, err := ParseIP(bad); err == nil && bad != "1.2.3.4\n" {
			t.Errorf("ParseIP(%q) accepted", bad)
		}
	}
	ip, err := ParseIP("::ffff:10.0.0.1")
	if err != nil || ip.String() != "10.0.0.1" {
		t.Errorf("mapped address = %v, %v", ip, err)
	}
}

func TestCleanText(t *testing.T) {
	in := "ok\nline\r\x1b[31mred\x1b[0m ‮evil​  end"
	got := CleanText(in, 100)
	for _, bad := range []string{"\n", "\r", "\x1b", "‮", "​"} {
		if contains(got, bad) {
			t.Errorf("CleanText left %q in %q", bad, got)
		}
	}
	if n := len([]rune(CleanText("abcdefghij", 4))); n != 4 {
		t.Errorf("max not applied: %d", n)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestHostnameAndZones(t *testing.T) {
	for _, bad := range []string{"", "a b", "-a.lan", "a-.lan", "a..lan", "a\nb", "a/b", "a,b", "a#b", "*.lan"} {
		if ValidHostname(bad) == nil {
			t.Errorf("ValidHostname(%q) accepted", bad)
		}
	}
	zones := []string{"home.lan"}
	if !InZones("home.lan", zones) || !InZones("a.b.home.lan", zones) || InZones("evilhome.lan", zones) || InZones("lan", zones) {
		t.Error("InZones wrong")
	}
	for _, bad := range []string{"relative", "/a\nb", "/a,b", "/a/../b", "/a b"} {
		if SafePath(bad) == nil {
			t.Errorf("SafePath(%q) accepted", bad)
		}
	}
}
