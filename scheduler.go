package caddy_netx_geolocation

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// refreshScheduler runs the daily bulk fetch at a configured time.
type refreshScheduler struct {
	refreshHour   int
	refreshMinute int
	fetcher       *fetcher
	store         *dataStore
	logger        *zap.Logger
	done          chan struct{}
}

func newScheduler(refreshTime string, f *fetcher, s *dataStore, logger *zap.Logger) (*refreshScheduler, error) {
	hour, minute, err := parseTime(refreshTime)
	if err != nil {
		return nil, err
	}

	return &refreshScheduler{
		refreshHour:   hour,
		refreshMinute: minute,
		fetcher:       f,
		store:         s,
		logger:        logger,
		done:          make(chan struct{}),
	}, nil
}

func parseTime(s string) (int, int, error) {
	var hour, minute int
	_, err := fmt.Sscanf(s, "%d:%d", &hour, &minute)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid refresh_time %q: expected HH:MM format", s)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid refresh_time %q: hour must be 0-23, minute 0-59", s)
	}
	return hour, minute, nil
}

// Start begins the scheduler loop in a goroutine.
func (rs *refreshScheduler) Start() {
	go rs.run()
}

// Stop signals the scheduler to shut down.
func (rs *refreshScheduler) Stop() {
	close(rs.done)
}

func (rs *refreshScheduler) run() {
	for {
		wait := rs.durationUntilNext()
		rs.logger.Info("next refresh scheduled",
			zap.Duration("in", wait),
			zap.String("at", fmt.Sprintf("%02d:%02d", rs.refreshHour, rs.refreshMinute)),
		)

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			rs.doRefresh()
		case <-rs.done:
			timer.Stop()
			return
		}
	}
}

func (rs *refreshScheduler) doRefresh() {
	rs.logger.Info("starting daily refresh")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	entries, err := rs.fetcher.FetchAll(ctx)
	if err != nil {
		rs.logger.Error("daily refresh failed, keeping existing data",
			zap.Error(err),
			zap.Int("current_entries", rs.store.EntryCount()),
		)
		return
	}

	loaded, skipped := rs.store.Replace(entries)
	rs.logger.Info("trie rebuilt", zap.Int("loaded", loaded), zap.Int("skipped", skipped))

	if err := rs.store.SaveToFile(); err != nil {
		rs.logger.Error("failed to save data file", zap.Error(err))
	} else {
		rs.logger.Info("data file saved", zap.Int("entries", loaded))
	}
}

func (rs *refreshScheduler) durationUntilNext() time.Duration {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), rs.refreshHour, rs.refreshMinute, 0, 0, now.Location())

	if next.Before(now) || next.Equal(now) {
		next = next.Add(24 * time.Hour)
	}

	return next.Sub(now)
}
