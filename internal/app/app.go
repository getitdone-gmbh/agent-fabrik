// Package app wires configuration into providers, connectors and agents.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/agent"
	"github.com/getitdone-gmbh/agent-fabrik/internal/config"
	"github.com/getitdone-gmbh/agent-fabrik/internal/llm"
	"github.com/getitdone-gmbh/agent-fabrik/internal/mail"
	"github.com/getitdone-gmbh/agent-fabrik/internal/netguard"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tools"
)

// App is the running set of agents.
type App struct {
	Config     *config.Config
	Log        *slog.Logger
	Registry   *tools.Registry
	Store      agent.Store
	Agents     map[string]*agent.Agent
	Connectors map[string]tools.Connector
	sem        chan struct{}
}

// optional marks a connector whose startup failure is logged instead of fatal.
type connectorCommon struct {
	Optional bool `yaml:"optional"`
}

// Options customize how an app is built (used for tenant runtimes).
type Options struct {
	// Guarded routes all tenant-controlled networking through netguard and
	// forbids stdio subprocesses.
	Guarded bool
	// Shared providers usable by agents in addition to cfg.Providers.
	Shared map[string]llm.Provider
	// Store overrides the session store from cfg.Memory.
	Store agent.Store
	// MaxConcurrentRuns overrides cfg.Server.MaxConcurrentRuns.
	MaxConcurrentRuns int
	// ConnectorsOptional logs connector failures instead of failing.
	ConnectorsOptional bool
	// MaxSteps caps every agent's max_steps (0 = no cap).
	MaxSteps int
	// Files, when set, adds the built-in "files" connector (tenant storage).
	Files tools.FilesBackend
}

// Build constructs the app from config.
func Build(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	return BuildWith(ctx, cfg, log, Options{})
}

// BuildWith constructs the app with options.
func BuildWith(ctx context.Context, cfg *config.Config, log *slog.Logger, opts Options) (*App, error) {
	conc := cfg.Server.MaxConcurrentRuns
	if opts.MaxConcurrentRuns > 0 {
		conc = opts.MaxConcurrentRuns
	}
	if conc <= 0 {
		conc = 16
	}
	a := &App{
		Config:     cfg,
		Log:        log,
		Registry:   tools.NewRegistry(),
		Agents:     map[string]*agent.Agent{},
		Connectors: map[string]tools.Connector{},
		sem:        make(chan struct{}, conc),
	}

	providers := map[string]llm.Provider{}
	for name, p := range opts.Shared {
		providers[name] = p
	}
	for name, pc := range cfg.Providers {
		if opts.Guarded {
			pc.Transport = netguard.TenantTransport
		}
		p, err := llm.New(name, pc)
		if err != nil {
			return nil, err
		}
		providers[name] = p
	}

	if err := a.Registry.Add(ctx, tools.NewBuiltin("system")); err != nil {
		return nil, err
	}
	if opts.Files != nil {
		if err := a.Registry.Add(ctx, tools.NewFiles("files", opts.Files)); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(cfg.Connectors))
	for n := range cfg.Connectors {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		cc := cfg.Connectors[name]
		var common connectorCommon
		_ = cc.Decode(&common)
		if common.Optional && cc.Type == "mcp" {
			// An optional MCP connector without url/command is simply not
			// configured (e.g. MCP_URL unset): skip it quietly, it is no fault.
			var target struct {
				URL     string   `yaml:"url"`
				Command []string `yaml:"command"`
			}
			if _ = cc.Decode(&target); target.URL == "" && len(target.Command) == 0 {
				log.Debug("optional connector not configured, skipped", "connector", name)
				continue
			}
		}
		conn, err := newConnector(name, cc, log, opts.Guarded)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err = a.Registry.Add(cctx, conn)
			cancel()
		}
		if err != nil {
			if common.Optional || opts.ConnectorsOptional {
				log.Warn("optional connector unavailable", "connector", name, "err", err)
				continue
			}
			a.Registry.Close()
			return nil, err
		}
		a.Connectors[name] = conn
	}

	switch {
	case opts.Store != nil:
		a.Store = opts.Store
	case cfg.Memory.Type == "" || cfg.Memory.Type == "memory":
		a.Store = agent.NewMemoryStore(cfg.Memory.TTLDuration(), cfg.Memory.MaxSessions)
	case cfg.Memory.Type == "file":
		fs, err := agent.NewFileStore(cfg.Memory.Dir, cfg.Memory.TTLDuration())
		if err != nil {
			return nil, err
		}
		a.Store = fs
		go func() {
			t := time.NewTicker(time.Hour)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fs.Sweep()
				}
			}
		}()
	default:
		return nil, fmt.Errorf("memory.type %q: use memory or file", cfg.Memory.Type)
	}

	for name, ac := range cfg.Agents {
		if opts.MaxSteps > 0 && (ac.MaxSteps <= 0 || ac.MaxSteps > opts.MaxSteps) {
			ac.MaxSteps = opts.MaxSteps
		}
		p, ok := providers[ac.Provider]
		if !ok {
			a.Registry.Close()
			return nil, fmt.Errorf("agent %q: provider %q not available", name, ac.Provider)
		}
		ag, err := agent.New(ac.Config, p, a.Registry, a.Store, log)
		if err != nil {
			a.Registry.Close()
			return nil, err
		}
		a.Agents[name] = ag
		log.Info("agent ready", "agent", name, "provider", ac.Provider, "model", ac.Model, "tools", len(ag.Tools()))
	}
	return a, nil
}

func newConnector(name string, cc config.Connector, log *slog.Logger, guarded bool) (tools.Connector, error) {
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if guarded {
		dial = netguard.DialContext
	}
	switch cc.Type {
	case "mcp":
		var c tools.MCPConfig
		if err := cc.Decode(&c); err != nil {
			return nil, err
		}
		if guarded {
			c.Transport, c.DenyCommands = netguard.TenantTransport, true
		}
		return tools.NewMCPConnector(name, c, log)
	case "http":
		var c tools.HTTPConfig
		if err := cc.Decode(&c); err != nil {
			return nil, err
		}
		if guarded {
			c.Transport = netguard.TenantTransport
		}
		return tools.NewHTTPConnector(name, c)
	case "email":
		var c tools.EmailConfig
		if err := cc.Decode(&c); err != nil {
			return nil, err
		}
		if c.SMTP != nil {
			c.SMTP.Dial = dial
		}
		if c.IMAP != nil {
			c.IMAP.Dial = dial
			if guarded {
				c.IMAP.MaxMessageBytes = config.TenantMaxMailBytes
			}
		}
		return tools.NewEmailConnector(name, c)
	case "smtp":
		var c struct {
			mail.SMTPConfig `yaml:",inline"`
			AllowRecipients []string `yaml:"allow_recipients"`
		}
		if err := cc.Decode(&c); err != nil {
			return nil, err
		}
		c.SMTPConfig.Dial = dial
		return tools.NewEmailConnector(name, tools.EmailConfig{SMTP: &c.SMTPConfig, AllowRecipients: c.AllowRecipients})
	case "imap":
		var c struct {
			mail.IMAPConfig `yaml:",inline"`
			ReadOnly        bool `yaml:"read_only"`
		}
		if err := cc.Decode(&c); err != nil {
			return nil, err
		}
		c.IMAPConfig.Dial = dial
		if guarded {
			c.IMAPConfig.MaxMessageBytes = config.TenantMaxMailBytes
		}
		return tools.NewEmailConnector(name, tools.EmailConfig{IMAP: &c.IMAPConfig, ReadOnly: c.ReadOnly})
	case "shell":
		if guarded {
			// Tenants share this process: a shell would reach every tenant's secrets.
			return nil, fmt.Errorf("connector %q: shell is not available in multi-tenant mode", name)
		}
		var c tools.ShellConfig
		if err := cc.Decode(&c); err != nil {
			return nil, err
		}
		log.Warn("shell connector enabled: agents can run commands on this machine", "connector", name, "workdir", c.Workdir, "allow", c.Allow)
		return tools.NewShellConnector(name, c)
	}
	return nil, fmt.Errorf("connector %q: unknown type %q (mcp, http, email, smtp, imap, shell)", name, cc.Type)
}

// Run executes an agent, bounded by the app's concurrency limit.
func (a *App) Run(ctx context.Context, name string, in agent.RunInput) (*agent.RunResult, error) {
	return a.RunThen(ctx, name, in, nil)
}

// RunThen is Run with a gate that is passed only after the app's own slot is
// held. The tenant manager uses it for the process-wide slots: taking those
// first let a tenant over its own cap park runs on them and starve everyone.
func (a *App) RunThen(ctx context.Context, name string, in agent.RunInput, gate func(context.Context) (func(), error)) (*agent.RunResult, error) {
	ag, ok := a.Agents[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", name)
	}
	select {
	case a.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-a.sem }()
	if gate != nil {
		release, err := gate(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	return ag.Run(ctx, in)
}

// Close releases connectors.
func (a *App) Close() { a.Registry.Close() }
