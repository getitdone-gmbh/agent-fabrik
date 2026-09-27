package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/mcp"
)

// MCPConfig configures an MCP server connection. Set URL for streamable
// HTTP, or Command for a stdio subprocess.
type MCPConfig struct {
	URL        string            `yaml:"url"`
	Headers    map[string]string `yaml:"headers"`
	Command    []string          `yaml:"command"`
	Env        map[string]string `yaml:"env"`
	Dir        string            `yaml:"dir"`
	Include    []string          `yaml:"include"` // tool name globs to expose (default all)
	Exclude    []string          `yaml:"exclude"`
	TimeoutSec int               `yaml:"timeout_seconds"`

	// Set by the runtime for tenant connectors.
	Transport    http.RoundTripper `yaml:"-"`
	DenyCommands bool              `yaml:"-"`
}

// MCPConnector exposes the tools of an MCP server.
type MCPConnector struct {
	name string
	cfg  MCPConfig
	log  *slog.Logger

	mu     sync.Mutex
	client *mcp.Client
}

func NewMCPConnector(name string, cfg MCPConfig, log *slog.Logger) (*MCPConnector, error) {
	if (cfg.URL == "") == (len(cfg.Command) == 0) {
		return nil, fmt.Errorf("mcp connector %q: set exactly one of url or command", name)
	}
	if cfg.DenyCommands && len(cfg.Command) > 0 {
		return nil, fmt.Errorf("mcp connector %q: stdio commands are not allowed here; use an HTTP MCP server (url)", name)
	}
	return &MCPConnector{name: name, cfg: cfg, log: log}, nil
}

func (m *MCPConnector) Name() string { return m.name }

func (m *MCPConnector) connect(ctx context.Context) (*mcp.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil {
		return m.client, nil
	}
	var c *mcp.Client
	if m.cfg.URL != "" {
		c = mcp.NewHTTPClient(m.cfg.URL, m.cfg.Headers, time.Duration(m.cfg.TimeoutSec)*time.Second, m.cfg.Transport)
	} else {
		var err error
		c, err = mcp.NewStdioClient(m.cfg.Command, m.cfg.Env, m.cfg.Dir, m.log)
		if err != nil {
			return nil, err
		}
	}
	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.Initialize(ictx); err != nil {
		c.Close()
		return nil, err
	}
	m.client = c
	return c, nil
}

func (m *MCPConnector) reset(c *mcp.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client == c {
		m.client.Close()
		m.client = nil
	}
}

func (m *MCPConnector) allowed(name string) bool {
	for _, p := range m.cfg.Exclude {
		if ok, _ := path.Match(p, name); ok {
			return false
		}
	}
	if len(m.cfg.Include) == 0 {
		return true
	}
	for _, p := range m.cfg.Include {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func (m *MCPConnector) Tools(ctx context.Context) ([]Tool, error) {
	c, err := m.connect(ctx)
	if err != nil {
		return nil, err
	}
	defs, err := c.ListTools(ctx)
	if err != nil {
		m.reset(c)
		return nil, err
	}
	var out []Tool
	for _, d := range defs {
		if !m.allowed(d.Name) {
			continue
		}
		name := d.Name
		desc := d.Description
		if desc == "" {
			desc = d.Title
		}
		out = append(out, Tool{
			Local:       name,
			Description: desc,
			Schema:      d.InputSchema,
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				return m.call(ctx, name, args)
			},
		})
	}
	return out, nil
}

func (m *MCPConnector) call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		c, err := m.connect(ctx)
		if err != nil {
			return "", err
		}
		res, err := c.CallTool(ctx, name, args)
		if errors.Is(err, mcp.ErrClosed) && attempt == 0 {
			m.reset(c) // subprocess died: restart once
			continue
		}
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "", errors.New(res.Text())
		}
		return res.Text(), nil
	}
	return "", mcp.ErrClosed
}

func (m *MCPConnector) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil {
		err := m.client.Close()
		m.client = nil
		return err
	}
	return nil
}
