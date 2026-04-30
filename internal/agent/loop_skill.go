package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// LoopSkill enables continuous task execution with dynamic pacing.
// Inspired by OpenClaw's loop skill and Hermes' ScheduleWakeup.
type LoopSkill struct {
	enabled     bool
	interval    time.Duration
	maxPerHour  int
	maintenance bool

	mu        sync.RWMutex
	turnCount int
	lastTurn  time.Time
	startTime time.Time
	stopCh    chan struct{}
}

// NewLoopSkill creates a new loop skill.
func NewLoopSkill() *LoopSkill {
	return &LoopSkill{
		interval:   1 * time.Minute,
		maxPerHour: 60,
		startTime:  time.Now(),
		lastTurn:   time.Now(),
		stopCh:     make(chan struct{}),
	}
}

// Enable starts the loop with the given interval.
func (ls *LoopSkill) Enable(interval time.Duration) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.enabled = true
	ls.interval = interval
	ls.turnCount = 0
	ls.startTime = time.Now()
	ls.stopCh = make(chan struct{})
}

// Disable stops the loop.
func (ls *LoopSkill) Disable() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.enabled = false
	select {
	case <-ls.stopCh:
	default:
		close(ls.stopCh)
	}
}

// IsEnabled returns whether the loop is running.
func (ls *LoopSkill) IsEnabled() bool {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.enabled
}

// Interval returns the current loop interval.
func (ls *LoopSkill) Interval() time.Duration {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return ls.interval
}

// Stats returns loop statistics.
func (ls *LoopSkill) Stats() LoopSkillStats {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	elapsed := time.Since(ls.startTime)
	var rate float64
	if elapsed > 0 {
		rate = float64(ls.turnCount) / elapsed.Hours()
	}
	return LoopSkillStats{
		Enabled:     ls.enabled,
		Interval:    ls.interval.String(),
		TurnCount:   ls.turnCount,
		RatePerHr:   rate,
		Uptime:      elapsed.String(),
		LastTurn:    ls.lastTurn,
		Maintenance: ls.maintenance,
	}
}

// RecordTurn records a completed turn for rate limiting.
func (ls *LoopSkill) RecordTurn() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.turnCount++
	ls.lastTurn = time.Now()
}

// ShouldContinue checks if the loop should continue based on rate limits.
func (ls *LoopSkill) ShouldContinue() bool {
	ls.mu.RLock()
	defer ls.mu.RUnlock()

	if !ls.enabled {
		return false
	}

	if ls.maintenance {
		return false
	}

	// Rate limit check
	if ls.maxPerHour > 0 {
		elapsed := time.Since(ls.startTime)
		if elapsed > 0 {
			rate := float64(ls.turnCount) / elapsed.Hours()
			if rate > float64(ls.maxPerHour) {
				return false
			}
		}
	}

	return true
}

// SetMaintenance enables/disables maintenance mode.
func (ls *LoopSkill) SetMaintenance(enabled bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.maintenance = enabled
}

// Run starts the loop execution in the background.
// Calls onTurn for each iteration.
func (ls *LoopSkill) Run(ctx context.Context, onTurn func(context.Context) error) error {
	ls.mu.RLock()
	stopCh := ls.stopCh
	interval := ls.interval
	ls.mu.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopCh:
			return nil
		case <-ticker.C:
			if !ls.ShouldContinue() {
				ls.Disable()
				return fmt.Errorf("loop stopped: rate limit exceeded or maintenance mode")
			}
			if err := onTurn(ctx); err != nil {
				return err
			}
			ls.RecordTurn()
		}
	}
}

// DynamicPace adjusts the interval based on activity.
// Inspired by OpenClaw's dynamic loop pacing.
func (ls *LoopSkill) DynamicPace(activity float64) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	// activity: 0.0 (idle) to 1.0 (very active)
	// Low activity -> longer intervals, High activity -> shorter intervals

	minInterval := 30 * time.Second
	maxInterval := 10 * time.Minute

	// Target rate: 10-30 turns per hour based on activity
	targetPerHour := 10 + int(activity*20)
	if targetPerHour < 1 {
		targetPerHour = 1
	}

	newInterval := time.Duration(float64(time.Hour) / float64(targetPerHour))
	if newInterval < minInterval {
		newInterval = minInterval
	}
	if newInterval > maxInterval {
		newInterval = maxInterval
	}

	ls.interval = newInterval
}

// LoopSkillStats holds loop execution statistics.
type LoopSkillStats struct {
	Enabled     bool      `json:"enabled"`
	Interval    string    `json:"interval"`
	TurnCount   int       `json:"turn_count"`
	RatePerHr   float64   `json:"rate_per_hour"`
	Uptime      string    `json:"uptime"`
	LastTurn    time.Time `json:"last_turn"`
	Maintenance bool      `json:"maintenance"`
}
