package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// maxSeries bounds the label combinations of one metric. Tenants, agents
// and tools are named by customers; past the cap new combinations are
// counted under the label value "other" instead of growing without limit.
const maxSeries = 2000

// Vec is a counter or histogram with labels.
type Vec struct {
	name, help string
	labels     []string
	buckets    []float64 // nil: counter

	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	values []string
	sum    float64
	count  uint64
	bucket []uint64
}

var vecs []*Vec

func newVec(name, help string, buckets []float64, labels ...string) *Vec {
	v := &Vec{name: name, help: help, labels: labels, buckets: buckets, series: map[string]*series{}}
	vecs = append(vecs, v)
	return v
}

// Add adds n to the counter with the given label values.
func (v *Vec) Add(n float64, values ...string) { v.observe(n, values) }

// Inc adds 1.
func (v *Vec) Inc(values ...string) { v.observe(1, values) }

// Observe records a histogram sample.
func (v *Vec) Observe(x float64, values ...string) { v.observe(x, values) }

func (v *Vec) observe(x float64, values []string) {
	if len(values) != len(v.labels) {
		panic(fmt.Sprintf("metrics: %s wants %d labels, got %d", v.name, len(v.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	v.mu.Lock()
	defer v.mu.Unlock()
	s := v.series[key]
	if s == nil {
		if len(v.series) >= maxSeries {
			values = make([]string, len(v.labels))
			for i := range values {
				values[i] = "other"
			}
			key = strings.Join(values, "\x00")
			s = v.series[key]
		}
		if s == nil {
			s = &series{values: append([]string(nil), values...), bucket: make([]uint64, len(v.buckets))}
			v.series[key] = s
		}
	}
	s.count++
	s.sum += x
	for i, b := range v.buckets {
		if x <= b {
			s.bucket[i]++
		}
	}
}

func (v *Vec) write(w io.Writer) {
	v.mu.Lock()
	defer v.mu.Unlock()
	typ := "counter"
	if v.buckets != nil {
		typ = "histogram"
	}
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", v.name, v.help, v.name, typ)
	keys := make([]string, 0, len(v.series))
	for k := range v.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := v.series[k]
		lbl := v.labelString(s.values, "")
		if v.buckets == nil {
			fmt.Fprintf(w, "%s%s %s\n", v.name, lbl, num(s.sum))
			continue
		}
		for i, b := range v.buckets {
			fmt.Fprintf(w, "%s_bucket%s %d\n", v.name, v.labelString(s.values, num(b)), s.bucket[i])
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", v.name, v.labelString(s.values, "+Inf"), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n%s_count%s %d\n", v.name, lbl, num(s.sum), v.name, lbl, s.count)
	}
}

func (v *Vec) labelString(values []string, le string) string {
	parts := make([]string, 0, len(values)+1)
	for i, l := range v.labels {
		parts = append(parts, l+"="+strconv.Quote(values[i]))
	}
	if le != "" {
		parts = append(parts, `le="`+le+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Labeled metrics. tenant/agent let alerts and dashboards say which agent
// misbehaves; status is ok, max_steps or error.
var (
	Runs = newVec("agent_runs_total", "Finished agent runs by outcome.", nil,
		"tenant", "agent", "source", "status")
	RunDuration = newVec("agent_run_duration_seconds", "Duration of agent runs.",
		[]float64{1, 2.5, 5, 10, 20, 30, 60, 120, 300, 600}, "tenant", "agent")
	RunSteps = newVec("agent_run_steps_total", "Model steps taken by agent runs.", nil, "tenant", "agent")
	Tools    = newVec("agent_tool_results_total", "Tool results by tool and outcome (ok, error).", nil,
		"tenant", "agent", "tool", "result")
	Tokens = newVec("agent_tokens_total", "LLM tokens by agent and direction (input, output).", nil,
		"tenant", "agent", "direction")
	Rejected = newVec("agent_runs_rejected_total", "Runs refused before they started (rate_limit, quota, disabled).", nil,
		"tenant", "reason")
	LLMErrors = newVec("agent_llm_errors_total", "Failed LLM calls by provider and kind (auth, throttled, server, client, timeout, network).", nil,
		"provider", "kind")
)

func writeVecs(w io.Writer) {
	for _, v := range vecs {
		v.write(w)
	}
}
