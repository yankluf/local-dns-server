// Command lds is LDS (Local DNS Server), a small DNS server for a home lab: dnsmasq serves the
// records, a web UI shows them, and this same binary is the CLI that edits them.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/yankluf/local-dns-server/internal/client"
	"github.com/yankluf/local-dns-server/internal/config"
	"github.com/yankluf/local-dns-server/internal/dnsmasq"
	"github.com/yankluf/local-dns-server/internal/docker"
	"github.com/yankluf/local-dns-server/internal/server"
	"github.com/yankluf/local-dns-server/internal/store"
)

var version = "dev"

const usage = `LDS (Local DNS Server) manages local DNS records served by dnsmasq.

Usage:
  lds serve                         Run the DNS server and web UI
  lds add <name> <value> [-c text]  Add a record
  lds rm <name|id>                  Remove a record
  lds list [--json] [--source s]    List records (s: manual or docker)
  lds import <file>                 Import records from Pi-hole or a hosts file
  lds version

The record type comes from the name and value:
  lds add '*.home.lan' 192.168.1.5          wildcard: home.lan and every subdomain
  lds add nas.home.lan 192.168.1.10         A record (an IPv6 address makes AAAA)
  lds add grafana.home.lan nas.home.lan     CNAME to another local record

Client commands talk to the server at --server, $LDS_URL or
http://127.0.0.1:5380, sending $LDS_TOKEN if the server requires one.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "add":
		err = add(args)
	case "rm", "remove", "delete":
		err = remove(args)
	case "list", "ls":
		err = list(args)
	case "import":
		err = importFile(args)
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve() error {
	log.SetFlags(log.LstdFlags)
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "records.json"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mgr := dnsmasq.NewManager(cfg.DNSmasq)

	// Rewrite the dnsmasq config whenever records change, coalescing bursts.
	changed := make(chan struct{}, 1)
	st.OnChange(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	apply := func() {
		if err := mgr.Apply(dnsmasq.Render(cfg.DNSmasq, st.Active())); err != nil {
			log.Printf("apply config: %v", err)
		}
	}

	var watcher *docker.Watcher
	if cfg.Docker.Enabled {
		if watcher, err = docker.NewWatcher(cfg.Docker, st); err != nil {
			return err
		}
		log.Printf("docker discovery on (%s), Traefik hosts -> %s", cfg.Docker.Host, cfg.Docker.Target)
	} else {
		log.Print("docker discovery off (set LDS_DOCKER_TARGET to enable)")
	}

	apply()
	mgrDone := make(chan struct{})
	go func() { mgr.Run(ctx); close(mgrDone) }()
	if watcher != nil {
		go watcher.Run(ctx)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				time.Sleep(200 * time.Millisecond)
				select { // drop changes that arrived while waiting
				case <-changed:
				default:
				}
				apply()
			}
		}
	}()

	srv := &server.Server{Store: st, DNSmasq: mgr, Docker: watcher, Upstreams: cfg.DNSmasq.Upstreams, Token: cfg.Token}
	httpSrv := &http.Server{Addr: cfg.HTTPListen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	log.Printf("web UI and API on %s", cfg.HTTPListen)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		stop()
		<-mgrDone
		return err
	}
	stop() // stop dnsmasq too if the HTTP server failed on its own
	<-mgrDone
	log.Print("stopped")
	return nil
}

// clientFlags adds --server and --token to fs and returns a constructor.
func clientFlags(fs *flag.FlagSet) func() *client.Client {
	def := os.Getenv("LDS_URL")
	if def == "" {
		def = "http://127.0.0.1:5380"
	}
	srv := fs.String("server", def, "lds server URL")
	tok := fs.String("token", os.Getenv("LDS_TOKEN"), "API token")
	return func() *client.Client { return client.New(*srv, *tok) }
}

// parse lets flags appear before or after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func add(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	comment := fs.String("c", "", "comment")
	fs.StringVar(comment, "comment", "", "comment")
	newClient := clientFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: lds add <name> <value> [-c comment]")
	}
	r, err := newClient().Add(pos[0], pos[1], *comment)
	if err != nil {
		return err
	}
	fmt.Printf("added %s %s -> %s (id %s)\n", r.Type, r.Name, r.Value, r.ID)
	if r.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", r.Warning)
	}
	return nil
}

func remove(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ExitOnError)
	newClient := clientFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return errors.New("usage: lds rm <name|id>...")
	}
	c := newClient()
	for _, key := range pos {
		r, err := c.Remove(key)
		if err != nil {
			return err
		}
		fmt.Printf("removed %s %s -> %s\n", r.Type, r.Name, r.Value)
	}
	return nil
}

func list(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	source := fs.String("source", "", "only show manual or docker records")
	newClient := clientFlags(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	records, err := newClient().List()
	if err != nil {
		return err
	}
	if *source != "" {
		filtered := records[:0]
		for _, r := range records {
			if r.Source == *source {
				filtered = append(filtered, r)
			}
		}
		records = filtered
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(records)
	}
	if len(records) == 0 {
		fmt.Println("no records")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tVALUE\tSOURCE\tID\tNOTE")
	for _, r := range records {
		src := r.Source
		if r.Container != "" {
			src += ":" + r.Container
		}
		note := r.Comment
		if r.Warning != "" {
			note = strings.TrimSpace("! " + r.Warning + " " + note)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Type, r.Value, src, r.ID, note)
	}
	return tw.Flush()
}

func importFile(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "only print what would be imported")
	newClient := clientFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(`usage: lds import [--dry-run] <file>

Understands:
  Pi-hole v6 pihole.toml             (dns.hosts and dns.cnameRecords)
  Pi-hole v5 custom.list / hosts     ("192.168.1.10 nas.lan")
  dnsmasq conf (05-pihole-custom-cname.conf etc.)
                                     (cname=, host-record=, address=/domain/ip)`)
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	pairs := ParseImport(string(data), strings.HasSuffix(pos[0], ".toml"))
	if len(pairs) == 0 {
		return errors.New("no records found in " + pos[0])
	}
	c := newClient()
	var added, skipped int
	for _, p := range pairs {
		if *dry {
			fmt.Printf("would add %s -> %s\n", p[0], p[1])
			continue
		}
		r, err := c.Add(p[0], p[1], "imported")
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s -> %s: %v\n", p[0], p[1], err)
			skipped++
			continue
		}
		added++
		fmt.Printf("added %s %s -> %s\n", r.Type, r.Name, r.Value)
	}
	if !*dry {
		fmt.Printf("%d added, %d skipped\n", added, skipped)
	}
	return nil
}
