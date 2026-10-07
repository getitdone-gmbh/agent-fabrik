//go:build !windows

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runShell(t *testing.T, c *ShellConnector, args string) (shellResult, error) {
	t.Helper()
	ts, _ := c.Tools(context.Background())
	out, err := ts[0].Handler(context.Background(), json.RawMessage(args))
	var r shellResult
	if err == nil {
		if jerr := json.Unmarshal([]byte(out), &r); jerr != nil {
			t.Fatal(jerr, out)
		}
	}
	return r, err
}

func TestShellFullMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTD_SECRET_FOR_TEST", "leak")
	c, err := NewShellConnector("sh", ShellConfig{Workdir: dir, Deny: []string{"sudo *"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runShell(t, c, `{"command":"echo hi | tr a-z A-Z; pwd; echo err >&2; exit 3"}`)
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if r.ExitCode != 3 || r.Stdout != "HI\n"+real+"\n" || r.Stderr != "err\n" {
		t.Errorf("%+v", r)
	}
	if r, _ := runShell(t, c, `{"command":"echo \"[$AGENTD_SECRET_FOR_TEST]\""}`); r.Stdout != "[]\n" {
		t.Errorf("environment leaked: %q", r.Stdout)
	}
	if _, err := runShell(t, c, `{"command":"sudo ls"}`); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("deny: %v", err)
	}
}

func TestShellAllowModeHasNoShell(t *testing.T) {
	dir := t.TempDir()
	c, err := NewShellConnector("sh", ShellConfig{Workdir: dir, Allow: []string{"echo", "ls"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runShell(t, c, `{"command":"echo 'a b' ; rm -rf x $(id)"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stdout != "a b ; rm -rf x $(id)\n" {
		t.Errorf("metacharacters must stay literal: %q", r.Stdout)
	}
	if _, err := runShell(t, c, `{"command":"rm -rf /"}`); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("allow: %v", err)
	}
	if _, err := runShell(t, c, `{"command":"/bin/rm x"}`); err == nil {
		t.Error("a path must not slip past allow")
	}
}

func TestShellCwdStaysInside(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.Symlink("/", filepath.Join(dir, "escape"))
	c, _ := NewShellConnector("sh", ShellConfig{Workdir: dir})
	if r, err := runShell(t, c, `{"command":"pwd","cwd":"sub"}`); err != nil || !strings.HasSuffix(r.Stdout, "/sub\n") {
		t.Errorf("%+v %v", r, err)
	}
	for _, cwd := range []string{"..", "/etc", "escape"} {
		if _, err := runShell(t, c, `{"command":"pwd","cwd":"`+cwd+`"}`); err == nil {
			t.Errorf("cwd %q escaped", cwd)
		}
	}
}

func TestShellTimeoutKillsGroupAndCapsOutput(t *testing.T) {
	c, _ := NewShellConnector("sh", ShellConfig{Workdir: t.TempDir(), TimeoutSec: 1, MaxOutput: 10})
	start := time.Now()
	r, err := runShell(t, c, `{"command":"sleep 30 & echo 0123456789abcdef; wait"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !r.TimedOut || r.ExitCode != -1 || !r.Truncated || r.Stdout != "0123456789" {
		t.Errorf("%+v", r)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("background child kept the call alive for %s", d)
	}
}

func TestShellConfigErrors(t *testing.T) {
	if _, err := NewShellConnector("sh", ShellConfig{}); err == nil {
		t.Error("workdir required")
	}
	if _, err := NewShellConnector("sh", ShellConfig{Workdir: "/does/not/exist"}); err == nil {
		t.Error("missing workdir")
	}
}

func TestSplitWords(t *testing.T) {
	got, err := SplitWords(`git commit -m "fix: \"a\" b" 'c d' e\ f`)
	if err != nil || !reflect.DeepEqual(got, []string{"git", "commit", "-m", `fix: "a" b`, "c d", "e f"}) {
		t.Errorf("%q %v", got, err)
	}
	for _, bad := range []string{`echo "x`, `echo 'x`, "   "} {
		if _, err := SplitWords(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
