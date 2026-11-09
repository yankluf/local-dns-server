// Package replica keeps a read-only copy of a primary LDS server's records,
// so a second DNS server can keep answering if the primary goes down.
package replica

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/yankluf/local-dns-server/internal/client"
	"github.com/yankluf/local-dns-server/internal/store"
)

// Status describes replication.
type Status struct {
	Primary   string    `json:"primary"`
	LastSync  time.Time `json:"last_sync,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Records   int       `json:"records"`
}

// Follower copies the primary's active records into the local store.
type Follower struct {
	client *client.Client
	store  *store.Store
	every  time.Duration
	// setZones, if set, makes this server answer for the primary's domains.
	setZones func([]string) error

	mu     sync.Mutex
	status Status
}

// New returns a follower for the primary at url.
func New(url, token string, st *store.Store, every time.Duration) *Follower {
	return &Follower{client: client.New(url, token), store: st, every: every, status: Status{Primary: url}}
}

// CopyZones makes the follower keep this server's domains equal to the
// primary's, so a replica needs no domain setup of its own.
func (f *Follower) CopyZones(set func([]string) error) { f.setZones = set }

// Status returns the replication state.
func (f *Follower) Status() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// Run syncs now and then every interval until ctx is cancelled. If the
// primary is unreachable the last copy (saved on disk) keeps being served.
func (f *Follower) Run(ctx context.Context) {
	t := time.NewTicker(f.every)
	defer t.Stop()
	for {
		f.sync()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (f *Follower) sync() {
	accepted, skipped, err := f.pull()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case err != nil:
		log.Printf("replica: %v (still serving the last copy)", err)
		f.status.LastError = err.Error()
		return
	case skipped > 0:
		msg := fmt.Sprintf("%d of the primary's record(s) were left out because they aren't valid on this server", skipped)
		if f.status.LastError != msg {
			log.Printf("replica: %s", msg)
		}
		f.status.LastError = msg
	default:
		f.status.LastError = ""
	}
	f.status.LastSync, f.status.Records = time.Now().UTC(), accepted
}

// pull copies the primary's domains and active records.
func (f *Follower) pull() (accepted, skipped int, err error) {
	if f.setZones != nil {
		zones, err := f.client.Zones()
		if err != nil {
			return 0, 0, err
		}
		if len(zones) > 0 && !slices.Equal(zones, f.store.Zones()) {
			if err := f.setZones(zones); err != nil {
				return 0, 0, fmt.Errorf("can't use the primary's domains: %w", err)
			}
			log.Printf("replica: now using the primary's domains (%v)", zones)
		}
	}
	views, err := f.client.List()
	if err != nil {
		return 0, 0, err
	}
	var recs []store.Record
	for _, v := range views {
		if v.Active {
			recs = append(recs, v.Record)
		}
	}
	changed, skipped, err := f.store.SetReplica(recs)
	if err != nil {
		return 0, skipped, err
	}
	accepted = len(recs) - skipped
	if changed {
		log.Printf("replica: copied %d record(s) from the primary", accepted)
	}
	return accepted, skipped, nil
}
