// Command lds is LDS (Local DNS Server), a small DNS server for a home lab: dnsmasq serves the
// records, a web UI shows them, and this same binary is the CLI that edits them.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
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
	"github.com/yankluf/local-dns-server/internal/names"
	"github.com/yankluf/local-dns-server/internal/replica"
	"github.com/yankluf/local-dns-server/internal/server"
	"github.com/yankluf/local-dns-server/internal/settings"
	"github.com/yankluf/local-dns-server/internal/store"
)

var version = "dev"

const usage = `LDS (Local DNS Server) manages local DNS records served by dnsmasq.

Usage:
  lds serve                         Run the DNS server and web UI
  lds add <name> <value> [-c text]  Add a record
  lds rm <name|id>                  Remove a record (a name removes its IPv4 and IPv6 address)
  lds list [--json] [--source s]    List records (s: manual, docker or replica)
  lds check <name>                  Show what a name resolves to, and which record decides it
  lds import <file>                 Import records from Pi-hole, a hosts file or an export
  lds export [--all]                Print your records as a file "lds import" can read (a backup)
  lds healthcheck                   Exit 0 if the local server answers DNS (for Docker)
  lds version

The record type comes from the name and value:
  lds add '*.home.lan' 192.168.1.5          wildcard: home.lan and every subdomain
  lds add nas.home.lan 192.168.1.10         A record (an IPv6 address makes AAAA;
                                            one name can have one of each)
  lds add grafana.home.lan nas.home.lan     CNAME to another local record

Client commands talk to the server at --server, $LDS_URL or
http://127.0.0.1:5380. The token comes from $LDS_TOKEN or --token-file.
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
	case "export":
		err = export(args)
	case "check":
		err = check(args)
	case "healthcheck":
		err = healthcheck()
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", names.CleanText(cmd, 50), usage)
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
		return fmt.Errorf("bad settings:\n%w", err)
	}
	if cfg.TokenAuto {
		if err := ensureToken(&cfg); err != nil {
			return err
		}
	}
	if cfg.InsecureNoAuth && cfg.Token == "" {
		log.Print("WARNING: LDS_INSECURE_NO_AUTH is on: anyone who can reach the API can change your DNS")
	}
	if cfg.Token != "" && cfg.TLSCert == "" && !config.IsLoopback(cfg.HTTPListen) {
		log.Print("note: the API uses plain HTTP, so the token crosses the network unencrypted; " +
			"set LDS_TLS_CERT/LDS_TLS_KEY or put LDS behind Traefik with HTTPS")
	}

	// Settings changed from the dashboard override the environment.
	sm := settings.Load(filepath.Join(cfg.DataDir, "settings.json"), cfg)
	cur := sm.Current()
	if len(cur.Zones) == 0 {
		log.Print("no domains set yet: DNS forwarding works, but records are refused until you add domains under Settings (or LDS_ZONES)")
	}

	mgr := dnsmasq.NewManager(cur.DNSmasq)
	renderWith := func(c config.DNSmasq, recs []store.Record) dnsmasq.Files {
		f, skipped := dnsmasq.Render(c, recs)
		for _, e := range skipped {
			log.Print(e)
		}
		return f
	}
	render := func(recs []store.Record) dnsmasq.Files { return renderWith(sm.Current().DNSmasq, recs) }
	st, err := store.Open(filepath.Join(cfg.DataDir, "records.json"), store.Options{
		Zones:      cur.Zones,
		MaxRecords: cur.MaxRecords,
		ReadOnly:   cfg.ReplicaOf != "",
		// Every change is tried against dnsmasq before it's saved.
		Validate: func(active []store.Record) error { return mgr.Validate(render(active)) },
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Rewrite the dnsmasq config whenever records change, coalescing bursts.
	changed := make(chan struct{}, 1)
	st.OnChange(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	apply := func() {
		if err := mgr.Apply(render(st.Active())); err != nil {
			log.Printf("apply config: %v", err)
		}
	}

	// The watcher always exists so discovery can be switched on from the
	// dashboard; it stays idle while no Traefik IP is set.
	watcher, dockerErr := docker.NewWatcher(cur.Docker, st)
	if dockerErr != nil {
		if cur.Docker.Enabled {
			return dockerErr
		}
		log.Printf("docker discovery unavailable: %v", dockerErr)
		watcher = nil
	} else if cur.Docker.Enabled {
		log.Printf("docker discovery on (%s), Traefik hosts -> %s", cur.Docker.Host, cur.Docker.Target)
	} else {
		log.Print("docker discovery off (set the Traefik IP under Settings, or LDS_DOCKER_TARGET)")
	}

	sm.Records = st
	sm.Check = func(c config.Config) ([]string, error) {
		if c.Docker.Enabled && watcher == nil {
			return nil, fmt.Errorf("can't turn on Docker discovery: %v", dockerErr)
		}
		active, _ := st.Preview(c.Zones)
		if err := mgr.Validate(renderWith(c.DNSmasq, active)); err != nil {
			return nil, fmt.Errorf("dnsmasq rejected these settings: %w", err)
		}
		return nil, nil
	}
	sm.Apply = func(c config.Config) {
		if watcher != nil {
			watcher.SetConfig(c.Docker)
		}
		st.SetRules(c.Zones, c.MaxRecords) // also rewrites the dnsmasq config
	}

	var follower *replica.Follower
	if cfg.ReplicaOf != "" {
		follower = replica.New(cfg.ReplicaOf, cfg.Token, st, cfg.ReplicaEvery)
		// A replica answers for the same domains as its primary.
		follower.CopyZones(func(zones []string) error {
			_, err := sm.Update(settings.Change{Set: config.Overrides{Zones: &zones}})
			return err
		})
		log.Printf("read-only replica of %s", cfg.ReplicaOf)
		if strings.HasPrefix(cfg.ReplicaOf, "http://") {
			log.Print("note: the replica sends the token to the primary over plain HTTP; use https:// for the primary if the network isn't trusted")
		}
	}

	mgr.Start(render(st.Active()), render(nil))
	mgrDone := make(chan struct{})
	go func() { mgr.Run(ctx); close(mgrDone) }()
	if watcher != nil {
		go watcher.Run(ctx)
	}
	if follower != nil {
		go follower.Run(ctx)
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

	srv := &server.Server{
		Store: st, DNSmasq: mgr, Docker: watcher, Settings: sm,
		Token: cfg.Token, PublicRead: cfg.PublicRead, AllowedHosts: cfg.AllowedHosts,
		TLS: cfg.TLSCert != "", ReadOnly: cfg.ReplicaOf != "",
	}
	if follower != nil {
		srv.Replica = follower.Status
	}
	httpSrv := &http.Server{
		Addr:              cfg.HTTPListen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		ErrorLog:          log.New(os.Stderr, "http: ", log.LstdFlags),
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	log.Printf("web UI and API on %s (%s), domains: %s", cfg.HTTPListen, map[bool]string{true: "https", false: "http"}[cfg.TLSCert != ""], strings.Join(cur.Zones, ", "))
	if cfg.TLSCert != "" {
		err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	stop() // stop dnsmasq too if the HTTP server failed on its own
	<-mgrDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Print("stopped")
	return nil
}

// ensureToken loads the generated token from the data folder, creating it on
// first start. Used when no LDS_TOKEN or LDS_TOKEN_FILE is given.
func ensureToken(cfg *config.Config) error {
	path := filepath.Join(cfg.DataDir, "admin-token")
	if data, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(data)); len(tok) >= config.MinTokenLength {
			cfg.Token = tok
			log.Printf("no LDS_TOKEN set: using the token saved in %s", path)
			return nil
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	cfg.Token = hex.EncodeToString(b)
	if err := store.WriteFileDurable(path, []byte(cfg.Token+"\n"), 0o600); err != nil {
		return fmt.Errorf("save generated token: %w", err)
	}
	log.Printf("no LDS_TOKEN set: generated a token and saved it in %s. Show it with: docker exec lds cat %s", path, path)
	return nil
}

// healthcheck asks the local server's /healthz, which sends a real DNS query
// to dnsmasq. Used by the Docker HEALTHCHECK.
func healthcheck() error {
	listen := os.Getenv("LDS_HTTP_LISTEN")
	if listen == "" {
		listen = ":5380"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if os.Getenv("LDS_TLS_CERT") != "" {
		scheme = "https"
	}
	c := &http.Client{
		Timeout: 5 * time.Second,
		// The health check only cares that our own server answers.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 -- local health probe, no secrets sent
	}
	resp, err := c.Get(scheme + "://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", resp.Status)
	}
	return nil
}

// clientFlags adds --server and --token-file to fs and returns a constructor.
// There is deliberately no --token flag: command-line arguments are visible
// to other users in `ps` and end up in shell history.
func clientFlags(fs *flag.FlagSet) func() *client.Client {
	def := os.Getenv("LDS_URL")
	if def == "" {
		def = "http://127.0.0.1:5380"
	}
	srv := fs.String("server", def, "lds server URL")
	tokFile := fs.String("token-file", os.Getenv("LDS_TOKEN_FILE"), "file containing the API token")
	return func() *client.Client {
		tok := os.Getenv("LDS_TOKEN")
		if tok == "" && *tokFile == "" {
			// Inside the container, use the token lds serve generated.
			dir := os.Getenv("LDS_DATA_DIR")
			if dir == "" {
				dir = "/data"
			}
			if data, err := os.ReadFile(filepath.Join(dir, "admin-token")); err == nil {
				tok = strings.TrimSpace(string(data))
			}
		}
		if *tokFile != "" {
			data, err := os.ReadFile(*tokFile)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error: --token-file:", err)
				os.Exit(1)
			}
			tok = strings.TrimSpace(string(data))
		}
		return client.New(*srv, tok)
	}
}

// clean makes server-supplied text safe to print in a terminal.
func clean(s string) string { return names.CleanText(s, 300) }

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
	fmt.Printf("added %s %s -> %s (id %s)\n", clean(r.Type), clean(r.Name), clean(r.Value), clean(r.ID))
	if r.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", clean(r.Warning))
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
		removed, err := c.Remove(key)
		if err != nil {
			return err
		}
		for _, r := range removed {
			fmt.Printf("removed %s %s -> %s\n", clean(r.Type), clean(r.Name), clean(r.Value))
		}
	}
	return nil
}

// check prints what the server answers for a name and which record decides it.
func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	newClient := clientFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: lds check <name>")
	}
	res, err := newClient().Check(pos[0])
	if err != nil {
		return err
	}
	switch {
	case res.Error != "":
		fmt.Printf("%s: %s\n", clean(res.Name), clean(res.Error))
	case len(res.Answers) > 0:
		fmt.Printf("%s resolves to %s\n", clean(res.Name), clean(strings.Join(res.Answers, ", ")))
	case res.InZone:
		fmt.Printf("%s doesn't resolve\n", clean(res.Name))
	default:
		fmt.Printf("%s isn't answered here\n", clean(res.Name))
	}
	for _, r := range res.Records {
		src := r.Source
		if r.Container != "" {
			src += ":" + r.Container
		}
		fmt.Printf("  because of %s %s -> %s (%s)\n", clean(r.Type), clean(r.Name), clean(r.Value), clean(src))
	}
	if res.Note != "" {
		fmt.Printf("  note: %s\n", clean(res.Note))
	}
	return nil
}

// export prints the records as a file `lds import` reads back.
func export(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	all := fs.Bool("all", false, "also include records found through Docker")
	newClient := clientFlags(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	records, err := newClient().List()
	if err != nil {
		return err
	}
	fmt.Print(exportText(records, *all))
	return nil
}

// exportText renders records in the dnsmasq/hosts syntax ParseImport reads.
// Docker records are left out unless all is set: they come back on their own.
func exportText(records []store.View, all bool) string {
	var hosts, wild, cnames []string
	for _, r := range records {
		if r.Source == store.SourceDocker && (!all || !r.Active) {
			continue
		}
		// Names and values were validated by the server; clean them anyway
		// so a hostile server can't smuggle extra lines into the file.
		name, value, note := names.CleanText(r.Name, 300), names.CleanText(r.Value, 300), ""
		if strings.ContainsAny(name+value, " #,/") && r.Type != store.TypeWildcard || strings.ContainsAny(name+value, " #,") {
			continue
		}
		if c := names.CleanText(r.Comment, store.MaxCommentLength); c != "" {
			note = " # " + c
		}
		switch r.Type {
		case store.TypeWildcard:
			wild = append(wild, "address=/"+strings.TrimPrefix(name, "*.")+"/"+value+note)
		case store.TypeCNAME:
			cnames = append(cnames, "cname="+name+","+value+note)
		default:
			hosts = append(hosts, value+" "+name+note)
		}
	}
	var b strings.Builder
	b.WriteString("# LDS records. Restore with: lds import <this file>\n")
	for _, group := range [][]string{hosts, wild, cnames} {
		for _, line := range group {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(r.Name), clean(r.Type), clean(r.Value), clean(src), clean(r.ID), clean(note))
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
                                     (cname=, host-record=, address=/domain/ip)
  the output of "lds export"`)
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	pairs := parseImport(string(data), strings.HasSuffix(pos[0], ".toml"))
	if len(pairs) == 0 {
		return errors.New("no records found in " + pos[0])
	}
	c := newClient()
	var added, skipped int
	for _, p := range pairs {
		if *dry {
			fmt.Printf("would add %s -> %s\n", clean(p[0]), clean(p[1]))
			continue
		}
		comment := p[2]
		if comment == "" {
			comment = "imported"
		}
		r, err := c.Add(p[0], p[1], comment)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s -> %s: %v\n", clean(p[0]), clean(p[1]), err)
			skipped++
			continue
		}
		added++
		fmt.Printf("added %s %s -> %s\n", clean(r.Type), clean(r.Name), clean(r.Value))
	}
	if !*dry {
		fmt.Printf("%d added, %d skipped\n", added, skipped)
	}
	return nil
}
