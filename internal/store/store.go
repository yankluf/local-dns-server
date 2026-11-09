// Package store keeps the DNS records served by LDS.
//
// Manual records (added from the CLI, API or web UI) are persisted to a JSON
// file. Docker records are discovered from container labels at runtime and
// are kept in memory only, since they are rebuilt on every sync.
//
// Every change is checked before it is committed: names must be inside the
// configured zones, CNAMEs must not loop, and an optional Validate hook (the
// server uses `dnsmasq --test`) must accept the resulting record set. A change
// that fails any check is rejected and nothing is saved.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yankluf/local-dns-server/internal/names"
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
	SourceManual  = "manual"
	SourceDocker  = "docker"
	SourceReplica = "replica"
)

// MaxCommentLength is the longest comment kept on a record.
const MaxCommentLength = 200

// MaxCNAMEChain is the longest allowed chain of CNAMEs.
const MaxCNAMEChain = 8

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
	ErrFull     = errors.New("record limit reached (LDS_MAX_RECORDS)")
	ErrReadOnly = errors.New("this server is a read-only replica; make changes on the primary")
	ErrInvalid  = errors.New("invalid record")
)

// Options configures a store.
type Options struct {
	Zones      []string // required: records must be inside these domains
	MaxRecords int      // 0 = unlimited
	ReadOnly   bool     // replica mode: only SetReplica may change records
	// Validate, if set, is called with the full active record set before a
	// change is committed. Returning an error rejects the change.
	Validate func(active []Record) error
}

// Store holds manual and Docker records.
type Store struct {
	opts Options

	mu       sync.Mutex
	path     string
	manual   []Record
	docker   []Record
	onChange func()
}

// Open loads the store from path. If the file is missing or damaged it falls
// back to the backup copy kept next to it.
func Open(path string, opts Options) (*Store, error) {
	s := &Store{path: path, opts: opts}
	recs, err := readRecords(path)
	if err != nil {
		bak, bakErr := readRecords(path + ".bak")
		switch {
		case bakErr == nil && errors.Is(err, os.ErrNotExist):
			log.Printf("store: %s missing, using backup %s.bak", path, path)
			recs = bak
		case bakErr == nil:
			log.Printf("store: %s is damaged (%v); using backup %s.bak", path, err, path)
			recs = bak
		case errors.Is(err, os.ErrNotExist) && errors.Is(bakErr, os.ErrNotExist):
			recs = nil
		default:
			return nil, fmt.Errorf("can't read records: %w (backup: %v)", err, bakErr)
		}
	}
	for i := range recs {
		recs[i].Comment = names.CleanText(recs[i].Comment, MaxCommentLength)
		recs[i].Container = ""
		if recs[i].Source != SourceReplica {
			recs[i].Source = SourceManual
		}
	}
	s.manual = recs
	return s, nil
}

func readRecords(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Records []Record `json:"records"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	return file.Records, nil
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
//	*.home.lan  192.168.1.5           -> WILDCARD
//	nas.home.lan 192.168.1.10         -> A
//	nas.home.lan fd00::10             -> AAAA
//	grafana.home.lan traefik.home.lan -> CNAME
//
// Every name (and CNAME target) must be inside one of zones.
func New(name, value, comment string, zones []string) (Record, error) {
	name = names.Normalize(name)
	value = strings.TrimSpace(value)
	r := Record{Name: name, Comment: names.CleanText(comment, MaxCommentLength)}
	inZones := func(n string) error {
		if len(zones) == 0 {
			return errors.New("no domains are set up yet: add yours under Settings → Domains (or LDS_ZONES)")
		}
		if !names.InZones(n, zones) {
			return fmt.Errorf("%s is outside the domains this server manages (%s)", n, strings.Join(zones, ", "))
		}
		return nil
	}

	ip, ipErr := names.ParseIP(value)
	switch {
	case strings.HasPrefix(name, "*."):
		domain := name[2:]
		if err := names.ValidHostname(domain); err != nil {
			return r, err
		}
		if ipErr != nil {
			return r, fmt.Errorf("wildcard %q needs an IP address: %v", name, ipErr)
		}
		if err := inZones(domain); err != nil {
			return r, err
		}
		r.Type, r.Value = TypeWildcard, ip.String()
	case ipErr == nil:
		if err := names.ValidHostname(name); err != nil {
			return r, err
		}
		if err := inZones(name); err != nil {
			return r, err
		}
		r.Type, r.Value = TypeA, ip.String()
		if ip.Is6() {
			r.Type = TypeAAAA
		}
	default:
		if err := names.ValidHostname(name); err != nil {
			return r, err
		}
		target := names.Normalize(value)
		if err := names.ValidHostname(target); err != nil {
			return r, fmt.Errorf("value must be an IP address or a hostname: %w", err)
		}
		if target == name {
			return r, errors.New("a CNAME can't point to itself")
		}
		if err := inZones(name); err != nil {
			return r, err
		}
		if err := inZones(target); err != nil {
			return r, fmt.Errorf("CNAME target: %w (dnsmasq can only alias names it serves itself)", err)
		}
		r.Type, r.Value = TypeCNAME, target
	}
	return r, nil
}

// check re-validates a stored record against the current zones, so records
// saved under an older config can't bypass the rules.
func check(r Record, zones []string) error {
	n, err := New(r.Name, r.Value, "", zones)
	if err != nil {
		return err
	}
	if n.Type != r.Type || n.Value != r.Value {
		return fmt.Errorf("stored record doesn't match its type")
	}
	return nil
}

// Add stores a new manual record.
func (s *Store) Add(r Record) (Record, error) {
	if err := check(r, s.Zones()); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	r.Comment = names.CleanText(r.Comment, MaxCommentLength)
	s.mu.Lock()
	if s.opts.ReadOnly {
		s.mu.Unlock()
		return Record{}, ErrReadOnly
	}
	for _, m := range s.manual {
		if m.Name == r.Name {
			s.mu.Unlock()
			return Record{}, fmt.Errorf("%w: %s (%s %s)", ErrExists, m.Name, m.Type, m.Value)
		}
	}
	if s.opts.MaxRecords > 0 && len(s.manual) >= s.opts.MaxRecords {
		s.mu.Unlock()
		return Record{}, ErrFull
	}
	r.ID = s.newIDLocked()
	r.Source = SourceManual
	r.Container = ""
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC().Truncate(time.Second)
	}
	next := append(append([]Record{}, s.manual...), r)
	if err := s.commitLocked(next, s.docker); err != nil {
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
	name := names.Normalize(key)
	s.mu.Lock()
	if s.opts.ReadOnly {
		s.mu.Unlock()
		return Record{}, ErrReadOnly
	}
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
		return Record{}, fmt.Errorf("%w: %s", ErrNotFound, names.CleanText(key, 100))
	}
	removed := s.manual[idx]
	next := append(append([]Record{}, s.manual[:idx]...), s.manual[idx+1:]...)
	if err := s.commitLocked(next, s.docker); err != nil {
		s.mu.Unlock()
		return Record{}, err
	}
	s.mu.Unlock()
	s.changed()
	return removed, nil
}

// SetDocker replaces the set of Docker-discovered records. Only A/AAAA
// records inside the zones are accepted. It reports whether anything changed.
func (s *Store) SetDocker(records []Record) (bool, error) {
	zones := s.Zones()
	var clean []Record
	for _, r := range records {
		if (r.Type != TypeA && r.Type != TypeAAAA) || check(r, zones) != nil {
			continue
		}
		r.Source = SourceDocker
		r.ID = "docker:" + r.Name
		r.Container = names.CleanText(r.Container, 128)
		r.Comment = ""
		clean = append(clean, r)
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].Name < clean[j].Name })

	s.mu.Lock()
	if sameRecords(clean, s.docker) {
		s.mu.Unlock()
		return false, nil
	}
	// Keep the first-seen time of records that already existed.
	seen := map[string]time.Time{}
	for _, d := range s.docker {
		seen[d.Name] = d.CreatedAt
	}
	now := time.Now().UTC().Truncate(time.Second)
	for i := range clean {
		if t, ok := seen[clean[i].Name]; ok {
			clean[i].CreatedAt = t
		} else {
			clean[i].CreatedAt = now
		}
	}
	if err := s.validateLocked(s.manual, clean); err != nil {
		s.mu.Unlock()
		return false, err
	}
	s.docker = clean
	s.mu.Unlock()
	s.changed()
	return true, nil
}

// SetReplica replaces every manual record with a copy of the primary's
// active records (replica mode only).
func (s *Store) SetReplica(records []Record) (bool, error) {
	zones := s.Zones()
	var clean []Record
	for _, r := range records {
		if check(r, zones) != nil {
			continue
		}
		r.Comment = names.CleanText(r.Comment, MaxCommentLength)
		r.Container = names.CleanText(r.Container, 128)
		r.Source = SourceReplica
		r.ID = "replica:" + r.Name
		clean = append(clean, r)
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].Name < clean[j].Name })
	s.mu.Lock()
	if s.opts.MaxRecords > 0 && len(clean) > s.opts.MaxRecords {
		s.mu.Unlock()
		return false, ErrFull
	}
	if sameRecords(clean, s.manual) {
		s.mu.Unlock()
		return false, nil
	}
	if err := s.commitLocked(clean, s.docker); err != nil {
		s.mu.Unlock()
		return false, err
	}
	s.mu.Unlock()
	s.changed()
	return true, nil
}

func sameRecords(a, b []Record) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Name != y.Name || x.Value != y.Value || x.Type != y.Type || x.Container != y.Container || x.Comment != y.Comment {
			return false
		}
	}
	return true
}

// commitLocked validates the candidate state, saves it, and only then makes
// it current.
func (s *Store) commitLocked(manual, docker []Record) error {
	if err := s.validateLocked(manual, docker); err != nil {
		return err
	}
	if err := s.save(manual); err != nil {
		return err
	}
	s.manual = manual
	return nil
}

func (s *Store) validateLocked(manual, docker []Record) error {
	// Reject loops this change would create. Loops already in a damaged
	// file are switched off by views() and mustn't block unrelated changes.
	before := loopMembers(s.manual)
	for name := range loopMembers(manual) {
		if !before[name] {
			if err := checkCNAMEs(manual); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			return fmt.Errorf("%w: CNAME loop or overlong chain at %s", ErrInvalid, name)
		}
	}
	active := activeRecords(manual, docker, s.opts.Zones)
	if err := checkCNAMEs(active); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if s.opts.Validate != nil {
		if err := s.opts.Validate(active); err != nil {
			return fmt.Errorf("%w: dnsmasq rejected the change: %v", ErrInvalid, err)
		}
	}
	return nil
}

// checkCNAMEs rejects CNAME loops and overly long chains.
func checkCNAMEs(active []Record) error {
	target := map[string]string{}
	for _, r := range active {
		if r.Type == TypeCNAME {
			target[r.Name] = r.Value
		}
	}
	for start := range target {
		seen := map[string]bool{start: true}
		cur := start
		for depth := 0; ; depth++ {
			next, ok := target[cur]
			if !ok {
				break
			}
			if seen[next] {
				return fmt.Errorf("CNAME loop: following %s leads back to %s", start, next)
			}
			if depth >= MaxCNAMEChain {
				return fmt.Errorf("CNAME chain starting at %s is longer than %d", start, MaxCNAMEChain)
			}
			seen[next] = true
			cur = next
		}
	}
	return nil
}

// loopMembers returns the CNAMEs that are in a loop or too long a chain.
// Normal changes can't create these; they only appear if the records file
// was edited by hand or damaged, and are then left out instead of breaking
// the whole config.
func loopMembers(recs []Record) map[string]bool {
	target := map[string]string{}
	for _, r := range recs {
		if r.Type == TypeCNAME {
			target[r.Name] = r.Value
		}
	}
	bad := map[string]bool{}
	for start := range target {
		seen := map[string]bool{start: true}
		cur := start
		for depth := 0; ; depth++ {
			next, ok := target[cur]
			if !ok {
				break
			}
			if seen[next] || depth >= MaxCNAMEChain {
				bad[start] = true
				break
			}
			seen[next] = true
			cur = next
		}
	}
	return bad
}

// SetRules changes the zones and record limit (from the dashboard). Records
// outside the new zones stay saved but are no longer served.
func (s *Store) SetRules(zones []string, maxRecords int) {
	s.mu.Lock()
	s.opts.Zones = append([]string(nil), zones...)
	s.opts.MaxRecords = maxRecords
	s.mu.Unlock()
	s.changed()
}

// Zones returns the current zones.
func (s *Store) Zones() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.opts.Zones...)
}

// Preview returns the records that would be served under other zones, and
// the manual records that would stop being served.
func (s *Store) Preview(zones []string) (active []Record, dropped []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range views(s.manual, s.docker, zones) {
		if v.Active {
			active = append(active, v.Record)
		} else if v.Source != SourceDocker && check(v.Record, s.opts.Zones) == nil {
			dropped = append(dropped, v.Name)
		}
	}
	return active, dropped
}

// List returns every record with its state.
func (s *Store) List() []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return views(s.manual, s.docker, s.opts.Zones)
}

// Active returns the records that should be written to dnsmasq.
func (s *Store) Active() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return activeRecords(s.manual, s.docker, s.opts.Zones)
}

func activeRecords(manual, docker []Record, zones []string) []Record {
	var out []Record
	for _, v := range views(manual, docker, zones) {
		if v.Active {
			out = append(out, v.Record)
		}
	}
	return out
}

func views(manual, docker []Record, zones []string) []View {
	manualNames := map[string]bool{}
	local := map[string]bool{} // names dnsmasq can resolve locally (A/AAAA/CNAME)
	for _, m := range manual {
		manualNames[m.Name] = true
		if m.Type != TypeWildcard {
			local[m.Name] = true
		}
	}
	for _, d := range docker {
		local[d.Name] = true
	}

	looping := loopMembers(manual)
	out := make([]View, 0, len(manual)+len(docker))
	for _, m := range manual {
		v := View{Record: m, Active: true}
		if err := check(m, zones); err != nil {
			v.Active = false
			v.Warning = "not served: " + err.Error()
		} else if looping[m.Name] {
			v.Active = false
			v.Warning = "not served: part of a CNAME loop or an overlong chain; remove it"
		} else if m.Type == TypeCNAME && !local[m.Value] {
			v.Warning = "won't resolve: dnsmasq can only alias names defined here, and " + m.Value + " isn't one"
		}
		out = append(out, v)
	}
	for _, d := range docker {
		v := View{Record: d, Active: true}
		if manualNames[d.Name] {
			v.Active = false
			v.Warning = "overridden by a manual record with the same name"
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// save writes the records durably: write and fsync a temporary file, keep the
// previous version as .bak, rename the new file into place, fsync the folder.
func (s *Store) save(manual []Record) error {
	dir := filepath.Dir(s.path)
	// 0755: dnsmasq's unprivileged user must reach hosts.d in this folder;
	// records.json itself is 0600.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Records []Record `json:"records"`
	}{manual}, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileDurable(s.path+".tmp", append(data, '\n'), 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(s.path); err == nil {
		if err := os.Rename(s.path, s.path+".bak"); err != nil {
			return err
		}
	}
	if err := os.Rename(s.path+".tmp", s.path); err != nil {
		return err
	}
	return SyncDir(dir)
}

// WriteFileDurable writes data to path and fsyncs it before returning.
func WriteFileDurable(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// SyncDir fsyncs a directory so renames inside it survive a power loss.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) newIDLocked() string {
	for {
		b := make([]byte, 8)
		_, _ = rand.Read(b) // never fails since Go 1.24
		id := hex.EncodeToString(b)
		taken := false
		for _, m := range s.manual {
			if m.ID == id {
				taken = true
				break
			}
		}
		if !taken {
			return id
		}
	}
}
