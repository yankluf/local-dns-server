// Package replica keeps a read-only copy of a primary LDS server's records,
// so a second DNS server can keep answering if the primary goes down.
package replica

import (
	"context"
	"log"
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

	mu     sync.Mutex
	status Status
}

// New returns a follower for the primary at url.
func New(url, token string, st *store.Store, every time.Duration) *Follower {
	return &Follower{client: client.New(url, token), store: st, every: every, status: Status{Primary: url}}
}

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
	views, err := f.client.List()
	if err == nil {
		var recs []store.Record
		for _, v := range views {
			if v.Active {
				recs = append(recs, v.Record)
			}
		}
		var changed bool
		changed, err = f.store.SetReplica(recs)
		if err == nil {
			if changed {
				log.Printf("replica: copied %d record(s) from the primary", len(recs))
			}
			f.mu.Lock()
			f.status.LastSync, f.status.LastError, f.status.Records = time.Now().UTC(), "", len(recs)
			f.mu.Unlock()
			return
		}
	}
	log.Printf("replica: %v (still serving the last copy)", err)
	f.mu.Lock()
	f.status.LastError = err.Error()
	f.mu.Unlock()
}
