// Package sentinel — scorer.go
//
// The Scorer evaluates one outbound connection against the baseline model
// and produces an AnomalyScore.  Scoring is intentionally additive and
// readable — each rule contributes a named point value.  This makes
// tuning transparent and the rationale auditable in emitted events.
//
// Score thresholds:
//
//	score < WarnThreshold   → no event
//	score >= WarnThreshold  → WARN event
//	score >= CritThreshold  → CRITICAL event
//
// Default thresholds:  Warn=3, Crit=6.
//
// Rule table:
//
//	Rule                                Weight  Rationale
//	────────────────────────────────────────────────────────
//	New remote IP (not in baseline)     +3      Most common indicator
//	New remote port (not in baseline)   +2      Unusual service contact
//	Ephemeral/high port (>49152)        +1      Often C2 channels
//	Very high port (>60000)             +1      Stacking with above
//	Connection burst (>20 in 5s)        +4      Scanning / beaconing
//	Concurrent count spike (>2× max)    +3      Mass connection event
//	Private→public (RFC1918 src)        -1      Usually legitimate
//	Loopback remote                     -5      Internal — never alert
package sentinel

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Score
// ---------------------------------------------------------------------------

// AnomalyScore is the result of evaluating one connection against the baseline.
type AnomalyScore struct {
	Total    int
	Reasons  []string // human-readable explanation of each contributing rule
	IsNew    bool     // was this connection new (from deduplicator)?
}

// Level returns the alert level implied by the score.
func (s *AnomalyScore) Level() ScoreLevel {
	switch {
	case s.Total >= CritThreshold:
		return ScoreCrit
	case s.Total >= WarnThreshold:
		return ScoreWarn
	default:
		return ScoreOK
	}
}

// Summary returns a one-line description: "score 5 (new IP + high port)"
func (s *AnomalyScore) Summary() string {
	if len(s.Reasons) == 0 {
		return fmt.Sprintf("score %d (no anomalies)", s.Total)
	}
	return fmt.Sprintf("score %d (%s)", s.Total, strings.Join(s.Reasons, " + "))
}

// ScoreLevel is the alert tier produced by a score.
type ScoreLevel uint8

const (
	ScoreOK   ScoreLevel = iota // below WarnThreshold — no event
	ScoreWarn                   // warn-level event
	ScoreCrit                   // critical-level event
)

// Default score thresholds. Tunable via config in a future pass.
const (
	WarnThreshold = 3
	CritThreshold = 6
)

// ---------------------------------------------------------------------------
// Scorer
// ---------------------------------------------------------------------------

// Scorer evaluates connections against a frozen Baseline.
type Scorer struct {
	baseline        *Baseline
	rate            *RateTracker
	burstThreshold  int // new connections per rateWindow before +4 penalty
	spikeMultiplier int // maxConcurrent * this = spike threshold
}

// NewScorer creates a Scorer using the given frozen baseline.
func NewScorer(b *Baseline, rate *RateTracker) *Scorer {
	return &Scorer{
		baseline:        b,
		rate:            rate,
		burstThreshold:  20,
		spikeMultiplier: 2,
	}
}

// Score evaluates conn and returns an AnomalyScore.
// conn.procName and conn.pid are used for the message context only.
//
// Parameters:
//
//	conn          — the outbound connection to evaluate
//	isNew         — from the Deduplicator (connection not seen recently)
//	currentTotal  — current total established connection count
//
// Hot-path optimisation: the score total is computed first without any
// string allocations. Reasons strings are built only when the score
// crosses WarnThreshold, since Reasons are only needed when emitting an event.
func (s *Scorer) Score(conn OutboundConn, isNew bool, currentTotal int) AnomalyScore {
	score := AnomalyScore{IsNew: isNew}

	// Loopback remote — internal traffic, never alert.
	if conn.RemoteIP.IsLoopback() {
		score.Total -= 5
		return score // exit early — no further rules apply
	}

	// --- Numeric scoring pass (no allocations) ---

	var (
		newIP    bool
		newPort  bool
		highPort bool
		vHighPort bool
		burst    int
		spike    bool
	)

	if isNew {
		newIP = !s.baseline.KnowsIP(conn.RemoteIP)
		if newIP {
			score.Total += 3
		}

		newPort = !s.baseline.KnowsPort(conn.RemotePort)
		if newPort {
			score.Total += 2
		}

		highPort = conn.RemotePort > 49152
		if highPort {
			score.Total++
		}

		vHighPort = conn.RemotePort > 60000
		if vHighPort {
			score.Total++
		}

		burst = s.rate.Add()
		if burst > s.burstThreshold {
			score.Total += 4
		}
	}

	spikeThreshold := s.baseline.MaxConcurrent() * s.spikeMultiplier
	if spikeThreshold > 0 && currentTotal > spikeThreshold {
		score.Total += 3
		spike = true
	}

	if isRFC1918(conn.RemoteIP) {
		score.Total--
	}

	// Only build Reasons strings if we're actually going to emit an event.
	// This eliminates []string allocs + fmt.Sprintf calls for the common OK path.
	if score.Total < WarnThreshold {
		return score
	}

	// --- Reasons building pass (allocates only for above-threshold connections) ---

	if isNew {
		if newIP {
			score.Reasons = append(score.Reasons, "new IP "+conn.RemoteIP.String())
		}
		if newPort {
			score.Reasons = append(score.Reasons, "new port "+strconv.Itoa(int(conn.RemotePort)))
		}
		if highPort {
			score.Reasons = append(score.Reasons, "high port "+strconv.Itoa(int(conn.RemotePort)))
		}
		if vHighPort {
			score.Reasons = append(score.Reasons, "very high port")
		}
		if burst > s.burstThreshold {
			score.Reasons = append(score.Reasons, "burst "+strconv.Itoa(burst)+" new conns/5s")
		}
	}
	if spike {
		score.Reasons = append(score.Reasons,
			"conn spike "+strconv.Itoa(currentTotal)+" (baseline max "+strconv.Itoa(s.baseline.MaxConcurrent())+")")
	}

	return score
}

// ---------------------------------------------------------------------------
// IP classification helpers
// ---------------------------------------------------------------------------

var (
	// RFC 1918 private ranges.
	rfc1918_10    = mustParseCIDR("10.0.0.0/8")
	rfc1918_172   = mustParseCIDR("172.16.0.0/12")
	rfc1918_192   = mustParseCIDR("192.168.0.0/16")
	linkLocal     = mustParseCIDR("169.254.0.0/16")
	cgnat         = mustParseCIDR("100.64.0.0/10")
)

// isRFC1918 reports whether ip is in a private address space.
func isRFC1918(ip net.IP) bool {
	return rfc1918_10.Contains(ip) ||
		rfc1918_172.Contains(ip) ||
		rfc1918_192.Contains(ip) ||
		linkLocal.Contains(ip) ||
		cgnat.Contains(ip)
}

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("scorer: bad CIDR %s: %v", s, err))
	}
	return n
}
