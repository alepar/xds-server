package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
)

// FortioResult holds parsed results from a fortio JSON output file.
type FortioResult struct {
	Labels          string           `json:"Labels"`
	StartTime       string           `json:"StartTime"`
	RequestedQPS    string           `json:"RequestedQPS"`
	RequestedDuration string         `json:"RequestedDuration"`
	ActualQPS       float64          `json:"ActualQPS"`
	ActualDuration  float64          `json:"ActualDuration"` // nanoseconds
	NumThreads      int              `json:"NumThreads"`
	DurationHistogram *DurationHisto `json:"DurationHistogram"`
	RetCodes        map[string]int   `json:"RetCodes"`
	URL             string           `json:"URL"`
}

// DurationHisto holds fortio's latency distribution data.
type DurationHisto struct {
	Count       int              `json:"Count"`
	Min         float64          `json:"Min"`
	Max         float64          `json:"Max"`
	Sum         float64          `json:"Sum"`
	Avg         float64          `json:"Avg"`
	StdDev      float64          `json:"StdDev"`
	Percentiles []PercentileData `json:"Percentiles"`
}

// PercentileData holds a single percentile entry.
type PercentileData struct {
	Percentile float64 `json:"Percentile"`
	Value      float64 `json:"Value"`
}

// FortioRun executes fortio load and returns the parsed results.
// target is the URL to hit (e.g. "http://127.0.0.1:10000/").
// qps is the target queries per second (0 = max).
// duration is the test duration string (e.g. "30s").
// jsonPath is where fortio writes its JSON output.
func FortioRun(target string, qps int, duration string, jsonPath string) (*FortioResult, error) {
	args := []string{
		"load",
		"-qps", fmt.Sprintf("%d", qps),
		"-t", duration,
		"-json", jsonPath,
		"-c", "16",
		"-nocatchup",
		target,
	}

	cmd := exec.Command("fortio", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("fortio load: %w", err)
	}

	return ParseFortioJSON(jsonPath)
}

// ParseFortioJSON reads and parses a fortio JSON output file.
func ParseFortioJSON(path string) (*FortioResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fortio json: %w", err)
	}

	var result FortioResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse fortio json: %w", err)
	}

	return &result, nil
}

// SuccessRate returns the fraction of requests that got HTTP 200.
func (r *FortioResult) SuccessRate() float64 {
	total := r.TotalRequests()
	if total == 0 {
		return 0
	}
	ok := r.RetCodes["200"]
	return float64(ok) / float64(total)
}

// ErrorCount returns the number of non-200 responses.
func (r *FortioResult) ErrorCount() int {
	total := r.TotalRequests()
	ok := r.RetCodes["200"]
	return total - ok
}

// TotalRequests returns the total number of requests made.
func (r *FortioResult) TotalRequests() int {
	total := 0
	for _, count := range r.RetCodes {
		total += count
	}
	return total
}

// GetPercentile returns the latency (in seconds) at the given percentile (0-100).
// Returns -1 if the percentile is not found.
func (r *FortioResult) GetPercentile(pct float64) float64 {
	if r.DurationHistogram == nil {
		return -1
	}
	// Fortio stores percentiles as 50, 75, 90, 99, 99.9 (not 0.5, 0.75, etc.)
	for _, p := range r.DurationHistogram.Percentiles {
		if p.Percentile >= pct-0.1 && p.Percentile <= pct+0.1 {
			return p.Value
		}
	}
	// If exact match not found, find closest >= target
	sorted := make([]PercentileData, len(r.DurationHistogram.Percentiles))
	copy(sorted, r.DurationHistogram.Percentiles)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Percentile < sorted[j].Percentile
	})
	for _, p := range sorted {
		if p.Percentile >= pct {
			return p.Value
		}
	}
	return -1
}
