// Package store keeps the DNS records served by lds.
//
// Manual records (added from the CLI, API or web UI) are persisted to a JSON
// file. Docker records are discovered from container labels at runtime and
// are kept in memory only, since they are rebuilt on every sync.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record types.
const (
	TypeA        = "A"
	TypeAAAA     = "AAAA"
	TypeCNAME    = "CNAME"
	TypeWildcard = "WILDCARD"
)

// Record sources.
const (
	SourceManual = "manual"
	SourceDocker = "docker"
)

// Record is a single DNS entry.
type Record struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	Comment   string    `json:"comment,omitempty"`
	Source    string    `json:"source"`
	Container string    `json:"container,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// View is a record as presented to clients, with its effective state.
type View struct {
	Record
	// Active is false when the record is not written to dnsmasq, e.g. a
	// Docker record shadowed by a manual record with the same name.
	Active bool `json:"active"`
	// Warning explains a problem with the record, if any.
	Warning string `json:"warning,omitempty"`
}

var (
	ErrNotFound = errors.New("record not found")
	ErrExists   = errors.New("a record with that name already exists")
)

// Store holds manual and Docker records.
type Store struct {
	mu       sync.RWMutex
	path     string
	manual   []Record
	docker   []Record
	onChange func()
}

// Open loads the store from path, creating an empty one if it doesn't exist.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	var file struct {
		Records []Record `json:"records"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.manual = file.Records
	return s, nil
}

// OnChange registers a callback invoked (without locks held) after every change.
func (s *Store) OnChange(fn func()) { s.onChange = fn }

func (s *Store) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}

// New builds a validated record from a name and value, inferring its type:
//
//	*.home.lan  192.168.1.5        -> WILDCARD
//	nas.home.lan 192.168.1.10      -> A
//	nas.home.lan fd00::10          -> AAAA
//	grafana.home.lan traefik.home.lan -> CNAME
func New(name, value, comment string) (Record, error) {
	name = normalize(name)
	value = strings.TrimSpace(value)
	r := Record{Name: name, Comment: strings.TrimSpace(comment)}

	ip, ipErr := netip.ParseAddr(value)
	switch {
	case strings.HasPrefix(name, "*."):
		if ipErr != nil {
			return r, fmt.Errorf("wildcard %q needs an IP address, got %q", name, value)
		}
		if err := validHostname(name[2:]); err != nil {
			return r, err
		}
		r.Type = TypeWildcard
		r.Value = ip.String()
	case ipErr == nil:
		if err := validHostname(name); err != nil {
			return r, err
		}
		r.Type = TypeA
		if ip.Is6() && !ip.Is4In6() {
			r.Type = TypeAAAA
		}
		r.Value = ip.Unmap().String()
	default:
		if err := validHostname(name); err != nil {
			return r, err
		}
		target := normalize(value)
		if err := validHostname(target); err != nil {
			return r, fmt.Errorf("value must be an IP address or a hostname: %w", err)
		}
		if target == name {
			return r, errors.New("a CNAME can't point to itself")
		}
		r.Type = TypeCNAME
		r.Value = target
	}
	return r, nil
}

// Add stores a new manual record.
func (s *Store) Add(r Record) (Record, error) {
	s.mu.Lock()
	for _, m := range s.manual {
		if m.Name == r.Name {
			s.mu.Unlock()
			return Record{}, fmt.Errorf("%w: %s (%s %s)", ErrExists, m.Name, m.Type, m.Value)
		}
	}
	r.ID = newID()
	r.Source = SourceManual
	r.Container = ""
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC().Truncate(time.Second)
	}
	s.manual = append(s.manual, r)
	if err := s.save(); err != nil {
		s.manual = s.manual[:len(s.manual)-1]
		s.mu.Unlock()
		return Record{}, err
	}
	s.mu.Unlock()
	s.changed()
	return r, nil
}

// Remove deletes a manual record by ID or name.
func (s *Store) Remove(key string) (Record, error) {
	key = strings.TrimSpace(key)
	name := normalize(key)
	s.mu.Lock()
	idx := -1
	for i, m := range s.manual {
		if m.ID == key || m.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		for _, d := range s.docker {
			if d.ID == key || d.Name == name {
				s.mu.Unlock()
				return Record{}, fmt.Errorf("%s comes from Docker container %q; remove or relabel the container instead", d.Name, d.Container)
			}
		}
		s.mu.Unlock()
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	removed := s.manual[idx]
	prev := s.manual
	s.manual = append(append([]Record{}, prev[:idx]...), prev[idx+1:]...)
	if err := s.save(); err != nil {
		s.manual = prev
		s.mu.Unlock()
		return Record{}, err
	}
	s.mu.Unlock()
	s.changed()
	return removed, nil
}

// SetDocker replaces the set of Docker-discovered records. It reports whether
// anything changed.
func (s *Store) SetDocker(records []Record) bool {
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	for i := range records {
		records[i].Source = SourceDocker
		records[i].ID = "docker:" + records[i].Name
	}
	s.mu.Lock()
	same := len(records) == len(s.docker)
	if same {
		for i := range records {
			a, b := records[i], s.docker[i]
			if a.Name != b.Name || a.Value != b.Value || a.Type != b.Type || a.Container != b.Container {
				same = false
				break
			}
		}
	}
	if !same {
		// Keep the first-seen time of records that already existed.
		seen := map[string]time.Time{}
		for _, d := range s.docker {
			seen[d.Name] = d.CreatedAt
		}
		now := time.Now().UTC().Truncate(time.Second)
		for i := range records {
			if t, ok := seen[records[i].Name]; ok {
				records[i].CreatedAt = t
			} else {
				records[i].CreatedAt = now
			}
		}
		s.docker = records
	}
	s.mu.Unlock()
	if !same {
		s.changed()
	}
	return !same
}

// List returns every record (manual first, then Docker) with its state.
func (s *Store) List() []View {
	s.mu.RLock()
	defer s.mu.RUnlock()

	manualNames := map[string]bool{}
	local := map[string]bool{} // names dnsmasq can resolve locally (A/AAAA/CNAME)
	for _, m := range s.manual {
		manualNames[m.Name] = true
		if m.Type != TypeWildcard {
			local[m.Name] = true
		}
	}
	for _, d := range s.docker {
		local[d.Name] = true
	}

	views := make([]View, 0, len(s.manual)+len(s.docker))
	for _, m := range s.manual {
		v := View{Record: m, Active: true}
		if m.Type == TypeCNAME && !local[m.Value] {
			v.Warning = "won't resolve: dnsmasq can only alias names defined here, and " + m.Value + " isn't one"
		}
		views = append(views, v)
	}
	for _, d := range s.docker {
		v := View{Record: d, Active: true}
		if manualNames[d.Name] {
			v.Active = false
			v.Warning = "overridden by a manual record with the same name"
		}
		views = append(views, v)
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

// Active returns the records that should be written to dnsmasq.
func (s *Store) Active() []Record {
	var out []Record
	for _, v := range s.List() {
		if v.Active {
			out = append(out, v.Record)
		}
	}
	return out
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Records []Record `json:"records"`
	}{s.manual}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func normalize(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func validHostname(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}
	if len(name) > 253 {
		return fmt.Errorf("%q is too long", name)
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%q is not a valid hostname", name)
		}
		for i, c := range label {
			ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
			if !ok || (c == '-' && (i == 0 || i == len(label)-1)) {
				return fmt.Errorf("%q is not a valid hostname", name)
			}
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
