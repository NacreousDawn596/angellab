// example/sentinel_tuning — interactive Sentinel baseline and scorer demo.
//
// This example runs the Sentinel scoring pipeline entirely in-process
// against synthetic connection data.  No daemon, no root privileges,
// no filesystem access.  Useful for:
//
//   - Understanding how the baseline learning works
//   - Tuning WarnThreshold / CritThreshold for your environment
//   - Verifying that a new scoring rule behaves as expected before deploying
//   - Reproducing a specific alert to understand why it fired
//
// It proceeds in three phases:
//
//  1. TRAINING  — feed 50 "normal" connections to build the baseline
//  2. SCORING   — evaluate 20 new connections (mix of normal + anomalous)
//  3. REPORT    — print a summary of every alert produced
//
// Run:
//
//	go run ./example/sentinel_tuning
//
// Modify the trainingConns and testConns slices below to match your
// environment's traffic profile.
package main

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/nacreousdawn596/angellab/internal/angels/sentinel"
)

func main() {
	fmt.Println("AngelLab Sentinel tuning example")
	fmt.Println(strings.Repeat("─", 60))

	// -----------------------------------------------------------------------
	// Phase 1: TRAINING
	//
	// These connections represent "normal" outbound traffic.
	// The baseline learns: which IPs are known, which ports are common,
	// and what the typical concurrent connection count looks like.
	// -----------------------------------------------------------------------

	fmt.Println("\n[1/3] Training baseline...")

	trainingConns := []sentinel.OutboundConn{
		// DNS
		{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 53},
		{RemoteIP: net.ParseIP("1.1.1.1"), RemotePort: 53},
		// HTTPS to common CDNs
		{RemoteIP: net.ParseIP("151.101.1.140"), RemotePort: 443},  // fastly
		{RemoteIP: net.ParseIP("104.21.0.1"), RemotePort: 443},     // cloudflare
		{RemoteIP: net.ParseIP("13.227.220.1"), RemotePort: 443},   // aws cloudfront
		// HTTP (uncommon but seen occasionally)
		{RemoteIP: net.ParseIP("151.101.1.140"), RemotePort: 80},
		// NTP
		{RemoteIP: net.ParseIP("162.159.200.1"), RemotePort: 123},
		// Internal services (RFC1918 — always scored down)
		{RemoteIP: net.ParseIP("10.0.0.1"), RemotePort: 5432},   // postgres
		{RemoteIP: net.ParseIP("10.0.0.2"), RemotePort: 6379},   // redis
		{RemoteIP: net.ParseIP("192.168.1.10"), RemotePort: 8080},
	}

	baseline := sentinel.NewBaseline()

	// Feed training connections 10 times (simulates repeated observations
	// over the training window, varying the concurrent count slightly).
	for i := 0; i < 10; i++ {
		n := len(trainingConns) - (i % 3) // vary concurrency
		if n < 1 {
			n = 1
		}
		baseline.Observe(trainingConns[:n])
	}
	baseline.Freeze()

	fmt.Printf("  Baseline frozen: %s\n", baseline.Summary())

	// -----------------------------------------------------------------------
	// Phase 2: SCORING
	//
	// These connections are evaluated against the frozen baseline.
	// Each one produces an AnomalyScore.
	// -----------------------------------------------------------------------

	fmt.Println("\n[2/3] Scoring test connections...")

	type testCase struct {
		label string
		conn  sentinel.OutboundConn
	}

	testConns := []testCase{
		// Should NOT alert (known IP + port)
		{"known DNS", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 53,
		}},
		{"known HTTPS", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("151.101.1.140"), RemotePort: 443,
		}},
		{"internal redis", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("10.0.0.2"), RemotePort: 6379,
		}},

		// Borderline (WARN expected)
		{"new IP, known port 443", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("185.199.108.153"), RemotePort: 443, // github pages
		}},
		{"known IP, new port", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 8853, // DNS-over-HTTPS alt port
		}},

		// Should CRITICAL (new IP + high/unusual port)
		{"suspicious: new IP + high port", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("45.33.32.156"), RemotePort: 4444, // common C2 port
		}},
		{"suspicious: new IP + ephemeral port", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("198.51.100.5"), RemotePort: 62345,
		}},
		{"suspicious: new IP + very high port", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("203.0.113.42"), RemotePort: 65535,
		}},

		// RFC1918 — should score low regardless
		{"internal, new IP (RFC1918)", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("192.168.99.100"), RemotePort: 9090, // new grafana
		}},

		// Loopback — must never alert
		{"loopback", sentinel.OutboundConn{
			RemoteIP: net.ParseIP("127.0.0.1"), RemotePort: 8080,
		}},
	}

	rateTrack := sentinel.NewRateTracker(0) // zero window = rate tracking disabled for demo
	scorer := sentinel.NewScorer(baseline, rateTrack)
	dedup := sentinel.NewDeduplicator()

	type result struct {
		label string
		score sentinel.AnomalyScore
		level sentinel.ScoreLevel
	}
	var results []result

	now := time.Now()
	for _, tc := range testConns {
		isNew := dedup.IsNew(tc.conn, now)
		score := scorer.Score(tc.conn, isNew, len(testConns))
		results = append(results, result{
			label: tc.label,
			score: score,
			level: score.Level(),
		})
	}

	// -----------------------------------------------------------------------
	// Phase 3: REPORT
	// -----------------------------------------------------------------------

	fmt.Println("\n[3/3] Results")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Printf("  %-38s  %-6s  %s\n", "Connection", "Level", "Details")
	fmt.Println(strings.Repeat("─", 60))

	for _, r := range results {
		level := levelLabel(r.level)
		fmt.Printf("  %-38s  %-6s  %s\n", r.label, level, r.score.Summary())
	}

	fmt.Println(strings.Repeat("─", 60))

	// Count by level.
	ok, warn, crit := 0, 0, 0
	for _, r := range results {
		switch r.level {
		case sentinel.ScoreOK:
			ok++
		case sentinel.ScoreWarn:
			warn++
		case sentinel.ScoreCrit:
			crit++
		}
	}
	fmt.Printf("\n  Total: %d  |  OK: %d  |  WARN: %d  |  CRIT: %d\n\n",
		len(results), ok, warn, crit)

	// -----------------------------------------------------------------------
	// Tuning hints
	// -----------------------------------------------------------------------

	fmt.Println("Tuning hints:")
	fmt.Printf("  WarnThreshold (default %d): lower = more sensitive, higher = quieter\n",
		sentinel.WarnThreshold)
	fmt.Printf("  CritThreshold (default %d): connections scoring ≥ this trigger CRIT events\n",
		sentinel.CritThreshold)
	fmt.Println()
	fmt.Println("  To raise the baseline for a specific IP/port, add it to your training")
	fmt.Println("  data above and re-run this example.  The score will drop accordingly.")
	fmt.Println()
	fmt.Println("  In production, extend baseline_duration in angellab.toml:")
	fmt.Println("    [[angel]]")
	fmt.Println("    type = \"sentinel\"")
	fmt.Println("    id   = \"A-02\"")
	fmt.Println("    baseline_duration = \"300s\"   # 5 minutes of observation")
}

func levelLabel(l sentinel.ScoreLevel) string {
	switch l {
	case sentinel.ScoreOK:
		return "OK"
	case sentinel.ScoreWarn:
		return "WARN"
	case sentinel.ScoreCrit:
		return "CRIT"
	default:
		return "????"
	}
}
