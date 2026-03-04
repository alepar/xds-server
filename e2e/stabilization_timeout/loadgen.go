package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// RequestRecord holds a single request result.
type RequestRecord struct {
	Timestamp time.Time     // when request started
	Latency   time.Duration // round-trip time
	Status    int           // HTTP status code (0 = connection error)
}

// LoadResult holds all recorded requests from a run.
type LoadResult struct {
	Records   []RequestRecord
	StartTime time.Time
	EndTime   time.Time
	TargetQPS int
}

// LoadGen runs concurrent HTTP load at a target QPS.
type LoadGen struct {
	target      string
	qps         int
	concurrency int
	client      *http.Client
}

// NewLoadGen creates a load generator targeting the given URL.
func NewLoadGen(target string, qps int, concurrency int) *LoadGen {
	return &LoadGen{
		target:      target,
		qps:         qps,
		concurrency: concurrency,
		client: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				MaxIdleConnsPerHost: concurrency,
			},
		},
	}
}

// Run sends requests at target QPS until ctx is cancelled.
func (lg *LoadGen) Run(ctx context.Context) *LoadResult {
	result := &LoadResult{
		StartTime: time.Now(),
		TargetQPS: lg.qps,
	}

	// Per-worker record slices to avoid mutex contention
	type workerRecords struct {
		mu      sync.Mutex
		records []RequestRecord
	}
	workers := make([]workerRecords, lg.concurrency)

	// Work channel — buffered to allow ticker to enqueue without blocking
	work := make(chan struct{}, lg.concurrency*2)

	var wg sync.WaitGroup
	for i := 0; i < lg.concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for range work {
				ts := time.Now()
				status := 0
				resp, err := lg.client.Get(lg.target)
				latency := time.Since(ts)
				if err == nil {
					status = resp.StatusCode
					resp.Body.Close()
				}
				workers[idx].mu.Lock()
				workers[idx].records = append(workers[idx].records, RequestRecord{
					Timestamp: ts,
					Latency:   latency,
					Status:    status,
				})
				workers[idx].mu.Unlock()
			}
		}(i)
	}

	// Ticker paces requests at target QPS
	ticker := time.NewTicker(time.Second / time.Duration(lg.qps))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			result.EndTime = time.Now()
			// Merge all worker records
			for i := range workers {
				result.Records = append(result.Records, workers[i].records...)
			}
			// Sort by timestamp for consistent output
			sort.Slice(result.Records, func(a, b int) bool {
				return result.Records[a].Timestamp.Before(result.Records[b].Timestamp)
			})
			return result
		case <-ticker.C:
			select {
			case work <- struct{}{}:
			default:
				// Workers are saturated — skip this tick
			}
		}
	}
}

// TotalRequests returns the total number of requests made.
func (r *LoadResult) TotalRequests() int {
	return len(r.Records)
}

// SuccessRate returns the fraction of status==200 requests.
func (r *LoadResult) SuccessRate() float64 {
	if len(r.Records) == 0 {
		return 0
	}
	ok := 0
	for i := range r.Records {
		if r.Records[i].Status == 200 {
			ok++
		}
	}
	return float64(ok) / float64(len(r.Records))
}

// ErrorCount returns the number of non-200 responses.
func (r *LoadResult) ErrorCount() int {
	errs := 0
	for i := range r.Records {
		if r.Records[i].Status != 200 {
			errs++
		}
	}
	return errs
}

// StatusCounts returns a map of status code → count.
func (r *LoadResult) StatusCounts() map[int]int {
	counts := make(map[int]int)
	for i := range r.Records {
		counts[r.Records[i].Status]++
	}
	return counts
}

// GetPercentile returns the latency at the given percentile (0-100).
func (r *LoadResult) GetPercentile(pct float64) time.Duration {
	if len(r.Records) == 0 {
		return 0
	}
	latencies := make([]time.Duration, len(r.Records))
	for i := range r.Records {
		latencies[i] = r.Records[i].Latency
	}
	sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
	idx := int(float64(len(latencies)-1) * pct / 100.0)
	return latencies[idx]
}

// ActualQPS returns total requests divided by duration.
func (r *LoadResult) ActualQPS() float64 {
	d := r.EndTime.Sub(r.StartTime).Seconds()
	if d == 0 {
		return 0
	}
	return float64(len(r.Records)) / d
}

// Bucket holds aggregated stats for one time window.
type Bucket struct {
	Start   time.Duration // relative to test start
	End     time.Duration
	Total   int
	Success int // status 200
	Errors  int // non-200 or connection error
	P50     time.Duration
	P99     time.Duration
}

// Timeline produces time-bucketed stats with the given bucket size.
func (r *LoadResult) Timeline(bucketSize time.Duration) []Bucket {
	if len(r.Records) == 0 {
		return nil
	}
	duration := r.EndTime.Sub(r.StartTime)
	numBuckets := int(duration/bucketSize) + 1
	buckets := make([]Bucket, numBuckets)

	// Initialize bucket time ranges
	for i := range buckets {
		buckets[i].Start = time.Duration(i) * bucketSize
		buckets[i].End = time.Duration(i+1) * bucketSize
	}

	// Per-bucket latency collections for percentile computation
	bucketLatencies := make([][]time.Duration, numBuckets)

	for i := range r.Records {
		offset := r.Records[i].Timestamp.Sub(r.StartTime)
		idx := int(offset / bucketSize)
		if idx < 0 {
			idx = 0
		}
		if idx >= numBuckets {
			idx = numBuckets - 1
		}
		buckets[idx].Total++
		if r.Records[i].Status == 200 {
			buckets[idx].Success++
		} else {
			buckets[idx].Errors++
		}
		bucketLatencies[idx] = append(bucketLatencies[idx], r.Records[i].Latency)
	}

	// Compute percentiles per bucket
	for i := range buckets {
		lats := bucketLatencies[i]
		if len(lats) == 0 {
			continue
		}
		sort.Slice(lats, func(a, b int) bool { return lats[a] < lats[b] })
		buckets[i].P50 = lats[int(float64(len(lats)-1)*0.50)]
		buckets[i].P99 = lats[int(float64(len(lats)-1)*0.99)]
	}

	return buckets
}

// --- JSON serialization ---

type jsonOutput struct {
	StartTime string           `json:"start_time"`
	EndTime   string           `json:"end_time"`
	TargetQPS int              `json:"target_qps"`
	Summary   jsonSummary      `json:"summary"`
	Timeline  []jsonBucket     `json:"timeline"`
	Records   []jsonRecord     `json:"records"`
}

type jsonSummary struct {
	Total       int     `json:"total"`
	SuccessRate float64 `json:"success_rate"`
	Errors      int     `json:"errors"`
	ActualQPS   float64 `json:"actual_qps"`
	P50Us       int64   `json:"p50_us"`
	P99Us       int64   `json:"p99_us"`
}

type jsonBucket struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
	Total   int   `json:"total"`
	Success int   `json:"success"`
	Errors  int   `json:"errors"`
	P50Us   int64 `json:"p50_us"`
	P99Us   int64 `json:"p99_us"`
}

type jsonRecord struct {
	TMs      int64 `json:"t_ms"`
	LatencyUs int64 `json:"latency_us"`
	Status   int   `json:"status"`
}

// WriteJSON saves the full result to a JSON file.
func (r *LoadResult) WriteJSON(path string) error {
	timeline := r.Timeline(100 * time.Millisecond)

	out := jsonOutput{
		StartTime: r.StartTime.Format(time.RFC3339Nano),
		EndTime:   r.EndTime.Format(time.RFC3339Nano),
		TargetQPS: r.TargetQPS,
		Summary: jsonSummary{
			Total:       r.TotalRequests(),
			SuccessRate: r.SuccessRate(),
			Errors:      r.ErrorCount(),
			ActualQPS:   r.ActualQPS(),
			P50Us:       r.GetPercentile(50).Microseconds(),
			P99Us:       r.GetPercentile(99).Microseconds(),
		},
	}

	for _, b := range timeline {
		out.Timeline = append(out.Timeline, jsonBucket{
			StartMs: b.Start.Milliseconds(),
			EndMs:   b.End.Milliseconds(),
			Total:   b.Total,
			Success: b.Success,
			Errors:  b.Errors,
			P50Us:   b.P50.Microseconds(),
			P99Us:   b.P99.Microseconds(),
		})
	}

	for i := range r.Records {
		out.Records = append(out.Records, jsonRecord{
			TMs:       r.Records[i].Timestamp.Sub(r.StartTime).Milliseconds(),
			LatencyUs: r.Records[i].Latency.Microseconds(),
			Status:    r.Records[i].Status,
		})
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}
