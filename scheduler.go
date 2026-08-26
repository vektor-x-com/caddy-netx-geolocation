package caddy_netx_geolocation

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// refreshTimeout bounds a single export download. The transfer is one streamed
// response of roughly 50MB compressed to a fraction of that; 10 minutes is far
// more than it needs and exists to stop a wedged connection from pinning the
// refresh goroutine forever.
const refreshTimeout = 10 * time.Minute

// refreshScheduler runs the daily bulk fetch at a configured time.
type refreshScheduler struct {
	refreshHour   int
	refreshMinute int
	fetcher       *fetcher
	store         *dataStore
	logger        *zap.Logger

	// ctx is cancelled by Stop, which aborts an in-flight download as well as
	// the timer wait. Without it a Caddy config reload during the initial fetch
	// would leave the old module's download running against the old store.
	ctx    context.Context
	cancel context.CancelFunc

	// wg tracks refreshes so Stop can wait for them to unwind.
	wg sync.WaitGroup
	// refreshing serializes refreshes: the startup fetch and the daily timer
	// can otherwise overlap and download the export twice concurrently.
	refreshing sync.Mutex
}

func newScheduler(refreshTime string, f *fetcher, s *dataStore, logger *zap.Logger) (*refreshScheduler, error) {
	hour, minute, err := parseTime(refreshTime)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &refreshScheduler{
		refreshHour:   hour,
		refreshMinute: minute,
		fetcher:       f,
		store:         s,
		logger:        logger,
		ctx:           ctx,
		cancel:        cancel,
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
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		rs.run()
	}()
}

// RefreshNow runs a refresh in the background, returning immediately.
//
// Used for the initial population so Provision does not block. The module
// previously fetched synchronously there, which held up Caddy's config load for
// the whole download — minutes on the old paging fetcher — and left the process
// still binding ports long after a reload was issued.
func (rs *refreshScheduler) RefreshNow() {
	rs.wg.Add(1)
	go func() {
		defer rs.wg.Done()
		rs.doRefresh()
	}()
}

// Stop signals the scheduler to shut down and waits for any in-flight refresh
// to return. Bounded by the cancellation of rs.ctx, which aborts the download.
func (rs *refreshScheduler) Stop() {
	rs.cancel()
	rs.wg.Wait()
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
		case <-rs.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (rs *refreshScheduler) doRefresh() {
	rs.refreshing.Lock()
	defer rs.refreshing.Unlock()

	if rs.ctx.Err() != nil {
		return
	}

	rs.logger.Info("starting refresh")

	ctx, cancel := context.WithTimeout(rs.ctx, refreshTimeout)
	defer cancel()

	result, err := rs.fetcher.FetchAll(ctx, rs.store.ETag())
	if err != nil {
		rs.logger.Error("refresh failed, keeping existing data",
			zap.Error(err),
			zap.Int("current_entries", rs.store.EntryCount()),
		)
		return
	}

	if result.NotModified {
		// Nothing to rebuild or rewrite: the trie already reflects this
		// snapshot. The on-disk file is left alone so its mtime keeps meaning
		// "when the data last changed".
		return
	}

	loaded, skipped := rs.store.Replace(result.Entries, result.ETag)
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
