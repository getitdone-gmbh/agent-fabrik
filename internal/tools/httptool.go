package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/netguard"
)

// HTTPConfig configures an HTTP connector. It can expose declared endpoints
// as individual typed tools and/or a generic "request" tool limited to an
// allowlist of hosts.
type HTTPConfig struct {
	BaseURL          string            `yaml:"base_url"`
	Headers          map[string]string `yaml:"headers"`
	AllowHosts       []string          `yaml:"allow_hosts"`
	AllowMethods     []string          `yaml:"allow_methods"`
	AllowPrivate     bool              `yaml:"allow_private"`
	TimeoutSec       int               `yaml:"timeout_seconds"`
	MaxResponseBytes int               `yaml:"max_response_bytes"`
	Endpoints        []Endpoint        `yaml:"endpoints"`

	// Transport, when set (tenant connectors), is used for all requests
	// including configured endpoints, and AllowPrivate is ignored.
	Transport http.RoundTripper `yaml:"-"`
}

// Endpoint is a declared API operation exposed as its own tool.
type Endpoint struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Method      string            `yaml:"method"`
	Path        string            `yaml:"path"` // may contain {param} placeholders
	Params      map[string]Param  `yaml:"params"`
	Headers     map[string]string `yaml:"headers"`
}

// Param is an endpoint parameter.
type Param struct {
	Type        string   `yaml:"type"` // string, integer, number, boolean, object, array
	Description string   `yaml:"description"`
	Required    bool     `yaml:"required"`
	In          string   `yaml:"in"` // path, query, body (default: path if in template, body for POST/PUT/PATCH, else query)
	Enum        []string `yaml:"enum"`
}

// HTTPConnector implements Connector for HTTP APIs.
type HTTPConnector struct {
	name    string
	cfg     HTTPConfig
	base    *url.URL
	trusted *http.Client // for configured endpoints
	generic *http.Client // for model-chosen URLs
}

func NewHTTPConnector(name string, cfg HTTPConfig) (*HTTPConnector, error) {
	c := &HTTPConnector{name: name, cfg: cfg}
	if cfg.BaseURL != "" {
		u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("http connector %q: invalid base_url %q", name, cfg.BaseURL)
		}
		c.base = u
	}
	if c.cfg.MaxResponseBytes == 0 {
		c.cfg.MaxResponseBytes = 64 << 10
	}
	timeout := 30 * time.Second
	if cfg.TimeoutSec > 0 {
		timeout = time.Duration(cfg.TimeoutSec) * time.Second
	}
	// The trusted client carries the connector's credentials, so it never
	// leaves the base host: Go would re-send custom headers like X-Api-Key
	// to any redirect target.
	c.trusted = &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !c.onBase(req.URL) {
				return fmt.Errorf("redirect to %s://%s is not allowed", req.URL.Scheme, req.URL.Host)
			}
			return nil
		},
	}
	if cfg.Transport != nil {
		c.cfg.AllowPrivate = false
		c.trusted.Transport = cfg.Transport
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var genericRT http.RoundTripper = &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second}
	if cfg.Transport != nil {
		genericRT = cfg.Transport
	} else if !cfg.AllowPrivate {
		genericRT = netguard.Transport
	}
	c.generic = &http.Client{
		Timeout:   timeout,
		Transport: genericRT,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if !c.hostAllowed(req.URL) {
				return fmt.Errorf("redirect to %s is not allowed", req.URL.Host)
			}
			return nil
		},
	}
	return c, nil
}

func (c *HTTPConnector) Name() string { return c.name }
func (c *HTTPConnector) Close() error { return nil }

// onBase reports whether u has the base URL's scheme and host, the only
// place the connector's credentials may go.
func (c *HTTPConnector) onBase(u *url.URL) bool {
	return c.base != nil && strings.EqualFold(u.Scheme, c.base.Scheme) && strings.EqualFold(u.Host, c.base.Host)
}

func (c *HTTPConnector) hostAllowed(u *url.URL) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if c.base != nil && strings.EqualFold(c.base.Hostname(), host) {
		return true
	}
	for _, p := range c.cfg.AllowHosts {
		p = strings.ToLower(p)
		if p == "*" || p == host {
			return true
		}
		if ok, _ := path.Match(p, host); ok {
			return true
		}
	}
	return false
}

func (c *HTTPConnector) methodAllowed(m string) bool {
	if len(c.cfg.AllowMethods) == 0 {
		return true
	}
	for _, a := range c.cfg.AllowMethods {
		if strings.EqualFold(a, m) {
			return true
		}
	}
	return false
}

func (c *HTTPConnector) Tools(context.Context) ([]Tool, error) {
	var out []Tool
	for _, ep := range c.cfg.Endpoints {
		if c.base == nil {
			return nil, fmt.Errorf("http connector %q: endpoints require base_url", c.name)
		}
		out = append(out, c.endpointTool(ep))
	}
	if len(c.cfg.AllowHosts) > 0 || (c.base != nil && len(c.cfg.Endpoints) == 0) {
		out = append(out, c.genericTool())
	}
	return out, nil
}

func (c *HTTPConnector) genericTool() Tool {
	desc := "Performs an HTTP request and returns status, content type and (truncated) body."
	var hosts []string
	if c.base != nil {
		hosts = append(hosts, c.base.Host)
		desc += " Relative URLs resolve against " + c.base.String() + "."
	}
	hosts = append(hosts, c.cfg.AllowHosts...)
	desc += " Allowed hosts: " + strings.Join(hosts, ", ") + "."
	return Tool{
		Local:       "request",
		Description: desc,
		Schema: Schema(map[string]any{
			"method":  map[string]any{"type": "string", "description": "HTTP method", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}},
			"url":     Prop("string", "Absolute URL, or a path relative to the base URL"),
			"headers": map[string]any{"type": "object", "description": "Extra request headers", "additionalProperties": map[string]any{"type": "string"}},
			"body":    Prop("string", "Request body (send JSON as a string)"),
		}, "url"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Method  string            `json:"method"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
			}
			if err := Decode(args, &a); err != nil {
				return "", err
			}
			if a.Method == "" {
				a.Method = http.MethodGet
			}
			a.Method = strings.ToUpper(a.Method)
			if !c.methodAllowed(a.Method) {
				return "", fmt.Errorf("method %s is not allowed", a.Method)
			}
			u, err := url.Parse(a.URL)
			if err != nil {
				return "", err
			}
			if !u.IsAbs() && c.base != nil {
				q := u.RawQuery
				u = c.base.JoinPath(u.Path)
				u.RawQuery = q
			}
			if !c.hostAllowed(u) {
				return "", fmt.Errorf("host %q is not in the allowlist", u.Host)
			}
			// Connector credentials are only sent to the configured base host.
			headers := map[string]string{}
			if c.onBase(u) {
				for k, v := range c.cfg.Headers {
					headers[k] = v
				}
			}
			for k, v := range a.Headers {
				headers[k] = v
			}
			var body io.Reader
			if a.Body != "" {
				body = strings.NewReader(a.Body)
				if _, ok := headers["Content-Type"]; !ok && json.Valid([]byte(a.Body)) {
					headers["Content-Type"] = "application/json"
				}
			}
			client := c.generic
			if c.onBase(u) {
				client = c.trusted
			}
			return c.do(ctx, client, a.Method, u.String(), headers, body)
		},
	}
}

func (c *HTTPConnector) endpointTool(ep Endpoint) Tool {
	method := strings.ToUpper(ep.Method)
	if method == "" {
		method = http.MethodGet
	}
	props := map[string]any{}
	var required []string
	names := make([]string, 0, len(ep.Params))
	for n := range ep.Params {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := ep.Params[n]
		typ := p.Type
		if typ == "" {
			typ = "string"
		}
		s := map[string]any{"type": typ}
		if p.Description != "" {
			s["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			s["enum"] = p.Enum
		}
		if typ == "array" {
			s["items"] = map[string]any{}
		}
		props[n] = s
		if p.Required || strings.Contains(ep.Path, "{"+n+"}") {
			required = append(required, n)
		}
	}
	desc := ep.Description
	if desc == "" {
		desc = method + " " + ep.Path
	}
	return Tool{
		Local:       ep.Name,
		Description: desc,
		Schema:      Schema(props, required...),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a map[string]any
			if err := Decode(args, &a); err != nil {
				return "", err
			}
			p := ep.Path
			query := url.Values{}
			bodyParams := map[string]any{}
			hasBody := method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
			for k, v := range a {
				spec, known := ep.Params[k]
				if !known {
					continue
				}
				in := spec.In
				if in == "" {
					switch {
					case strings.Contains(p, "{"+k+"}"):
						in = "path"
					case hasBody:
						in = "body"
					default:
						in = "query"
					}
				}
				switch in {
				case "path":
					p = strings.ReplaceAll(p, "{"+k+"}", url.PathEscape(fmt.Sprint(v)))
				case "query":
					query.Set(k, fmt.Sprint(v))
				default:
					bodyParams[k] = v
				}
			}
			if strings.Contains(p, "{") {
				return "", fmt.Errorf("missing path parameters in %s", p)
			}
			rel, err := url.Parse(p)
			if err != nil {
				return "", err
			}
			u := c.base.JoinPath(rel.Path)
			q := rel.Query()
			for k, vs := range query {
				q[k] = vs
			}
			u.RawQuery = q.Encode()
			headers := map[string]string{}
			for k, v := range c.cfg.Headers {
				headers[k] = v
			}
			for k, v := range ep.Headers {
				headers[k] = v
			}
			var body io.Reader
			if hasBody && len(bodyParams) > 0 {
				b, _ := json.Marshal(bodyParams)
				body = bytes.NewReader(b)
				headers["Content-Type"] = "application/json"
			}
			return c.do(ctx, c.trusted, method, u.String(), headers, body)
		},
	}
}

func (c *HTTPConnector) do(ctx context.Context, client *http.Client, method, u string, headers map[string]string, body io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "agentd/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	limit := int64(c.cfg.MaxResponseBytes)
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	truncated := ""
	if int64(len(data)) > limit {
		data = data[:limit]
		truncated = "\n…(truncated)"
	}
	return fmt.Sprintf("HTTP %d\nContent-Type: %s\n\n%s%s", resp.StatusCode, resp.Header.Get("Content-Type"), data, truncated), nil
}
