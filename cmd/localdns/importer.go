package main

import (
	"net/netip"
	"regexp"
	"strings"
)

var (
	tomlArray  = regexp.MustCompile(`(?ms)^\s*(hosts|cnameRecords)\s*=\s*\[(.*?)\]`)
	tomlString = regexp.MustCompile(`"([^"]*)"`)
)

// ParseImport extracts (name, value) pairs from Pi-hole and dnsmasq files.
// Host records come before CNAMEs so aliases point at names that exist.
func ParseImport(data string, isTOML bool) [][2]string {
	var hosts, cnames [][2]string
	addHosts := func(line string) {
		f := strings.Fields(line)
		if len(f) < 2 {
			return
		}
		if _, err := netip.ParseAddr(f[0]); err != nil {
			return
		}
		for _, name := range f[1:] {
			if name == "localhost" || strings.HasPrefix(name, "#") {
				break
			}
			hosts = append(hosts, [2]string{name, f[0]})
		}
	}
	addCNAME := func(spec string) {
		// alias[,alias...],target[,ttl]
		parts := strings.Split(spec, ",")
		if len(parts) >= 3 && isNumber(parts[len(parts)-1]) {
			parts = parts[:len(parts)-1]
		}
		if len(parts) < 2 {
			return
		}
		target := strings.TrimSpace(parts[len(parts)-1])
		for _, a := range parts[:len(parts)-1] {
			cnames = append(cnames, [2]string{strings.TrimSpace(a), target})
		}
	}

	if isTOML {
		for _, m := range tomlArray.FindAllStringSubmatch(data, -1) {
			for _, s := range tomlString.FindAllStringSubmatch(m[2], -1) {
				if m[1] == "hosts" {
					addHosts(s[1])
				} else {
					addCNAME(s[1])
				}
			}
		}
		return append(hosts, cnames...)
	}

	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "cname="):
			addCNAME(strings.TrimPrefix(line, "cname="))
		case strings.HasPrefix(line, "host-record="):
			// host-record=name[,name...],ip[,ip][,ttl]
			var names, ips []string
			for _, p := range strings.Split(strings.TrimPrefix(line, "host-record="), ",") {
				p = strings.TrimSpace(p)
				if _, err := netip.ParseAddr(p); err == nil {
					ips = append(ips, p)
				} else if !isNumber(p) {
					names = append(names, p)
				}
			}
			for _, n := range names {
				for _, ip := range ips {
					hosts = append(hosts, [2]string{n, ip})
				}
			}
		case strings.HasPrefix(line, "address=/"):
			// address=/domain/ip -> wildcard
			parts := strings.Split(strings.TrimPrefix(line, "address="), "/")
			if len(parts) == 3 && parts[1] != "" && parts[2] != "" {
				hosts = append(hosts, [2]string{"*." + parts[1], parts[2]})
			}
		default:
			addHosts(line)
		}
	}
	return append(hosts, cnames...)
}

func isNumber(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
