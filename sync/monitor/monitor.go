package monitor

import (
	"context"
	"sync"
	"time"

	"github.com/Conflux-Chain/go-conflux-util/health"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	errSyncHeightNotGrowing = errors.New("sync height not growing")
)

// Config holds the configuration parameters for the sync monitor.
type Config struct {
	// Health check configuration
	Health health.CounterConfig

	// Maximum allowable duration for sync height to remain unchanged.
	MaxStalledDuration time.Duration `default:"5m"`
	// Maximum allowed lag in height between the latest chain height and the sync height.
	MaxAllowedLag uint64 `default:"200"`
}

// NewConfig returns a new Config with default values.
func NewConfig() (conf Config) {
	return Config{
		MaxAllowedLag:      200,
		MaxStalledDuration: 5 * time.Minute,
		Health:             health.CounterConfig{Remind: 5},
	}
}

// Monitor periodically checks sync height growth.
type Monitor struct {
	Config
	mu     sync.Mutex
	cancel func()

	currentHeight   uint64
	lastAdvancedAt  time.Time
	getLatestHeight func() (uint64, error)

	healthStatus health.Counter
}

func NewMonitor(cfg Config, latestHeightFunc func() (uint64, error)) *Monitor {
	return &Monitor{
		Config:          cfg,
		getLatestHeight: latestHeightFunc,
	}
}

// Start begins the monitoring process by checking sync status periodically.
func (m *Monitor) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel any existing monitoring goroutine if already running
	if m.cancel != nil {
		m.cancel()
	}

	// Create a new cancellable context for this monitor
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	// Reset the monitor before starting
	m.reset()

	go m.run(ctx)
}

// run performs the periodic sync status check.
func (m *Monitor) run(ctx context.Context) {
	ticker := time.NewTicker(m.MaxStalledDuration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkOnce()
		}
	}
}

// Update updates the current sync height.
func (m *Monitor) Update(newHeight uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Update the last advanced time if the height increases
	if newHeight > m.currentHeight {
		m.lastAdvancedAt = time.Now()
	}
	m.currentHeight = newHeight
}

// Stop halts the monitoring process.
func (m *Monitor) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel the running context if monitoring is active
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// reset resets the monitor's current height and sets the last advanced time to now.
func (m *Monitor) reset() {
	m.currentHeight = 0
	m.lastAdvancedAt = time.Now()
}

func (m *Monitor) checkOnce() {
	// Fetch the latest height to compare
	latestHeight, err := m.getLatestHeight()
	if err != nil {
		logrus.WithError(err).Info("Sync monitor failed to fetch latest height")
		m.onFailure(errors.WithMessage(err, "fetch latest height error"))
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// The sync height grew within the time window
	if time.Since(m.lastAdvancedAt) < m.MaxStalledDuration {
		m.onSuccess()
		return
	}

	// The sync height didn't grow, let's check if it has already caught up.
	if m.currentHeight+m.MaxAllowedLag-1 < latestHeight {
		m.onFailure(errSyncHeightNotGrowing, m.currentHeight, latestHeight)
	} else {
		m.onSuccess()
	}
}

func (m *Monitor) onSuccess() {
	recovered, failures := m.healthStatus.OnSuccess(m.Health)
	if recovered {
		logrus.WithFields(logrus.Fields{
			"failures": failures,
		}).Info("Sync monitor recovered after failures")
	}
}

func (m *Monitor) onFailure(err error, heights ...uint64) {
	_, unrecovered, failures := m.healthStatus.OnFailure(m.Health)
	if unrecovered {
		logrus.WithFields(logrus.Fields{
			"ctxHeights": heights,
			"failures":   failures,
		}).WithError(err).Error("Sync monitor not recovered after failures")
	}
}
