package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"ggsearch/internal/config"
	"ggsearch/internal/manager"
	"ggsearch/internal/server"
	"ggsearch/internal/store"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var rest []string
	if len(os.Args) > 2 {
		rest = os.Args[2:]
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var dbPath, addr string
	fs.StringVar(&dbPath, "db", "", "path to SQLite db (env GGSP_DB, default data/ggsearch.db)")
	fs.StringVar(&addr, "addr", "", "listen address (env GGSP_ADDR, default :20129)")
	_ = fs.Parse(rest)

	cfg := config.Load()
	if dbPath != "" {
		cfg.DBPath = dbPath
	}
	if addr != "" {
		cfg.Addr = addr
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch cmd {
	case "serve":
		runServe(cfg, log)
	case "login":
		runLogin(cfg, log)
	case "import-9router":
		path := fs.Arg(0)
		if path == "" {
			path = default9RouterDB()
		}
		runImport(cfg, log, path)
	case "accounts":
		runAccounts(cfg, log)
	case "keys":
		runKeys(cfg, log, fs.Args())
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`ggsearch — self-hosted Antigravity web search API

Usage:
  ggsearch serve [-addr :20129] [-db data/ggsearch.db]   start the API server
  ggsearch login [-db ...]                               add a Google account via OAuth
  ggsearch import-9router [path/to/9router.db] [-db ...] import Antigravity accounts from 9router
  ggsearch accounts [-db ...]                            list connected accounts
  ggsearch keys create <label> [-db ...]                 create an API key
  ggsearch keys list [-db ...]                           list API keys

Environment:
  GGSP_ADDR         listen address (default :20129)
  GGSP_DB           sqlite path (default data/ggsearch.db)
  GGSP_ADMIN_TOKEN  enables /v1 admin endpoints
  GGSP_UPSTREAM_TIMEOUT  upstream timeout (default 20s)`)
}

func default9RouterDB() string {
	// os.UserConfigDir resolves per-OS:
	//   windows: %AppData%\9router\db\data.sqlite
	//   darwin:  ~/Library/Application Support/9router/db/data.sqlite
	//   linux:   ~/.config/9router/db/data.sqlite
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "9router", "db", "data.sqlite")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "9router", "db", "data.sqlite")
}

func openStore(cfg *config.Config) *store.Store {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	return st
}

func runServe(cfg *config.Config, log *slog.Logger) {
	st := openStore(cfg)
	defer st.Close()

	adminToken, tokenSource := resolveAdminToken(cfg)
	if adminToken == "" {
		fmt.Fprintln(os.Stderr, "error: could not create an admin token")
		os.Exit(1)
	}

	mgr := manager.New(st, log)
	handler := server.Handler(server.Deps{
		Store: st, Manager: mgr, Log: log,
		AdminToken: adminToken, DefaultModel: cfg.DefaultModel,
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		fmt.Printf("ggsearch listening on http://localhost%s\n", displayAddr(cfg.Addr))
		fmt.Printf("admin endpoints: /v1/keys /v1/accounts /v1/usage\n")
		fmt.Printf("admin token (%s): %s\n", tokenSource, adminToken)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	fmt.Println("\nshutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	return "/" + addr
}

// resolveAdminToken returns the admin token and where it came from:
//  1. GGSP_ADMIN_TOKEN (.env or environment) — explicit wins
//  2. <db-dir>/admin-token file — persisted auto-generated token
//  3. generate a random one and persist it next to the database
//
// To rotate: change GGSP_ADMIN_TOKEN in .env, or delete the admin-token file.
func resolveAdminToken(cfg *config.Config) (string, string) {
	if cfg.AdminToken != "" {
		return cfg.AdminToken, "from GGSP_ADMIN_TOKEN"
	}
	path := filepath.Join(filepath.Dir(cfg.DBPath), "admin-token")
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, "from " + path + " — delete file to rotate"
		}
	}
	t := "gsk-admin-" + store.NewAPISecret()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if err := os.WriteFile(path, []byte(t), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not persist admin token: %v\n", err)
		}
	}
	return t, "auto-generated, saved to " + path
}

func runLogin(cfg *config.Config, log *slog.Logger) {
	st := openStore(cfg)
	defer st.Close()
	mgr := manager.New(st, log)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	_, err := mgr.Login(ctx, openBrowser)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		os.Exit(1)
	}
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func runImport(cfg *config.Config, log *slog.Logger, dbPath string) {
	st := openStore(cfg)
	defer st.Close()
	mgr := manager.New(st, log)
	n, err := mgr.Import9Router(context.Background(), dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("imported %d antigravity account(s) from %s\n", n, dbPath)
}

func runAccounts(cfg *config.Config, log *slog.Logger) {
	st := openStore(cfg)
	defer st.Close()
	accounts, err := st.ListAccounts(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "list: %v\n", err)
		os.Exit(1)
	}
	if len(accounts) == 0 {
		fmt.Println("no accounts. run: ggsearch login  (or ggsearch import-9router)")
		return
	}
	for _, a := range accounts {
		status := "active"
		if !a.IsActive {
			status = "disabled"
		}
		fmt.Printf("%s  %-32s  project=%s  [%s]\n", a.ID[:8], a.Email, a.ProjectID, status)
	}
}

func runKeys(cfg *config.Config, log *slog.Logger, args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	st := openStore(cfg)
	defer st.Close()
	switch sub := args[0]; sub {
	case "create":
		label := ""
		if len(args) > 1 {
			label = args[1]
		}
		full, _, _ := store.NewAPIKeyMaterial()
		k, err := st.CreateAPIKey(context.Background(), label, full)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create key: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("API key created (label: %s, id: %s)\n  %s\n\nStore it now — it will not be shown again.\n", k.Label, k.ID, full)
	case "list":
		keys, err := st.ListAPIKeys(context.Background())
		if err != nil {
			fmt.Fprintf(os.Stderr, "list keys: %v\n", err)
			os.Exit(1)
		}
		if len(keys) == 0 {
			fmt.Println("no keys. run: ggsearch keys create <label>")
			return
		}
		for _, k := range keys {
			fmt.Printf("%s  %s…  label=%s  created=%s\n", k.ID[:8], k.Prefix, k.Label, k.CreatedAt.Format("2006-01-02"))
		}
	default:
		usage()
		os.Exit(2)
	}
}
