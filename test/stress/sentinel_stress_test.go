// Package stress contains load and latency tests for the Sentinel pipeline.
//
// These tests do NOT use the network — they exercise the parse and scoring
// paths directly against synthetic /proc/net/tcp data to measure throughput
// and latency under realistic conditions.
//
// Target: parse + score < 5ms per poll cycle at 1000 connections.
//
// Run:
//
//	go test ./test/stress/... -v -run TestSentinelLatency
//	go test ./test/stress/... -v -bench=. -benchmem
//
// The tests import internal packages directly.  This is intentional — we
// want to measure the hot path (parser → dedup → scorer) in isolation, not
// the poll goroutine overhead.
package stress

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/nacreousdawn596/angellab/internal/angels/sentinel"
	"github.com/nacreousdawn596/angellab/pkg/linux"
)

// ---------------------------------------------------------------------------
// Synthetic /proc/net/tcp generator
// ---------------------------------------------------------------------------

// syntheticProcNetTCP generates a realistic /proc/net/tcp file with n entries.
//
// Format (columns):
//
//	sl  local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode
//
// We generate:
//   - 60% established outbound connections to diverse remote IPs
//   - 20% connections to a small set of "known" IPs (would be in baseline)
//   - 20% listening sockets (state 0A = LISTEN, skipped by scorer)
func syntheticProcNetTCP(n int, rng *rand.Rand) []byte {
	var buf bytes.Buffer
	buf.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")

	knownIPs := []string{
		"08080808", // 8.8.8.8
		"01010101", // 1.1.1.1
		"8EFAC7AD", // 173.199.250.142 (example CDN)
	}

	for i := 0; i < n; i++ {
		var remAddrHex string
		var remPortHex string
		var state string

		r := rng.Intn(100)
		switch {
		case r < 20:
			// Listening socket — state 0A, remote = 00000000:0000, skipped by scorer.
			state      = "0A"
			remAddrHex = "00000000"
			remPortHex = "0000"
		case r < 40:
			// Known IP — in baseline, should not score.
			remAddrHex = knownIPs[rng.Intn(len(knownIPs))]
			remPortHex = fmt.Sprintf("%04X", uint16(443))
			state      = "01"
		default:
			// Novel outbound — mix of ports and IPs.
			ip := randomIPv4Hex(rng)
			port := randomPort(rng)
			remAddrHex = ip
			remPortHex = fmt.Sprintf("%04X", port)
			state      = "01"
		}

		localPort := uint16(30000 + rng.Intn(30000))
		localAddr := fmt.Sprintf("0100007F:%04X", localPort) // 127.0.0.1:random

		inode := rng.Uint64() % 100000
		fmt.Fprintf(&buf,
			"%4d: %s %s:%s %s 00000000:00000000 00:00000000 00000000     0        0 %d 1 0000000000000000 100 0 0 10 0\n",
			i, localAddr, remAddrHex, remPortHex, state, inode)
	}
	return buf.Bytes()
}

// randomIPv4Hex generates a random non-RFC1918, non-loopback IPv4 in LE hex.
func randomIPv4Hex(rng *rand.Rand) string {
	for {
		b := make([]byte, 4)
		b[0] = byte(rng.Intn(256))
		b[1] = byte(rng.Intn(256))
		b[2] = byte(rng.Intn(256))
		b[3] = byte(rng.Intn(256))
		ip := net.IP(b)
		// Skip private and loopback ranges.
		if ip.IsLoopback() || ip.IsPrivate() || ip[0] == 0 {
			continue
		}
		// Encode as little-endian hex (the /proc/net/tcp format).
		var le [4]byte
		binary.LittleEndian.PutUint32(le[:], binary.BigEndian.Uint32(b))
		return hex.EncodeToString(le[:])
	}
}

// randomPort returns a mix of well-known (<=1024), registered (1025-49151),
// and ephemeral (49152-65535) ports to exercise all scoring rules.
func randomPort(rng *rand.Rand) uint16 {
	r := rng.Intn(100)
	switch {
	case r < 20:
		return uint16(rng.Intn(1024) + 1) // well-known
	case r < 60:
		return uint16(1025 + rng.Intn(48126)) // registered
	default:
		return uint16(49152 + rng.Intn(16383)) // ephemeral / high
	}
}

// ---------------------------------------------------------------------------
// Baseline builder
// ---------------------------------------------------------------------------

// buildTestBaseline creates a frozen baseline with the "known" IPs and
// ports so the scorer has something to compare against.
func buildTestBaseline() *sentinel.Baseline {
	b := sentinel.NewBaseline()

	knownConns := []sentinel.OutboundConn{
		{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443},
		{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 53},
		{RemoteIP: net.ParseIP("1.1.1.1"), RemotePort: 443},
		{RemoteIP: net.ParseIP("1.1.1.1"), RemotePort: 53},
		{RemoteIP: net.ParseIP("173.199.250.142"), RemotePort: 443},
		{RemoteIP: net.ParseIP("173.199.250.142"), RemotePort: 80},
	}

	// Feed 10× to let maxConcurrent stabilise.
	for i := 0; i < 10; i++ {
		// Vary concurrent count slightly to get a realistic max.
		n := 8 + rand.Intn(6)
		b.Observe(knownConns[:n%len(knownConns)+1])
	}
	b.Freeze()
	return b
}

// ---------------------------------------------------------------------------
// Benchmark: full parse + score pipeline
// ---------------------------------------------------------------------------

// BenchmarkSentinelPipeline measures end-to-end latency of the hot path:
//
//  1. Parse synthetic /proc/net/tcp bytes into []NetConn
//  2. Filter to outbound ESTABLISHED connections
//  3. Deduplicate
//  4. Score each connection against the baseline
//
// Run: go test -bench=BenchmarkSentinelPipeline -benchmem -count=5
func BenchmarkSentinelPipeline(b *testing.B) {
	for _, n := range []int{100, 500, 1000, 2000} {
		n := n
		b.Run(fmt.Sprintf("conns_%d", n), func(b *testing.B) {
			rng       := rand.New(rand.NewSource(42))
			raw       := syntheticProcNetTCP(n, rng)
			baseline  := buildTestBaseline()
			dedup     := sentinel.NewDeduplicator()
			rateTrack := sentinel.NewRateTracker(5 * time.Second)
			scorer    := sentinel.NewScorer(baseline, rateTrack)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				conns := linux.ParseTCPFile(raw)

				outbound := make([]sentinel.OutboundConn, 0, len(conns))
				for _, c := range conns {
					if c.IsOutbound() && c.IsEstablished() {
						outbound = append(outbound, sentinel.OutboundConn{
							RemoteIP:   c.RemoteIP,
							RemotePort: c.RemotePort,
						})
					}
				}

				now := time.Now()
				for _, oc := range outbound {
					if isNew := dedup.IsNew(oc, now); isNew {
						scorer.Score(oc, true, len(outbound))
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Latency test: verifies the <5ms target
// ---------------------------------------------------------------------------

// TestSentinelLatency asserts that parse + score < 5ms for 1000 connections.
//
// This is a functional test, not just a benchmark — it fails CI if the hot
// path regresses past the target latency.
//
// The 5ms budget was chosen based on a 2s poll interval:
// keeping the poll cycle at < 0.25% wall time means the angel
// is not CPU-visible under normal monitoring conditions.
func TestSentinelLatency(t *testing.T) {
	const numConns = 1000
	targetP99 := 5 * time.Millisecond
	if raceDetectorEnabled {
		// The race detector slows down the hot path by ~8-10x due to atomic syncs
		// on every map lookup and slice access.
		targetP99 = 40 * time.Millisecond
	}
	const iterations = 200 // run 200 cycles, measure P50/P95/P99

	rng      := rand.New(rand.NewSource(99))
	baseline := buildTestBaseline()
	dedup    := sentinel.NewDeduplicator()
	rateTrack := sentinel.NewRateTracker(5 * time.Second)
	scorer   := sentinel.NewScorer(baseline, rateTrack)

	// Pre-generate stable synthetic data so timing isn't inflated by generation.
	rawBufs := make([][]byte, iterations)
	for i := range rawBufs {
		rawBufs[i] = syntheticProcNetTCP(numConns, rng)
	}

	latencies := make([]time.Duration, iterations)

	for i := 0; i < iterations; i++ {
		start := time.Now()

		conns := linux.ParseTCPFile(rawBufs[i])
		for _, c := range conns {
			if !c.IsOutbound() || !c.IsEstablished() {
				continue
			}
			oc := sentinel.OutboundConn{
				RemoteIP:   c.RemoteIP,
				RemotePort: c.RemotePort,
			}
			if isNew := dedup.IsNew(oc, start); isNew {
				scorer.Score(oc, true, len(conns))
			}
		}

		latencies[i] = time.Since(start)
	}

	// Compute percentiles.
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	p50 := latencies[iterations/2]
	p95 := latencies[int(float64(iterations)*0.95)]
	p99 := latencies[int(float64(iterations)*0.99)]
	max := latencies[iterations-1]

	t.Logf("Sentinel pipeline latency @ %d connections (%d iterations):", numConns, iterations)
	t.Logf("  P50: %v", p50)
	t.Logf("  P95: %v", p95)
	t.Logf("  P99: %v", p99)
	t.Logf("  Max: %v", max)
	t.Logf("  Target P99: %v", targetP99)

	if p99 > targetP99 {
		t.Errorf("P99 latency %v exceeds target %v — Sentinel hot path is too slow", p99, targetP99)
	}
}

// ---------------------------------------------------------------------------
// Deduplicator stress test
// ---------------------------------------------------------------------------

// TestDeduplicatorHighChurn verifies that the deduplicator stays bounded
// under high connection churn (simulating a container host).
func TestDeduplicatorHighChurn(t *testing.T) {
	const (
		uniqueConnsPerCycle = 5000  // unique keys per poll (container churn)
		cycles              = 50
		maxSizeLimit        = 120_000 // must stay below 100k cap + one cycle slack
	)

	dedup := sentinel.NewDeduplicator()
	rng   := rand.New(rand.NewSource(7))

	for cycle := 0; cycle < cycles; cycle++ {
		now := time.Now()
		for i := 0; i < uniqueConnsPerCycle; i++ {
			oc := sentinel.OutboundConn{
				RemoteIP:   net.IPv4(byte(rng.Uint32()), byte(rng.Uint32()), byte(rng.Uint32()), byte(rng.Uint32())),
				RemotePort: uint16(rng.Uint32() % 65535),
			}
			dedup.IsNew(oc, now)
		}

		sz := dedup.Size()
		if sz > maxSizeLimit {
			t.Errorf("cycle %d: dedup map size %d exceeds limit %d", cycle, sz, maxSizeLimit)
		}
	}

	t.Logf("TestDeduplicatorHighChurn: final dedup size after %d×%d entries: %d",
		cycles, uniqueConnsPerCycle, dedup.Size())
}

// ---------------------------------------------------------------------------
// Scorer correctness under load
// ---------------------------------------------------------------------------

// TestScorerDetectionRate measures what fraction of "novel" connections
// get scored above the warn threshold.  Validates scoring isn't broken
// by the dedup cache or concurrent access patterns.
func TestScorerDetectionRate(t *testing.T) {
	const numConns = 500
	rng      := rand.New(rand.NewSource(13))
	baseline := buildTestBaseline()
	rateTrack := sentinel.NewRateTracker(5 * time.Second)
	scorer   := sentinel.NewScorer(baseline, rateTrack)

	warned  := 0
	critted := 0
	total   := 0

	// Generate novel connections (not in baseline).
	for i := 0; i < numConns; i++ {
		ip   := make(net.IP, 4)
		rng.Read(ip)
		// Ensure it's not private.
		ip[0] = 185
		port := randomPort(rng)

		oc := sentinel.OutboundConn{
			RemoteIP:   ip,
			RemotePort: port,
		}
		score := scorer.Score(oc, true /*isNew*/, numConns)
		total++
		switch score.Level() {
		case sentinel.ScoreWarn:
			warned++
		case sentinel.ScoreCrit:
			critted++
		}
	}

	alertRate := float64(warned+critted) / float64(total) * 100
	t.Logf("TestScorerDetectionRate: %d connections → %d WARN, %d CRIT (%.1f%% alert rate)",
		total, warned, critted, alertRate)

	// All novel public IPs should score at least WARN (score ≥ 3: new IP + any port).
	if alertRate < 90.0 {
		t.Errorf("alert rate %.1f%% is too low — expected >90%% for all-novel connections", alertRate)
	}
}

// ---------------------------------------------------------------------------
// ParseTCPFile benchmark in isolation
// ---------------------------------------------------------------------------

// BenchmarkParseTCPFile measures just the /proc/net/tcp parser hot path.
// This is the floor latency we cannot go below — everything else is overhead.
func BenchmarkParseTCPFile(b *testing.B) {
	for _, n := range []int{100, 500, 1000} {
		n := n
		b.Run(fmt.Sprintf("lines_%d", n), func(b *testing.B) {
			rng := rand.New(rand.NewSource(1))
			raw := syntheticProcNetTCP(n, rng)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = linux.ParseTCPFile(raw)
			}
		})
	}
}
