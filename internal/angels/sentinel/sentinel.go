package sentinel

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/nacreousdawn596/angellab/pkg/ipc"
	"github.com/nacreousdawn596/angellab/pkg/linux"
	"github.com/nacreousdawn596/angellab/pkg/logging"
)

type ConnCache struct {
	mu      sync.RWMutex
	conns   []linux.NetConn
	builtAt time.Time
	ttl     time.Duration
}

func NewConnCache(ttl time.Duration) *ConnCache {
	return &ConnCache{ttl: ttl}
}

var globalConnCache = NewConnCache(5 * time.Second)

func (c *ConnCache) Get() ([]linux.NetConn, error) {
	c.mu.RLock()
	if c.conns != nil && time.Since(c.builtAt) < c.ttl {
		conns := c.conns
		c.mu.RUnlock()
		return conns, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conns != nil && time.Since(c.builtAt) < c.ttl {
		return c.conns, nil
	}

	fresh, err := linux.ReadTCP()
	if err != nil {
		return nil, err
	}

	c.conns = fresh
	c.builtAt = time.Now()

	return fresh, nil
}

func Run() {
	var (
		id        = flag.String("id", "", "angel ID assigned by Lab")
		labSocket = flag.String("lab", "/run/angellab/lab.sock", "path to lab.sock")
	)
	flag.Parse()

	if *id == "" {
		fmt.Fprintln(os.Stderr, "sentinel: --id is required")
		os.Exit(1)
	}

	log := logging.NewDefault(fmt.Sprintf("Sentinel/%s", *id))

	cfg, err := readConfig(os.Stdin)
	if err != nil {
		log.Crit("read config: %v", err)
		os.Exit(1)
	}

	s := &Sentinel{
		id:        *id,
		labSocket: *labSocket,
		cfg:       cfg,
		log:       log,
		dedup:     NewDeduplicator(),
		rateTrack: NewRateTracker(5 * time.Second),
		inodes:    NewInodeCache(),
		startedAt: time.Now(),
	}

	s.inodes.Get()

	if err := s.run(context.Background()); err != nil {
		log.Crit("sentinel exited: %v", err)
		os.Exit(1)
	}
}

type phase uint8

const (
	phaseTraining phase = iota
	phaseActive
)

func (p phase) String() string {
	if p == phaseTraining {
		return "TRAINING"
	}
	return "ACTIVE"
}

type Sentinel struct {
	id        string
	labSocket string
	cfg       *Config
	log       *logging.Logger
	startedAt time.Time

	conn       *ipc.Conn
	cpuSampler *linux.CPUSampler

	phase     phase
	baseline  *Baseline
	scorer    *Scorer
	dedup     *Deduplicator
	rateTrack *RateTracker
	inodes    *InodeCache

	totalEventsEmitted int
	lastPollCount      int
	pollsSinceLog      int

	inodeSnapshot linux.InodeMap
	inodeBuiltAt  time.Time
}

func (s *Sentinel) run(ctx context.Context) error {

	conn, err := ipc.Dial(s.labSocket, ipc.RoleAngel)
	if err != nil {
		return fmt.Errorf("dial lab: %w", err)
	}

	s.conn = conn
	defer conn.Close()

	if err := s.register(); err != nil {
		return err
	}

	s.cpuSampler, _ = linux.NewCPUSampler()

	if saved, err := loadBaseline(s.id, s.cfg.StateDir); err != nil {
		s.log.Warn("could not load saved baseline: %v", err)
		s.baseline = NewBaseline()
	} else if saved != nil {
		s.baseline = saved
		s.phase = phaseActive
		s.scorer = NewScorer(s.baseline, s.rateTrack)
	} else {
		s.baseline = NewBaseline()
	}

	pollTick := time.NewTicker(s.cfg.PollInterval)
	heartbeatTick := time.NewTicker(10 * time.Second)
	baselineTimer := time.NewTimer(s.cfg.BaselineDuration)

	defer pollTick.Stop()
	defer heartbeatTick.Stop()
	defer baselineTimer.Stop()

	if err := s.sendHeartbeat(); err != nil {
		return err
	}

	for {
		select {

		case <-ctx.Done():
			return nil

		case <-baselineTimer.C:
			if s.phase == phaseTraining {
				s.finaliseBaseline()
			}

		case <-pollTick.C:
			s.poll()

		case <-heartbeatTick.C:
			if err := s.sendHeartbeat(); err != nil {
				return err
			}
		}
	}
}

func (s *Sentinel) poll() {

	raw, err := globalConnCache.Get()
	if err != nil && len(raw) == 0 {
		s.log.Warn("poll: read connections: %v", err)
		return
	}

	if s.inodeSnapshot == nil || time.Since(s.inodeBuiltAt) > 5*time.Second {
		s.inodeSnapshot = s.inodes.Get()
		s.inodeBuiltAt = time.Now()
	}
	inodes := s.inodeSnapshot

	count := 0

	var trainingBatch []OutboundConn
	if s.phase == phaseTraining {
		trainingBatch = make([]OutboundConn, 0, len(raw))
	}

	now := time.Now()
	for i := range raw {
		c := &raw[i]

		if !c.IsOutbound() || !c.IsEstablished() {
			continue
		}

		oc := OutboundConn{
			RemoteIP:   c.RemoteIP,
			RemotePort: c.RemotePort,
			inode:      c.Inode,
			proto:      c.Proto,
		}

		if !s.dedup.IsNew(oc, now) {
			continue
		}

		count++

		score := s.scorer.Score(oc, true, count)

		// Only enrich if actually needed
		if score.Level() != ScoreOK || s.phase == phaseTraining {
			if info, ok := inodes.Lookup(c.Inode); ok {
				oc.procName = info.Comm
				oc.pid = info.PID
				oc.exePath = info.ExePath
			}
		}

		if score.Level() != ScoreOK {
			s.emitAnomaly(oc, score)
		}

		if s.phase == phaseTraining {
			trainingBatch = append(trainingBatch, oc)
		}
	}

	if s.phase == phaseTraining {
		s.baseline.Observe(trainingBatch)
	}

	s.lastPollCount = count
}

func (s *Sentinel) finaliseBaseline() {

	s.baseline.Freeze()
	s.phase = phaseActive
	s.scorer = NewScorer(s.baseline, s.rateTrack)

	saveBaseline(s.id, s.cfg.StateDir, s.baseline)
}

func (s *Sentinel) emitAnomaly(conn OutboundConn, score AnomalyScore) {

	s.totalEventsEmitted++

	msg := fmt.Sprintf(
		"suspicious outbound → %s:%d (score %d)",
		conn.RemoteIP,
		conn.RemotePort,
		score.Total,
	)

	s.emitEvent(ipc.SeverityWarn, msg, map[string]string{
		"remote_ip":   conn.RemoteIP.String(),
		"remote_port": fmt.Sprintf("%d", conn.RemotePort),
	})
}

func (s *Sentinel) register() error {

	payload, err := ipc.EncodePayload(&ipc.RegisterPayload{
		AngelID:   s.id,
		AngelType: "sentinel",
		PID:       os.Getpid(),
	})

	if err != nil {
		return err
	}

	return s.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindRegister,
		Payload: payload,
	})
}

func (s *Sentinel) sendHeartbeat() error {

	stat, _ := linux.ReadSelfStat()

	var rss uint64
	if stat != nil {
		rss = stat.RSSBytes()
	}

	var cpu float64
	if s.cpuSampler != nil {
		cpu, _ = s.cpuSampler.Sample()
	}

	payload, err := ipc.EncodePayload(&ipc.HeartbeatPayload{
		AngelID:    s.id,
		State:      s.phase.String(),
		Uptime:     int64(time.Since(s.startedAt).Seconds()),
		CPUPercent: cpu,
		RSSBytes:   rss,
		Goroutines: runtime.NumGoroutine(),
		FDCount:    linux.CountFDs(),
	})

	if err != nil {
		return err
	}

	return s.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindHeartbeat,
		Payload: payload,
	})
}

func (s *Sentinel) emitEvent(severity ipc.Severity, message string, meta map[string]string) {

	payload, err := ipc.EncodePayload(&ipc.EventPayload{
		AngelID:   s.id,
		Severity:  severity,
		Message:   message,
		Timestamp: time.Now(),
		Meta:      meta,
	})

	if err != nil {
		s.log.Warn("encode event: %v", err)
		return
	}

	if err := s.conn.Send(&ipc.Message{
		Version: ipc.ProtocolVersion,
		Kind:    ipc.KindEvent,
		Payload: payload,
	}); err != nil {
		s.log.Warn("send event: %v", err)
	}
}