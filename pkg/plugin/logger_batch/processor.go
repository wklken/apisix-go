package logger_batch

import (
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
)

const (
	DefaultBatchMaxSize    = 1000
	DefaultMaxRetryCount   = 0
	DefaultRetryDelay      = time.Second
	DefaultBufferDuration  = time.Minute
	DefaultInactiveTimeout = 5 * time.Second
)

type DeliveryFunc func(entries []map[string]any, batchMaxSize int) (firstFail int, err error)

type Config struct {
	Name              string
	BatchMaxSize      int
	MaxRetryCount     int
	RetryDelay        time.Duration
	BufferDuration    time.Duration
	InactiveTimeout   time.Duration
	MaxPendingEntries int
	RouteID           string
	ServerAddr        string
}

type Processor struct {
	config  Config
	deliver DeliveryFunc

	mu          sync.Mutex
	wg          sync.WaitGroup
	timer       *time.Timer
	stopped     bool
	buffer      []map[string]any
	firstEntry  time.Time
	lastEntry   time.Time
	pending     int
	processing  int
	dropped     int
	delivered   int
	failedDrops int
}

func New(config Config, deliver DeliveryFunc) *Processor {
	config.applyDefaults()
	return &Processor{
		config:  config,
		deliver: deliver,
		buffer:  make([]map[string]any, 0, config.BatchMaxSize),
	}
}

func (p *Processor) Push(entry map[string]any) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return false
	}
	if p.config.MaxPendingEntries > 0 && p.pending > p.config.MaxPendingEntries {
		p.dropped++
		logger.Errorf(
			"max pending entries limit exceeded for logger batch processor [%s], pending [%d], max_pending_entries [%d]",
			p.config.Name,
			p.pending,
			p.config.MaxPendingEntries,
		)
		return false
	}

	now := time.Now()
	if len(p.buffer) == 0 {
		p.firstEntry = now
	}
	p.lastEntry = now
	p.buffer = append(p.buffer, entry)
	p.pending++
	p.setBufferedMetricLocked()

	if len(p.buffer) >= p.config.BatchMaxSize {
		p.flushLocked()
		return true
	}

	p.ensureTimerLocked()
	return true
}

func (p *Processor) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.flushLocked()
}

func (p *Processor) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.flushLocked()
	p.stopTimerLocked()
	p.mu.Unlock()

	p.wg.Wait()
}

func (p *Processor) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()

	return Stats{
		Pending:     p.pending,
		Processing:  p.processing,
		Buffered:    len(p.buffer),
		Dropped:     p.dropped,
		Delivered:   p.delivered,
		FailedDrops: p.failedDrops,
	}
}

func (p *Processor) ensureTimerLocked() {
	if p.timer != nil || len(p.buffer) == 0 {
		return
	}
	p.timer = time.AfterFunc(p.config.InactiveTimeout, p.onTimer)
}

func (p *Processor) onTimer() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.timer = nil
	if p.stopped || len(p.buffer) == 0 {
		return
	}

	now := time.Now()
	if now.Sub(p.lastEntry) >= p.config.InactiveTimeout || now.Sub(p.firstEntry) >= p.config.BufferDuration {
		p.flushLocked()
		return
	}
	p.ensureTimerLocked()
}

func (p *Processor) flushLocked() {
	if len(p.buffer) == 0 {
		return
	}

	batch := append([]map[string]any(nil), p.buffer...)
	p.buffer = p.buffer[:0]
	p.firstEntry = time.Time{}
	p.lastEntry = time.Time{}
	p.setBufferedMetricLocked()
	p.stopTimerLocked()
	p.processing += len(batch)

	p.wg.Add(1)
	go p.process(batch)
}

func (p *Processor) setBufferedMetricLocked() {
	if p.config.Name == "" || p.config.RouteID == "" || p.config.ServerAddr == "" {
		return
	}
	metrics.SetBatchProcessEntries(p.config.Name, p.config.RouteID, p.config.ServerAddr, len(p.buffer))
}

func (p *Processor) stopTimerLocked() {
	if p.timer == nil {
		return
	}
	p.timer.Stop()
	p.timer = nil
}

func (p *Processor) process(batch []map[string]any) {
	defer p.wg.Done()

	entries := batch
	for attempt := 0; ; attempt++ {
		firstFail, err := p.deliver(entries, p.config.BatchMaxSize)
		if err == nil {
			p.markProcessed(len(entries), true)
			return
		}

		if firstFail > 1 && firstFail <= len(entries) {
			processed := firstFail - 1
			p.markProcessed(processed, true)
			entries = append([]map[string]any(nil), entries[processed:]...)
		}

		if attempt >= p.config.MaxRetryCount || len(entries) == 0 {
			logger.Errorf(
				"logger batch processor [%s] exceeded max_retry_count [%d], dropping %d entries: %s",
				p.config.Name,
				p.config.MaxRetryCount,
				len(entries),
				err,
			)
			p.markProcessed(len(entries), false)
			return
		}

		time.Sleep(p.config.RetryDelay)
	}
}

func (p *Processor) markProcessed(count int, delivered bool) {
	if count <= 0 {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pending -= count
	if p.pending < 0 {
		p.pending = 0
	}
	p.processing -= count
	if p.processing < 0 {
		p.processing = 0
	}
	if delivered {
		p.delivered += count
		return
	}
	p.failedDrops += count
}

func (c *Config) applyDefaults() {
	if c.Name == "" {
		c.Name = "log buffer"
	}
	if c.BatchMaxSize <= 0 {
		c.BatchMaxSize = DefaultBatchMaxSize
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = DefaultRetryDelay
	}
	if c.BufferDuration <= 0 {
		c.BufferDuration = DefaultBufferDuration
	}
	if c.InactiveTimeout <= 0 {
		c.InactiveTimeout = DefaultInactiveTimeout
	}
	if c.MaxRetryCount < 0 {
		c.MaxRetryCount = DefaultMaxRetryCount
	}
}

type Stats struct {
	Pending     int
	Processing  int
	Buffered    int
	Dropped     int
	Delivered   int
	FailedDrops int
}
