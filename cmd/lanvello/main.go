// Command lanvello is a lightweight universal gateway for opencode free models.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"lanvello/internal/config"
	"lanvello/internal/keys"
	"lanvello/internal/lanes"
	"lanvello/internal/server"
	"lanvello/internal/tor"
	"lanvello/internal/upstream"
)

// stamped in release builds via -ldflags "-X main.version=..."
var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "key":
		cmdKey(os.Args[2:])
	case "lanes":
		cmdLanes(os.Args[2:])
	case "demo":
		cmdDemo(os.Args[2:])
	case "tui":
		cmdTui(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("lanvello " + version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`lanvello - opencode free models over one local openai-compatible port
usage:
  lanvello serve [--listen 127.0.0.1:11434] [--lanes 5] [--config file.json]
  lanvello key add --name aider
  lanvello key list
  lanvello key revoke --name aider
  lanvello lanes status
  lanvello demo [question]
  lanvello tui`)
}

func baseConfig(args []string) config.Config {
	fs := flag.NewFlagSet("base", flag.ContinueOnError)
	listen := fs.String("listen", "", "listen addr")
	ln := fs.Int("lanes", 0, "lane count")
	cfgPath := fs.String("config", "", "config json path")
	dataDir := fs.String("data-dir", "", "state dir")
	_ = fs.Parse(args)
	cfg := config.Defaults()
	if *listen != "" {
		cfg.Listen = *listen
	}
	if v := os.Getenv("LANVELLO_LISTEN"); v != "" && *listen == "" {
		cfg.Listen = v
	}
	if *ln > 0 {
		cfg.Lanes = *ln
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	var err error
	cfg, err = config.LoadFile(*cfgPath, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error: "+err.Error())
		os.Exit(1)
	}
	return cfg
}

func openDeps(cfg config.Config) (*keys.Store, *lanes.Manager) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ks, err := keys.Open(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pri, fb := lanes.LoadCountriesFile(cfg.DataDir, cfg.Countries, cfg.Fallback)
	m := lanes.New(cfg.DataDir, pri, fb, cfg.Socks, false, cfg.Lanes)
	return ks, m
}

func healthySocks(m *lanes.Manager) int {
	n := 0
	for _, l := range m.Lanes() {
		if l.Healthy && l.SocksAddr != "" {
			n++
		}
	}
	return n
}

func cmdServe(args []string) {
	cfg := baseConfig(args)
	ks, m := openDeps(cfg)
	// tor-only: direct egress is gone. without tor lanes there is no service.
	if len(cfg.Socks) == 0 {
		tor.Ensure(m, cfg.DataDir, 52001, 52301, 90*time.Second)
		// A 429 rotates the lane: respawn its tor in the background so the
		// next request lands on a fresh exit.
		m.OnRotate = func(l *lanes.Lane) {
			tor.Respawn(m, l, cfg.DataDir, 52001, 52301, 90*time.Second)
		}
	}
	if n := healthySocks(m); n == 0 {
		fmt.Fprintln(os.Stderr, "no tor lanes up (tor binary missing or bootstrap failed) -- refusing to serve direct")
		os.Exit(1)
	}
	up := upstream.NewClient(m)
	up.BaseURL = cfg.BaseURL
	up.WaitBudget = time.Duration(cfg.WaitBudgetS * float64(time.Second))
	up.CatalogBudget = time.Duration(cfg.CatalogBudgetS * float64(time.Second))
	// per-model exit pins come from config only: the free tier serves nothing
	// that needs a specific country today, and a wrong pin costs a lane.
	up.WantCountry = map[string]string{}
	for pref, cc := range cfg.ModelLanes {
		up.WantCountry[pref] = cc
	}
	srv := server.New(ks, m, up, cfg.DataDir)
	fmt.Printf("lanvello %s listening on http://%s lanes=%d tor-only, free, no login\n", version, cfg.Listen, cfg.Lanes)
	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// baseFlags lifts the flags baseConfig understands (--config,
// --data-dir, --listen, --lanes) out of a subcommand's argument
// list, dropping flags that belong to the subcommand itself
// (--name): flag.Parse stops at the first unknown flag, so passing
// the raw list would silently ignore --config and put keys in the
// wrong data dir.
func baseFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch name {
		case "config", "data-dir", "listen", "lanes":
			if hasVal {
				out = append(out, "--"+name+"="+val)
			} else if i+1 < len(args) {
				out = append(out, "--"+name, args[i+1])
				i++
			}
		}
	}
	return out
}

func cmdKey(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lanvello key add|list|revoke")
		os.Exit(2)
	}
	cfg := baseConfig(baseFlags(args[1:]))
	ks, _ := openDeps(cfg)
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("key-add", flag.ContinueOnError)
		name := fs.String("name", "", "key name")
		// base flags reach baseFlags before us; declare them here too
		// so a plain `key add --config ...` line parses without noise
		fs.String("config", "", "config json path")
		fs.String("data-dir", "", "state dir")
		_ = fs.Parse(args[1:])
		if *name == "" {
			fmt.Fprintln(os.Stderr, "need --name")
			os.Exit(2)
		}
		sec, _, err := ks.Add(*name)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(sec)
		fmt.Printf("baseURL: http://%s/v1\nmodel: opencode/muse-spark-1.3-contributor-free\n", cfg.Listen)
	case "list":
		for _, e := range ks.List() {
			fmt.Printf("%s\t%s\tlast=%d\n", e.Name, e.Prefix, e.LastUsed)
		}
	case "revoke":
		fs := flag.NewFlagSet("key-revoke", flag.ContinueOnError)
		name := fs.String("name", "", "key name")
		fs.String("config", "", "config json path")
		fs.String("data-dir", "", "state dir")
		_ = fs.Parse(args[1:])
		if !ks.Revoke(*name) {
			fmt.Fprintln(os.Stderr, "not found")
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown key subcommand")
		os.Exit(2)
	}
}

func cmdLanes(args []string) {
	cfg := baseConfig(baseFlags(args))
	_, m := openDeps(cfg)
	for _, l := range m.Lanes() {
		fmt.Printf("lane %d cc=%s socks=%s healthy=%v active=%d score=%d\n",
			l.Index, l.Country, l.SocksAddr, l.Healthy, l.Active, l.Score)
	}
}

func cmdDemo(args []string) {
	q := "say hi in one line"
	if len(args) > 0 {
		q = strings.Join(args, " ")
	}
	_ = q
	fmt.Println("demo needs a running server; use: curl http://127.0.0.1:11434/v1/models")
}

func cmdTui(args []string) {
	cfg := baseConfig(args)
	_, m := openDeps(cfg)
	// lite tui: 3 refreshes of lane table, no external deps.
	for i := 0; i < 3; i++ {
		fmt.Printf("\033[H\033[2Jlanvello %s  %s\n\n", version, time.Now().Format(time.RFC3339))
		fmt.Println("lane  cc   socks            healthy active score")
		for _, l := range m.Lanes() {
			fmt.Printf("#%-4d %-4s %-16s %-7v %-6d %d\n", l.Index, l.Country, l.SocksAddr, l.Healthy, l.Active, l.Score)
		}
		if i < 2 {
			time.Sleep(2 * time.Second)
		}
	}
}
