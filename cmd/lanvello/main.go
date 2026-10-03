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

const version = "0.1.0"

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
  lanvello serve [--listen 127.0.0.1:11434] [--lanes 5] [--no-tor] [--config file.json]
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
	noTor := fs.Bool("no-tor", false, "skip tor")
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
	if *noTor {
		cfg.NoTor = true
	}
	if os.Getenv("LANVELLO_NO_TOR") == "1" {
		cfg.NoTor = true
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
	m := lanes.New(cfg.DataDir, pri, fb, cfg.Socks, cfg.NoTor, cfg.Lanes)
	return ks, m
}

func cmdServe(args []string) {
	cfg := baseConfig(args)
	ks, m := openDeps(cfg)
	if !cfg.NoTor && len(cfg.Socks) == 0 {
		tor.Ensure(m, cfg.DataDir, 52001, 52301, 60*time.Second)
	}
	up := upstream.NewClient(m)
	up.BaseURL = cfg.BaseURL
	up.WaitBudget = time.Duration(cfg.WaitBudgetS * float64(time.Second))
	up.WantCountry = map[string]string{"union-alpha": "us"}
	for pref, cc := range cfg.ModelLanes {
		up.WantCountry[pref] = cc
	}
	srv := server.New(ks, m, up)
	fmt.Printf("lanvello %s listening on http://%s lanes=%d tor=%v free-only, no login\n", version, cfg.Listen, cfg.Lanes, !cfg.NoTor)
	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func cmdKey(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: lanvello key add|list|revoke")
		os.Exit(2)
	}
	cfg := baseConfig([]string{})
	ks, _ := openDeps(cfg)
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("key-add", flag.ContinueOnError)
		name := fs.String("name", "", "key name")
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
	cfg := baseConfig([]string{})
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
