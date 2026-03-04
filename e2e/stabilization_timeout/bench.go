package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// BenchConfig holds CLI flags for the bench subcommand.
type BenchConfig struct {
	EnvoyWithFix    string
	EnvoyWithoutFix string
	AdminPort       int
	XDSPort         int
	BasePort        int
	QPS             int
	WarmupDuration  time.Duration
	TestDuration    time.Duration
	HealthyCount    int
	TotalCount      int
}

func parseBenchFlags(args []string) *BenchConfig {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	cfg := &BenchConfig{}

	fs.StringVar(&cfg.EnvoyWithFix, "envoy-with-fix", "./envoy-static-with-fix", "Path to envoy binary with fix")
	fs.StringVar(&cfg.EnvoyWithoutFix, "envoy-without-fix", "./envoy-static-without-fix", "Path to envoy binary without fix")
	fs.IntVar(&cfg.AdminPort, "admin-port", 9901, "Envoy admin port")
	fs.IntVar(&cfg.XDSPort, "xds-port", 5678, "xDS gRPC port")
	fs.IntVar(&cfg.BasePort, "base-port", 8081, "Base port for backend pool")
	fs.IntVar(&cfg.QPS, "qps", 500, "Fortio target QPS")
	fs.DurationVar(&cfg.WarmupDuration, "warmup", 10*time.Second, "Warmup duration before endpoint swap")
	fs.DurationVar(&cfg.TestDuration, "duration", 30*time.Second, "Total fortio test duration (warmup + transition)")
	fs.IntVar(&cfg.HealthyCount, "healthy-count", 3, "Number of healthy backends after swap")
	fs.IntVar(&cfg.TotalCount, "total-count", 5, "Total backends after swap (healthy + black holes)")

	fs.Parse(args)
	return cfg
}

// runBench orchestrates two sequential benchmark runs and compares results.
func runBench(cfg *BenchConfig) {
	// Validate binaries
	for _, bin := range []string{cfg.EnvoyWithFix, cfg.EnvoyWithoutFix} {
		if _, err := os.Stat(bin); err != nil {
			log.Fatalf("binary not found: %s", bin)
		}
	}

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║           EDS Stabilization Timeout Benchmark               ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Run 1: Legacy (without fix, ignore_health_on_host_removal=true, timeout=0)
	fmt.Println("━━━ Run 1: Legacy (ignore_health_on_host_removal=true) ━━━")
	legacyResult := runBenchScenario(cfg, benchScenarioConfig{
		label:                 "legacy",
		envoyBinary:           cfg.EnvoyWithoutFix,
		ignoreHealthOnRemoval: true,
		timeoutMs:             0,
	})

	// Run 2: Fixed (with fix, ignore_health_on_host_removal=false, timeout=60s)
	fmt.Println()
	fmt.Println("━━━ Run 2: Fixed (stabilization_timeout=60s) ━━━")
	fixedResult := runBenchScenario(cfg, benchScenarioConfig{
		label:                 "fixed",
		envoyBinary:           cfg.EnvoyWithFix,
		ignoreHealthOnRemoval: false,
		timeoutMs:             60000,
	})

	// Compare and print summary
	fmt.Println()
	compareBenchResults(legacyResult, fixedResult)
}

type benchScenarioConfig struct {
	label                 string
	envoyBinary           string
	ignoreHealthOnRemoval bool
	timeoutMs             uint
}

type benchResult struct {
	Label       string
	Fortio      *FortioResult
	SwapTime    time.Time
	EnvoyBinary string
}

func runBenchScenario(cfg *BenchConfig, sc benchScenarioConfig) *benchResult {
	xds := NewXDSController()
	admin := NewAdminClient(cfg.AdminPort)
	backends := NewBackendPool()
	defer backends.StopAll()

	configPath := "envoy-bench.yaml"

	// Start xDS
	if err := xds.Start(cfg.XDSPort); err != nil {
		log.Fatalf("[%s] xds start: %v", sc.label, err)
	}
	defer xds.Stop()

	// Configure cluster
	var opts []func(*XDSController)
	if sc.ignoreHealthOnRemoval {
		opts = append(opts, WithIgnoreHealthOnRemoval(true))
	}
	xds.SetClusterConfig(sc.timeoutMs, true, opts...)

	// Start initial backends (all healthy)
	initialPorts := make([]int, cfg.TotalCount)
	for i := range initialPorts {
		initialPorts[i] = cfg.BasePort + i
	}
	for _, port := range initialPorts {
		if err := backends.Start(port); err != nil {
			log.Fatalf("[%s] backend start %d: %v", sc.label, port, err)
		}
	}

	// Push initial endpoints via EDS
	if err := xds.AddEndpoints(initialPorts...); err != nil {
		log.Fatalf("[%s] add endpoints: %v", sc.label, err)
	}

	// Start Envoy
	envoy, err := startEnvoy(sc.envoyBinary, configPath, cfg.AdminPort, 99)
	if err != nil {
		log.Fatalf("[%s] envoy start: %v", sc.label, err)
	}
	defer envoy.Stop()

	if err := admin.WaitForReady(15 * time.Second); err != nil {
		log.Fatalf("[%s] envoy not ready: %v", sc.label, err)
	}

	// Wait for all hosts to become healthy
	for _, port := range initialPorts {
		if err := admin.WaitForHostHealthy(clusterName, addr(port), 15*time.Second); err != nil {
			log.Fatalf("[%s] host %d not healthy: %v", sc.label, port, err)
		}
	}
	log.Printf("[%s] All %d hosts healthy", sc.label, len(initialPorts))

	// Start Fortio in background (total duration covers warmup + transition)
	jsonPath := fmt.Sprintf("bench-result-%s.json", sc.label)
	target := "http://127.0.0.1:10000/"

	fortioDone := make(chan *FortioResult, 1)
	fortioErr := make(chan error, 1)
	go func() {
		result, err := FortioRun(target, cfg.QPS, cfg.TestDuration.String(), jsonPath)
		if err != nil {
			fortioErr <- err
		} else {
			fortioDone <- result
		}
	}()

	// Warmup phase
	log.Printf("[%s] Warming up for %v...", sc.label, cfg.WarmupDuration)
	time.Sleep(cfg.WarmupDuration)

	// Endpoint swap: replace with new set (healthy-count respond 200, rest are black holes)
	log.Printf("[%s] Swapping endpoints: %d healthy + %d black holes",
		sc.label, cfg.HealthyCount, cfg.TotalCount-cfg.HealthyCount)

	// Stop all old backends
	for _, port := range initialPorts {
		backends.Stop(port)
	}

	// Start new backends — first healthy-count get real HTTP servers,
	// remaining ports get no listener (black holes / connection refused)
	newPorts := make([]int, cfg.TotalCount)
	for i := range newPorts {
		newPorts[i] = cfg.BasePort + 100 + i // offset to avoid port conflicts
	}
	for i := 0; i < cfg.HealthyCount; i++ {
		if err := backends.Start(newPorts[i]); err != nil {
			log.Printf("[%s] warning: backend start %d: %v", sc.label, newPorts[i], err)
		}
	}
	// Black hole ports: no listener started

	swapTime := time.Now()
	if err := xds.ReplaceEndpoints(newPorts...); err != nil {
		log.Fatalf("[%s] replace endpoints: %v", sc.label, err)
	}
	log.Printf("[%s] Endpoints swapped at %s", sc.label, swapTime.Format("15:04:05.000"))

	// Wait for Fortio to finish
	log.Printf("[%s] Waiting for fortio to complete...", sc.label)
	select {
	case result := <-fortioDone:
		log.Printf("[%s] Fortio done: %d requests, %.1f%% success rate",
			sc.label, result.TotalRequests(), result.SuccessRate()*100)
		return &benchResult{
			Label:       sc.label,
			Fortio:      result,
			SwapTime:    swapTime,
			EnvoyBinary: sc.envoyBinary,
		}
	case err := <-fortioErr:
		log.Fatalf("[%s] fortio failed: %v", sc.label, err)
		return nil // unreachable
	}
}

func compareBenchResults(legacy, fixed *benchResult) {
	printBenchSummary(legacy, fixed)
}

func printBenchSummary(legacy, fixed *benchResult) {
	w := 30 // column width

	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Println("║                   Benchmark Comparison                      ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════╣")

	// Header
	fmt.Printf("║ %-28s │ %12s │ %12s ║\n", "Metric", "Legacy", "Fixed")
	fmt.Printf("╠%s╬%s╬%s╣\n", strings.Repeat("═", w), strings.Repeat("═", 14), strings.Repeat("═", 14))

	// QPS
	fmt.Printf("║ %-28s │ %12.1f │ %12.1f ║\n", "Actual QPS",
		legacy.Fortio.ActualQPS, fixed.Fortio.ActualQPS)

	// Total requests
	fmt.Printf("║ %-28s │ %12d │ %12d ║\n", "Total Requests",
		legacy.Fortio.TotalRequests(), fixed.Fortio.TotalRequests())

	// Success rate
	fmt.Printf("║ %-28s │ %11.2f%% │ %11.2f%% ║\n", "Success Rate",
		legacy.Fortio.SuccessRate()*100, fixed.Fortio.SuccessRate()*100)

	// Error count
	fmt.Printf("║ %-28s │ %12d │ %12d ║\n", "Errors",
		legacy.Fortio.ErrorCount(), fixed.Fortio.ErrorCount())

	// Latency percentiles
	for _, pct := range []float64{50, 75, 90, 99} {
		lVal := legacy.Fortio.GetPercentile(pct)
		fVal := fixed.Fortio.GetPercentile(pct)
		label := fmt.Sprintf("p%.0f Latency", pct)
		fmt.Printf("║ %-28s │ %12s │ %12s ║\n", label,
			fmtLatency(lVal), fmtLatency(fVal))
	}

	// Max latency
	if legacy.Fortio.DurationHistogram != nil && fixed.Fortio.DurationHistogram != nil {
		fmt.Printf("║ %-28s │ %12s │ %12s ║\n", "Max Latency",
			fmtLatency(legacy.Fortio.DurationHistogram.Max),
			fmtLatency(fixed.Fortio.DurationHistogram.Max))
	}

	fmt.Printf("╚%s╩%s╩%s╝\n", strings.Repeat("═", w), strings.Repeat("═", 14), strings.Repeat("═", 14))

	// Return codes breakdown
	fmt.Println()
	fmt.Println("Return Code Breakdown:")
	allCodes := make(map[string]bool)
	for code := range legacy.Fortio.RetCodes {
		allCodes[code] = true
	}
	for code := range fixed.Fortio.RetCodes {
		allCodes[code] = true
	}
	for code := range allCodes {
		lCount := legacy.Fortio.RetCodes[code]
		fCount := fixed.Fortio.RetCodes[code]
		fmt.Printf("  HTTP %s: legacy=%d  fixed=%d\n", code, lCount, fCount)
	}
}

// fmtLatency formats a latency value (in seconds) for display.
func fmtLatency(seconds float64) string {
	if seconds < 0 {
		return "N/A"
	}
	if seconds < 0.001 {
		return fmt.Sprintf("%.0fus", seconds*1_000_000)
	}
	if seconds < 1.0 {
		return fmt.Sprintf("%.2fms", seconds*1000)
	}
	return fmt.Sprintf("%.3fs", seconds)
}
