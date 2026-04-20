package caddy_netx_geolocation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestParseTime(t *testing.T) {
	tests := []struct {
		input  string
		hour   int
		minute int
		err    bool
	}{
		{"03:00", 3, 0, false},
		{"23:59", 23, 59, false},
		{"00:00", 0, 0, false},
		{"12:30", 12, 30, false},
		{"24:00", 0, 0, true},
		{"-1:00", 0, 0, true},
		{"12:60", 0, 0, true},
		{"abc", 0, 0, true},
		{"", 0, 0, true},
		{"12", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			h, m, err := parseTime(tt.input)
			if tt.err {
				if err == nil {
					t.Errorf("expected error for %q", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for %q: %v", tt.input, err)
				}
				if h != tt.hour || m != tt.minute {
					t.Errorf("expected %d:%d, got %d:%d", tt.hour, tt.minute, h, m)
				}
			}
		})
	}
}

func TestSchedulerDurationUntilNext(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	store := newDataStore("/dev/null")
	f := newFetcher("http://localhost", logger)

	sched, err := newScheduler("03:00", f, store, logger)
	if err != nil {
		t.Fatalf("newScheduler failed: %v", err)
	}

	dur := sched.durationUntilNext()
	if dur <= 0 || dur > 24*time.Hour {
		t.Errorf("duration should be between 0 and 24h, got %v", dur)
	}
}

func TestSchedulerStopBeforeFire(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	store := newDataStore("/dev/null")
	f := newFetcher("http://localhost", logger)

	// Set refresh far in the future so it won't fire
	sched, err := newScheduler("03:00", f, store, logger)
	if err != nil {
		t.Fatal(err)
	}

	sched.Start()
	time.Sleep(10 * time.Millisecond)
	sched.Stop()
	// Should not hang or panic
}

func TestSchedulerRefreshOnAPIFailure(t *testing.T) {
	var requestCount atomic.Int32

	// API that always fails
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service unavailable"))
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	store := newDataStore(t.TempDir() + "/test.gob")
	f := newFetcher(server.URL, logger)

	// Pre-load some data
	store.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	})

	sched := &refreshScheduler{
		refreshHour:   0,
		refreshMinute: 0,
		fetcher:       f,
		store:         store,
		logger:        logger,
		done:          make(chan struct{}),
	}

	// Manually trigger refresh
	sched.doRefresh()

	// API was called
	if requestCount.Load() == 0 {
		t.Fatal("expected API to be called")
	}

	// Existing data preserved after failure
	rec := store.Lookup(netip.MustParseAddr("10.1.2.3"))
	if rec == nil || rec.Country != "US" {
		t.Errorf("expected existing data preserved, got %+v", rec)
	}
}

func TestSchedulerRefreshOnAPITimeout(t *testing.T) {
	// API that hangs forever
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			return
		}
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	store := newDataStore(t.TempDir() + "/test.gob")
	f := &fetcher{
		apiURL: server.URL,
		logger: logger,
		client: &http.Client{Timeout: 100 * time.Millisecond}, // very short timeout
	}

	store.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	})

	sched := &refreshScheduler{
		refreshHour:   0,
		refreshMinute: 0,
		fetcher:       f,
		store:         store,
		logger:        logger,
		done:          make(chan struct{}),
	}

	sched.doRefresh()

	// Data still intact
	rec := store.Lookup(netip.MustParseAddr("10.1.2.3"))
	if rec == nil || rec.Country != "US" {
		t.Errorf("expected data preserved after timeout, got %+v", rec)
	}
}

func TestSchedulerRefreshOnPartialData(t *testing.T) {
	var callCount atomic.Int32

	// API that returns data on first page but fails on second
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count == 1 {
			json.NewEncoder(w).Encode(apiResponse{
				Data: []orgRecord{
					{
						OrgName:  "Test",
						OrgID:    "TST",
						ASNs:     []asnInfo{{ASN: 1, ASNName: "T", Registry: "arin", Country: "US"}},
						IPRanges: ipRanges{IPv4: []ipRange{{StartIP: "10.0.0.0/8", Country: "US", Registry: "arin"}}},
					},
				},
				Total: 2000, // claims there are more
			})
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	store := newDataStore(t.TempDir() + "/test.gob")
	f := newFetcher(server.URL, logger)

	// Pre-load different data
	store.Replace([]cidrEntry{
		{PrefixStr: "192.168.0.0/16", Record: geoRecord{Country: "DE"}},
	})

	sched := &refreshScheduler{
		refreshHour:   0,
		refreshMinute: 0,
		fetcher:       f,
		store:         store,
		logger:        logger,
		done:          make(chan struct{}),
	}

	sched.doRefresh()

	// Old data should be preserved since fetch failed mid-way
	rec := store.Lookup(netip.MustParseAddr("192.168.1.1"))
	if rec == nil || rec.Country != "DE" {
		t.Errorf("expected old data preserved after partial failure, got %+v", rec)
	}
}

func TestSchedulerRefreshSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(apiResponse{
			Data: []orgRecord{
				{
					OrgName:  "NewOrg",
					OrgID:    "NEW",
					ASNs:     []asnInfo{{ASN: 999, ASNName: "NEWASN", Registry: "ripencc", Country: "FR"}},
					IPRanges: ipRanges{IPv4: []ipRange{{StartIP: "172.16.0.0/12", Country: "FR", Registry: "ripencc"}}},
				},
			},
			Total: 1,
		})
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	dir := t.TempDir()
	store := newDataStore(dir + "/test.gob")
	f := newFetcher(server.URL, logger)

	// Old data
	store.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	})

	sched := &refreshScheduler{
		refreshHour:   0,
		refreshMinute: 0,
		fetcher:       f,
		store:         store,
		logger:        logger,
		done:          make(chan struct{}),
	}

	sched.doRefresh()

	// New data should be active
	rec := store.Lookup(netip.MustParseAddr("172.16.5.5"))
	if rec == nil || rec.Country != "FR" {
		t.Errorf("expected new data after successful refresh, got %+v", rec)
	}

	// Old data gone
	rec = store.Lookup(netip.MustParseAddr("10.1.2.3"))
	if rec != nil {
		t.Errorf("expected old data removed after refresh, got %+v", rec)
	}
}

func TestFetcherContextCancelMidPagination(t *testing.T) {
	var callCount atomic.Int32
	cancelAfter := int32(3) // cancel after 3rd request

	ctx, cancel := context.WithCancel(context.Background())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count >= cancelAfter {
			cancel() // cancel context during this request
		}
		json.NewEncoder(w).Encode(apiResponse{
			Data: []orgRecord{
				{
					OrgName:  "Org",
					OrgID:    "O",
					IPRanges: ipRanges{IPv4: []ipRange{{StartIP: "10.0.0.0/8", Country: "US"}}},
				},
			},
			Total: 100000, // pretend there are many pages
		})
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	f := newFetcher(server.URL, logger)

	_, err := f.FetchAll(ctx)
	if err == nil {
		t.Fatal("expected error on context cancellation")
	}

	// Should have made some requests but not all 100 pages
	count := callCount.Load()
	if count == 0 {
		t.Fatal("expected at least 1 request")
	}
	if count >= 100 {
		t.Errorf("expected cancellation to stop pagination early, got %d requests", count)
	}
}
