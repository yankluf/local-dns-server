// Package names holds the strict validators shared by every part of LDS that
// turns outside input (API requests, Docker labels, settings) into dnsmasq
// configuration. Anything that reaches a config file must pass through here.
package names

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
)

var (
	labelRe = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)
	pathRe  = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)
)

// Normalize lowercases a name and drops surrounding space and a trailing dot.
func Normalize(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// ValidHostname reports whether name (already normalized) is a plain DNS
// hostname: dot-separated labels of [a-z0-9_-], not starting or ending in '-'.
func ValidHostname(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}
	if len(name) > 253 {
		return fmt.Errorf("%q is too long", name)
	}
	for _, label := range strings.Split(name, ".") {
		if !labelRe.MatchString(label) {
			return fmt.Errorf("%q is not a valid hostname", name)
		}
	}
	return nil
}

// InZones reports whether name is one of zones or a subdomain of one.
func InZones(name string, zones []string) bool {
	for _, z := range zones {
		if name == z || strings.HasSuffix(name, "."+z) {
			return true
		}
	}
	return false
}

// ParseIP parses a plain IPv4 or IPv6 address. Unlike netip.ParseAddr it
// rejects IPv6 zones ("fe80::1%eth0"), whose text is unrestricted and could
// otherwise carry newlines into a config file. IPv4-mapped IPv6 addresses
// are returned as IPv4.
func ParseIP(s string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%q is not an IP address", s)
	}
	if ip.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("%q: IPv6 zones are not allowed", s)
	}
	return ip.Unmap(), nil
}

// CleanText removes control and invisible formatting characters (newlines,
// terminal escape sequences, bidirectional overrides) and limits the result
// to max characters. Use it for any free text shown in terminals, logs, the
// web UI or config comments.
func CleanText(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			r = ' '
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// SafePath reports whether p is an absolute path made only of characters that
// are safe to place in a dnsmasq config line.
func SafePath(p string) error {
	if !pathRe.MatchString(p) || strings.Contains(p, "..") {
		return fmt.Errorf("%q must be an absolute path using only letters, digits, '.', '_', '-' and '/'", p)
	}
	return nil
}
