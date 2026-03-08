package sentinel

import (
	"net"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Baseline tests
// ---------------------------------------------------------------------------

func TestBaselineObserveAndQuery(t *testing.T) {
	b := NewBaseline()

	conns := []OutboundConn{
		{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443},
		{RemoteIP: net.ParseIP("1.1.1.1"), RemotePort: 53},
		{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443}, // duplicate
	}
	b.Observe(conns)
	b.Observe(conns)
	b.Freeze()

	if !b.KnowsIP(net.ParseIP("8.8.8.8")) {
		t.Error("KnowsIP(8.8.8.8) should be true")
	}
	if !b.KnowsIP(net.ParseIP("1.1.1.1")) {
		t.Error("KnowsIP(1.1.1.1) should be true")
	}
	if b.KnowsIP(net.ParseIP("185.203.1.1")) {
		t.Error("KnowsIP(185.203.1.1) should be false")
	}
	if !b.KnowsPort(443) {
		t.Error("KnowsPort(443) should be true")
	}
	if b.KnowsPort(4444) {
		t.Error("KnowsPort(4444) should be false")
	}
	if b.KnownIPCount() != 2 {
		t.Errorf("KnownIPCount = %d, want 2", b.KnownIPCount())
	}
	if b.MaxConcurrent() != 3 {
		t.Errorf("MaxConcurrent = %d, want 3", b.MaxConcurrent())
	}
}

func TestBaselineFrozenIgnoresObserve(t *testing.T) {
	b := NewBaseline()
	b.Observe([]OutboundConn{{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443}})
	b.Freeze()
	// This should be a no-op.
	b.Observe([]OutboundConn{{RemoteIP: net.ParseIP("9.9.9.9"), RemotePort: 80}})
	if b.KnowsIP(net.ParseIP("9.9.9.9")) {
		t.Error("frozen baseline should not accept new observations")
	}
}

// ---------------------------------------------------------------------------
// Deduplicator tests
// ---------------------------------------------------------------------------

func TestDeduplicatorIsNew(t *testing.T) {
	d := NewDeduplicator()

	ip1 := OutboundConn{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443}
	ip2 := OutboundConn{RemoteIP: net.ParseIP("1.1.1.1"), RemotePort: 53}

	now := time.Now()
	// First time: new.
	if !d.IsNew(ip1, now) {
		t.Error("first observation should be new")
	}
	// Second time within TTL: not new.
	if d.IsNew(ip1, now.Add(time.Second)) {
		t.Error("second observation within TTL should not be new")
	}
	// Different key: new.
	if !d.IsNew(ip2, now) {
		t.Error("different key should be new")
	}
}

func TestDeduplicatorReset(t *testing.T) {
	d := NewDeduplicator()
	ip := OutboundConn{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443}
	d.IsNew(ip, time.Now())
	d.Reset()
	if !d.IsNew(ip, time.Now()) {
		t.Error("after Reset, key should be new again")
	}
}

// ---------------------------------------------------------------------------
// Scorer tests
// ---------------------------------------------------------------------------

func TestScorerNewIP(t *testing.T) {
	b := NewBaseline()
	b.Observe([]OutboundConn{{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443}})
	b.Freeze()

	rate := NewRateTracker(5 * time.Second)
	scorer := NewScorer(b, rate)

	// Unknown IP, new conn: should score at or above WarnThreshold.
	newConn := OutboundConn{
		RemoteIP:   net.ParseIP("185.203.1.1"),
		RemotePort: 443,
	}
	score := scorer.Score(newConn, true, 1)
	if score.Level() == ScoreOK {
		t.Errorf("new IP should score >= WarnThreshold, got %d (%s)",
			score.Total, score.Summary())
	}
	t.Logf("new IP score: %s", score.Summary())
}

func TestScorerKnownIPNoAlert(t *testing.T) {
	b := NewBaseline()
	b.Observe([]OutboundConn{{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443}})
	b.Freeze()

	rate := NewRateTracker(5 * time.Second)
	scorer := NewScorer(b, rate)

	// Known IP, known port, not new: should not alert.
	knownConn := OutboundConn{
		RemoteIP:   net.ParseIP("8.8.8.8"),
		RemotePort: 443,
	}
	score := scorer.Score(knownConn, false /* not new */, 1)
	if score.Level() != ScoreOK {
		t.Errorf("known connection should not alert, got %s", score.Summary())
	}
}

func TestScorerLoopbackNeverAlerts(t *testing.T) {
	b := NewBaseline()
	b.Freeze()
	rate := NewRateTracker(5 * time.Second)
	scorer := NewScorer(b, rate)

	loopback := OutboundConn{
		RemoteIP:   net.ParseIP("127.0.0.1"),
		RemotePort: 8080,
	}
	score := scorer.Score(loopback, true, 1)
	if score.Level() != ScoreOK {
		t.Errorf("loopback should never alert, got score %d", score.Total)
	}
}

func TestScorerHighPort(t *testing.T) {
	b := NewBaseline()
	b.Observe([]OutboundConn{{RemoteIP: net.ParseIP("8.8.8.8"), RemotePort: 443}})
	b.Freeze()
	rate := NewRateTracker(5 * time.Second)
	scorer := NewScorer(b, rate)

	// New IP + very high port: should be critical.
	conn := OutboundConn{
		RemoteIP:   net.ParseIP("203.0.113.1"),
		RemotePort: 62000,
	}
	score := scorer.Score(conn, true, 1)
	t.Logf("high port score: %s", score.Summary())
	if score.Level() != ScoreCrit {
		t.Errorf("new IP + very high port should be critical, got level %d score %d",
			score.Level(), score.Total)
	}
}

// ---------------------------------------------------------------------------
// RateTracker tests
// ---------------------------------------------------------------------------

func TestRateTrackerCount(t *testing.T) {
	r := NewRateTracker(1 * time.Second)
	for i := 0; i < 5; i++ {
		r.Add()
	}
	count := r.Count()
	if count != 5 {
		t.Errorf("Count() = %d, want 5", count)
	}
}

func TestRateTrackerExpiry(t *testing.T) {
	r := NewRateTracker(50 * time.Millisecond)
	r.Add()
	r.Add()
	time.Sleep(100 * time.Millisecond)
	if r.Count() != 0 {
		t.Errorf("events should have expired, got Count=%d", r.Count())
	}
}
