package metrics

import (
	"fmt"
	"strings"
	"testing"
)

func TestVec(t *testing.T) {
	c := newVec("test_total", "help", nil, "tenant", "agent")
	c.Inc("a", `x"y`)
	c.Add(2, "a", `x"y`)
	hist := newVec("test_seconds", "help", []float64{1, 10}, "agent")
	hist.Observe(0.5, "a")
	hist.Observe(5, "a")
	var b strings.Builder
	c.write(&b)
	hist.write(&b)
	for _, want := range []string{
		"# TYPE test_total counter\n",
		`test_total{tenant="a",agent="x\"y"} 3`,
		"# TYPE test_seconds histogram\n",
		`test_seconds_bucket{agent="a",le="1"} 1`,
		`test_seconds_bucket{agent="a",le="10"} 2`,
		`test_seconds_bucket{agent="a",le="+Inf"} 2`,
		`test_seconds_sum{agent="a"} 5.5`,
		`test_seconds_count{agent="a"} 2`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
}

func TestVecCapsSeries(t *testing.T) {
	c := newVec("capped_total", "help", nil, "tenant")
	for i := 0; i < maxSeries+50; i++ {
		c.Inc(fmt.Sprint("t", i))
	}
	if len(c.series) != maxSeries+1 {
		t.Fatalf("series = %d, want %d", len(c.series), maxSeries+1)
	}
	if s := c.series["other"]; s == nil || s.sum != 50 {
		t.Errorf("overflow not counted as other: %+v", s)
	}
	c.Inc("t1") // existing series still count
	if c.series["t1"].sum != 2 {
		t.Error("existing series stopped counting")
	}
}
