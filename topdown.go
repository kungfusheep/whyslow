package main

// topdown.go reads Instruments' CPU Bottlenecks breakdown: every cycle classified as useful, delivery
// (front end starved), processing (back end stalled) or discarded (wasted on mispredicted paths).

import (
	"fmt"
	"strconv"
	"strings"
)

type topDown struct {
	useful, delivery, processing, discarded float64 // fractions of all cycles
	tiers                                   map[byte]int
}

func loadTopDown(trace string, pid int) (topDown, error) {
	t, err := exportTable(trace, "MetricAggregationForProcess")
	if err != nil {
		return topDown{}, err
	}
	pidTag := fmt.Sprintf("(%d)", pid)
	sum := map[string]float64{}
	for _, row := range t.rows {
		proc := t.cell(row, "process")
		// the coarse (non-precise) windows tile the run without overlapping
		if proc == nil || !strings.HasSuffix(proc.attr("fmt"), pidTag) || t.cell(row, "is-precise").attr("fmt") != "No" {
			continue
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(valueOf(t.cell(row, "metric-value-double")), ",", ""), 64)
		if err != nil {
			continue
		}
		sum[valueOf(t.cell(row, "metric-name"))] += v
	}
	total := sum["useful"] + sum["delivery"] + sum["processing"] + sum["discarded"]
	if total == 0 {
		return topDown{}, fmt.Errorf("no bottleneck metrics for pid %d", pid)
	}
	td := topDown{sum["useful"] / total, sum["delivery"] / total, sum["processing"] / total, sum["discarded"] / total, nil}
	// the CPU Counters template samples stacks too, so this run's cores can be checked like the others
	if tp, err := exportTable(trace, "time-profile"); err == nil {
		td.tiers = coreTiers(tp, pid)
	}
	return td, nil
}
