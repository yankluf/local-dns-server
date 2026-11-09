package docker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yankluf/local-dns-server/internal/config"
	"github.com/yankluf/local-dns-server/internal/store"
)

// fakeDocker serves /containers/json and a streaming /events endpoint.
type fakeDocker struct {
	mu         sync.Mutex
	containers []Container
	events     chan string
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/containers/json":
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.containers)
	case "/events":
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-f.events:
				w.Write([]byte(ev + "\n"))
				w.(http.Flusher).Flush()
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func TestWatcherFollowsContainers(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	fake := &fakeDocker{events: make(chan string, 4), containers: []Container{{
		Names:  []string{"/whoami"},
		Labels: map[string]string{"traefik.http.routers.w.rule": "Host(`whoami.home.lan`)"},
	}}}
	srv := &http.Server{Handler: fake}
	go srv.Serve(ln)
	defer srv.Close()

	zones := []string{"home.lan"}
	st, _ := store.Open(filepath.Join(dir, "records.json"), store.Options{Zones: zones})
	w, err := NewWatcher(config.Docker{Enabled: true, Host: "unix://" + sock, Target: "192.168.1.5", AllowedTargets: []string{"192.168.1.5"}, Zones: zones}, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	waitFor := func(desc string, ok func([]store.View) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if ok(st.List()) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s; records: %+v", desc, st.List())
	}

	waitFor("initial sync", func(v []store.View) bool {
		return len(v) == 1 && v[0].Name == "whoami.home.lan" && v[0].Value == "192.168.1.5" && v[0].Container == "whoami"
	})
	if s := w.Status(); !s.Connected {
		t.Errorf("status not connected: %+v", s)
	}

	// A new container starts, the old one stops.
	fake.mu.Lock()
	fake.containers = []Container{{
		Names:  []string{"/grafana"},
		Labels: map[string]string{"traefik.http.routers.g.rule": "Host(`grafana.home.lan`)"},
	}}
	fake.mu.Unlock()
	fake.events <- `{"Type":"container","Action":"start"}`

	waitFor("event-driven resync", func(v []store.View) bool {
		return len(v) == 1 && v[0].Name == "grafana.home.lan"
	})
}

// Discovery switched on and off from the dashboard while running.
func TestWatcherSetConfig(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	fake := &fakeDocker{events: make(chan string, 4), containers: []Container{{
		Names:  []string{"/app"},
		Labels: map[string]string{"traefik.http.routers.a.rule": "Host(`app.home.lan`)"},
	}}}
	srv := &http.Server{Handler: fake}
	go srv.Serve(ln)
	defer srv.Close()

	zones := []string{"home.lan"}
	st, _ := store.Open(filepath.Join(dir, "records.json"), store.Options{Zones: zones})
	off := config.Docker{Host: "unix://" + sock, Zones: zones} // no target: discovery off
	w, err := NewWatcher(off, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	wait := func(desc string, n int) {
		t.Helper()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if len(st.List()) == n {
				return
			}
		}
		t.Fatalf("timed out waiting for %s: %+v", desc, st.List())
	}
	time.Sleep(200 * time.Millisecond)
	if len(st.List()) != 0 {
		t.Fatal("discovery ran while off")
	}
	on := off
	on.Target, on.Enabled, on.AllowedTargets = "10.0.0.5", true, []string{"10.0.0.5"}
	w.SetConfig(on)
	wait("records after turning on", 1)
	w.SetConfig(off)
	wait("records removed after turning off", 0)
}
