package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func testServer() *Server {
	return &Server{Name: "test", Version: "1", Tools: func() []ServerTool {
		return []ServerTool{
			{Name: "echo", Description: "echoes", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
				Handler: func(_ context.Context, args json.RawMessage) (string, error) {
					var a struct{ Text string }
					json.Unmarshal(args, &a)
					return "echo: " + a.Text, nil
				}},
			{Name: "fail", Description: "fails", InputSchema: json.RawMessage(`{"type":"object"}`),
				Handler: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("nope") }},
		}
	}}
}

// TestMain lets the test binary act as a stdio MCP server when re-executed.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_STDIO_SERVER") == "1" {
		srv := testServer()
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			// Send a notification first to make sure the client ignores it.
			fmt.Println(`{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp", strings.NewReader(sc.Text())))
			if rec.Code == 200 {
				os.Stdout.Write(bytes.TrimSpace(rec.Body.Bytes()))
				os.Stdout.Write([]byte("\n"))
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func exercise(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if c.ServerName != "test" {
		t.Errorf("server name %q", c.ServerName)
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 2 {
		t.Fatalf("list: %v %v", tools, err)
	}
	res, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil || res.Text() != "echo: hi" || res.IsError {
		t.Fatalf("call: %+v %v", res, err)
	}
	res, err = c.CallTool(ctx, "fail", nil)
	if err != nil || !res.IsError || res.Text() != "nope" {
		t.Fatalf("fail call: %+v %v", res, err)
	}
	if _, err := c.CallTool(ctx, "missing", nil); err == nil {
		t.Fatal("expected rpc error for unknown tool")
	}
}

func TestHTTPClientServer(t *testing.T) {
	var sawAuth, sawProto bool
	srv := testServer()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = sawAuth || r.Header.Get("Authorization") == "Bearer t"
		sawProto = sawProto || r.Header.Get("MCP-Protocol-Version") == ProtocolVersion
		srv.ServeHTTP(w, r)
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, map[string]string{"Authorization": "Bearer t"}, 0, nil)
	exercise(t, c)
	c.Close()
	if !sawAuth || !sawProto {
		t.Errorf("auth=%v proto=%v", sawAuth, sawProto)
	}
}

func TestHTTPClientSSEAndSession(t *testing.T) {
	srv := testServer()
	var gotSession string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		gotSession = r.Header.Get("Mcp-Session-Id")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		w.Header().Set("Mcp-Session-Id", "sess-1")
		if rec.Code != 200 {
			w.WriteHeader(rec.Code)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// A progress notification, then the response split over two data lines.
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		var pretty bytes.Buffer
		json.Indent(&pretty, bytes.TrimSpace(rec.Body.Bytes()), "", " ")
		io.WriteString(w, "event: message\n")
		for _, line := range strings.Split(pretty.String(), "\n") {
			fmt.Fprintf(w, "data: %s\n", line)
		}
		io.WriteString(w, "\n")
	}))
	defer hs.Close()
	c := NewHTTPClient(hs.URL, nil, 0, nil)
	exercise(t, c)
	if gotSession != "sess-1" {
		t.Errorf("session header not echoed: %q", gotSession)
	}
}

func TestStdioClient(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewStdioClient([]string{exe, "-test.run=^$"}, map[string]string{"MCP_STDIO_SERVER": "1"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, c)
	c.Close()
	// After close, calls fail fast.
	if _, err := c.CallTool(context.Background(), "echo", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("want ErrClosed, got %v", err)
	}
}

func TestServerRejectsGet(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer().ServeHTTP(rec, httptest.NewRequest("GET", "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code %d", rec.Code)
	}
}
