// Package metrics exposes a handful of counters in Prometheus text format
// without the weight of the Prometheus client library.
package metrics

import (
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"time"
)

var (
	RunsStarted   atomic.Int64
	RunsSucceeded atomic.Int64
	RunsFailed    atomic.Int64
	LLMCalls      atomic.Int64
	ToolCalls     atomic.Int64
	ToolErrors    atomic.Int64
	InputTokens   atomic.Int64
	OutputTokens  atomic.Int64
	TriggerRuns   atomic.Int64
	HTTPRequests  atomic.Int64

	started = time.Now()
)

// Write renders all metrics.
func Write(w io.Writer) {
	counter := func(name, help string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	gauge := func(name, help string, v float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	counter("agent_runs_started_total", "Agent runs started.", RunsStarted.Load())
	counter("agent_runs_succeeded_total", "Agent runs finished successfully.", RunsSucceeded.Load())
	counter("agent_runs_failed_total", "Agent runs that failed.", RunsFailed.Load())
	counter("agent_llm_calls_total", "LLM API calls.", LLMCalls.Load())
	counter("agent_tool_calls_total", "Tool invocations.", ToolCalls.Load())
	counter("agent_tool_errors_total", "Tool invocations that returned an error.", ToolErrors.Load())
	counter("agent_input_tokens_total", "LLM input tokens.", InputTokens.Load())
	counter("agent_output_tokens_total", "LLM output tokens.", OutputTokens.Load())
	counter("agent_trigger_runs_total", "Runs started by triggers (cron, imap, webhook).", TriggerRuns.Load())
	counter("agent_http_requests_total", "HTTP API requests.", HTTPRequests.Load())

	writeVecs(w)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gauge("process_heap_bytes", "Go heap in use.", float64(ms.HeapInuse))
	gauge("process_sys_bytes", "Memory obtained from the OS.", float64(ms.Sys))
	gauge("process_goroutines", "Number of goroutines.", float64(runtime.NumGoroutine()))
	gauge("process_uptime_seconds", "Seconds since start.", time.Since(started).Seconds())
}
