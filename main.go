// Command agentd is a small, multi-tenant agent runtime: a daemon that runs LLM
// agents with tools from MCP, HTTP, SMTP and IMAP connectors, triggered via
// HTTP, MCP, webhooks, schedules or incoming email.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/agent"
	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/app"
	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/config"
	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/server"
	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/store"
	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/tenant"
)

var version = "dev"

// defaultConfig is used when neither a config file nor AGENT_CONFIG_YAML is
// available (e.g. buildpack images, which drop the source tree). It is
// driven entirely by environment variables.
//
//go:embed agents.yaml
var defaultConfig []byte

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", os.Getenv("AGENT_CONFIG"), "path to config file (env AGENT_CONFIG; or inline YAML in AGENT_CONFIG_YAML; default ./agents.yaml, then the built-in config)")
	check := flag.Bool("check", false, "validate the config, connect connectors, list tools, then exit")
	chat := flag.String("chat", "", "interactive terminal chat with the named agent")
	once := flag.String("run", "", "run the named agent once with -input (or stdin) and print the answer")
	input := flag.String("input", "", "input for -run")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}
	server.Version = version

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log, *chat != "" || *once != "")
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var a *app.App
	if len(cfg.Agents) > 0 || !cfg.Tenancy.Enabled {
		if a, err = app.Build(ctx, cfg, log); err != nil {
			return err
		}
		defer a.Close()
	}

	if a == nil && (*check || *once != "" || *chat != "") {
		return errors.New("-check, -run and -chat need agents in the config file")
	}
	switch {
	case *check:
		for name, ag := range a.Agents {
			fmt.Printf("agent %s (provider %s)\n", name, ag.Config().Provider)
			for _, t := range ag.Tools() {
				fmt.Printf("  - %s: %s\n", t.Name, firstLine(t.Description))
			}
		}
		fmt.Println("config OK")
		return nil
	case *once != "":
		text := *input
		if text == "" {
			b, _ := readAll(os.Stdin)
			text = b
		}
		res, err := a.Run(ctx, *once, agent.RunInput{Input: text})
		if err != nil {
			return err
		}
		fmt.Println(res.Output)
		return nil
	case *chat != "":
		return chatLoop(ctx, a, *chat)
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	if st != nil {
		defer st.Close()
	}
	m, err := tenant.New(ctx, cfg, st, a, log)
	if err != nil {
		return err
	}
	if len(cfg.Server.Tokens()) == 0 {
		if cfg.Tenancy.Enabled {
			log.Warn("server.auth_token is not set: admin API disabled")
		} else {
			log.Warn("server.auth_token is not set: the HTTP API is UNAUTHENTICATED")
		}
	}
	var wg sync.WaitGroup
	m.Start(ctx, &wg)

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           server.New(ctx, m, cfg),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Server.Addr, "version", version, "system_agents", len(cfg.Agents), "multi_tenant", cfg.Tenancy.Enabled)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}
	wg.Wait()
	return nil
}

func newLogger(c config.LogConfig, quiet bool) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(c.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	if quiet && c.Level == "" {
		level = slog.LevelWarn
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" || quiet {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
}

func readAll(f *os.File) (string, error) {
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
	}
	return b.String(), sc.Err()
}

func chatLoop(ctx context.Context, a *app.App, name string) error {
	if _, ok := a.Agents[name]; !ok {
		return fmt.Errorf("unknown agent %q", name)
	}
	session := agent.NewSessionID()
	fmt.Printf("Chatting with %s (Ctrl-D to quit)\n", name)
	sc := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !sc.Scan() {
			fmt.Println()
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		res, err := a.Run(ctx, name, agent.RunInput{Input: line, SessionID: session, OnEvent: func(e agent.Event) {
			if e.Type == "tool_call" {
				fmt.Printf("  ⚙ %s %s\n", e.Tool, truncate(string(e.Args), 120))
			}
		}})
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Println(res.Output)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func loadConfig(path string) (*config.Config, error) {
	if path != "" || os.Getenv("AGENT_CONFIG_YAML") != "" {
		return config.Load(path)
	}
	if _, err := os.Stat("agents.yaml"); err == nil {
		return config.Load("agents.yaml")
	}
	return config.Parse(defaultConfig)
}

func openStore(ctx context.Context, cfg *config.Config) (store.Store, error) {
	t := cfg.Tenancy
	if !t.Enabled {
		return nil, nil
	}
	switch t.Store.Type {
	case "postgres":
		return store.NewPostgres(ctx, t.Store.URL, t.SessionTTLDur())
	case "file":
		return store.NewMem(t.Store.Path, t.SessionTTLDur(), cfg.Memory.MaxSessions)
	default:
		return store.NewMem("", t.SessionTTLDur(), cfg.Memory.MaxSessions)
	}
}
