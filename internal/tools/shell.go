package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ShellConfig configures a shell connector: commands on the machine agentd
// runs on. It is meant for a single owner's own machine and is refused in
// multi-tenant mode, where all tenants share one process.
//
// Two modes:
//   - allow empty: the command line runs through the shell ("bash -c"), with
//     pipes, redirects and everything else. deny is a guardrail only.
//   - allow set: no shell. The command line is split into words and the
//     program is executed directly, only if its name matches allow. Pipes,
//     ";", "$(...)" and redirects are then plain arguments, not syntax.
type ShellConfig struct {
	Shell      string            `yaml:"shell"`           // default bash, else sh
	Workdir    string            `yaml:"workdir"`         // required; commands run inside it
	TimeoutSec int               `yaml:"timeout_seconds"` // per command, default 30
	MaxOutput  int               `yaml:"max_output"`      // bytes per stream, default 32 KiB
	Allow      []string          `yaml:"allow"`           // program globs, e.g. [git, ls, "python3*"]
	Deny       []string          `yaml:"deny"`            // command-line globs, e.g. ["sudo *", "*rm -rf /*"]
	Env        map[string]string `yaml:"env"`             // extra variables
	InheritEnv bool              `yaml:"inherit_env"`     // pass agentd's environment (API keys!) on
}

// ShellConnector implements Connector for local commands.
type ShellConnector struct {
	name    string
	cfg     ShellConfig
	shell   string
	workdir string
	timeout time.Duration
}

func NewShellConnector(name string, cfg ShellConfig) (*ShellConnector, error) {
	if cfg.Workdir == "" {
		return nil, fmt.Errorf("shell connector %q: workdir is required", name)
	}
	wd, err := filepath.Abs(cfg.Workdir)
	if err == nil {
		wd, err = filepath.EvalSymlinks(wd)
	}
	if err != nil {
		return nil, fmt.Errorf("shell connector %q: workdir: %w", name, err)
	}
	if st, err := os.Stat(wd); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("shell connector %q: workdir %q is not a directory", name, cfg.Workdir)
	}
	c := &ShellConnector{name: name, cfg: cfg, workdir: wd, timeout: 30 * time.Second}
	if cfg.TimeoutSec > 0 {
		c.timeout = time.Duration(cfg.TimeoutSec) * time.Second
	}
	if c.cfg.MaxOutput <= 0 {
		c.cfg.MaxOutput = 32 << 10
	}
	if len(cfg.Allow) == 0 {
		c.shell = cfg.Shell
		if c.shell == "" {
			if p, err := exec.LookPath("bash"); err == nil {
				c.shell = p
			} else {
				c.shell = "/bin/sh"
			}
		}
	}
	for _, p := range append(append([]string{}, cfg.Allow...), cfg.Deny...) {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("shell connector %q: invalid pattern %q", name, p)
		}
	}
	return c, nil
}

func (c *ShellConnector) Name() string { return c.name }
func (c *ShellConnector) Close() error { return nil }

func (c *ShellConnector) Tools(context.Context) ([]Tool, error) {
	desc := fmt.Sprintf("Runs a command on the local machine in %s and returns exit code, stdout and stderr. Timeout %s.", c.workdir, c.timeout)
	if c.shell != "" {
		desc += " The command line is interpreted by " + filepath.Base(c.shell) + " (pipes and redirects work)."
	} else {
		desc += " No shell: pipes, redirects and ; do not work. Allowed programs: " + strings.Join(c.cfg.Allow, ", ") + "."
	}
	return []Tool{{
		Local:       "run",
		Description: desc,
		Schema: Schema(map[string]any{
			"command": Prop("string", "Command line to run"),
			"cwd":     Prop("string", "Directory relative to the working directory (optional)"),
		}, "command"),
		Handler: c.run,
	}}, nil
}

type shellResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

func (c *ShellConnector) run(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := Decode(args, &a); err != nil {
		return "", err
	}
	line := strings.TrimSpace(a.Command)
	if line == "" {
		return "", errors.New("command is empty")
	}
	for _, p := range c.cfg.Deny {
		if ok, _ := path.Match(p, line); ok {
			return "", fmt.Errorf("command denied by policy (%s)", p)
		}
	}
	dir, err := c.dir(a.Cwd)
	if err != nil {
		return "", err
	}

	var argv []string
	if c.shell != "" {
		argv = []string{c.shell, "-c", line}
	} else {
		if argv, err = SplitWords(line); err != nil {
			return "", err
		}
		if !c.allowed(argv[0]) {
			return "", fmt.Errorf("program %q is not allowed (allowed: %s)", argv[0], strings.Join(c.cfg.Allow, ", "))
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = c.env()
	cmd.Stdin = nil
	stdout := &capped{max: c.cfg.MaxOutput}
	stderr := &capped{max: c.cfg.MaxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		// Kill the whole group: "bash -c 'sleep 999 &'" must not outlive us.
		killProcessGroup(cmd)
		waitErr = <-done
	}

	res := shellResult{
		Stdout:    stdout.buf.String(),
		Stderr:    stderr.buf.String(),
		Truncated: stdout.cut || stderr.cut,
		TimedOut:  ctx.Err() != nil,
	}
	var exit *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exit):
		res.ExitCode = exit.ExitCode()
	default:
		return "", waitErr
	}
	if res.TimedOut {
		res.ExitCode = -1
	}
	return JSON(res), nil
}

// dir resolves cwd inside the working directory; symlinks may not leave it.
func (c *ShellConnector) dir(rel string) (string, error) {
	if rel == "" {
		return c.workdir, nil
	}
	if filepath.IsAbs(rel) {
		return "", errors.New("cwd must be relative to the working directory")
	}
	d, err := filepath.EvalSymlinks(filepath.Join(c.workdir, rel))
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	if d != c.workdir && !strings.HasPrefix(d, c.workdir+string(filepath.Separator)) {
		return "", errors.New("cwd is outside the working directory")
	}
	return d, nil
}

func (c *ShellConnector) allowed(prog string) bool {
	for _, p := range c.cfg.Allow {
		if ok, _ := path.Match(p, prog); ok {
			return true
		}
	}
	return false
}

// env is minimal by default so agentd's own secrets (LLM keys, runtime
// token) do not reach commands the model chooses.
func (c *ShellConnector) env() []string {
	var env []string
	if c.cfg.InheritEnv {
		env = os.Environ()
	} else {
		for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ", "TMPDIR"} {
			if v, ok := os.LookupEnv(k); ok {
				env = append(env, k+"="+v)
			}
		}
	}
	for k, v := range c.cfg.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// capped keeps the first max bytes and discards the rest.
type capped struct {
	buf bytes.Buffer
	max int
	cut bool
}

func (w *capped) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.cut = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.cut = true
	}
	return len(p), nil
}

// SplitWords splits a command line into words like a POSIX shell would for
// quoting ('...', "...", backslash), without expanding anything.
func SplitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			}
			inWord = true
		case ch == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case ch == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unterminated double quote")
			}
			inWord = true
		case ch == ' ' || ch == '\t' || ch == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		return nil, errors.New("command is empty")
	}
	return words, nil
}
