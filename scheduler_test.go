package caddy_netx_geolocation

import (
	"context"
	"fmt"
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

// testScheduler builds a scheduler through newScheduler rather than as a struct
// literal: the struct carries an internal context that Stop cancels and
// doRefresh reads, so a hand-built one would refresh against a nil context.
func testScheduler(t *testing.T, url string, store *dataStore) *refreshScheduler {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	sched, err := newScheduler("03:00", newFetcher(url, nil, nil, logger), store, logger)
	if err != nil {
		t.Fatalf("newScheduler failed: %v", err)
	}
	t.Cleanup(sched.Stop)
	return sched
}

func TestSchedulerDurationUntilNext(t *testing.T) {
	sched := testScheduler(t, "http://localhost", newDataStore("/dev/null"))

	dur := sched.durationUntilNext()
	if dur <= 0 || dur > 24*time.Hour {
		t.Errorf("duration should be between 0 and 24h, got %v", dur)
	}
}

func TestSchedulerStopBeforeFire(t *testing.T) {
	sched := testScheduler(t, "http://localhost", newDataStore("/dev/null"))

	sched.Start()
	time.Sleep(10 * time.Millisecond)
	sched.Stop()
	// Should not hang or panic. Stop is idempotent enough for the t.Cleanup
	// call that follows: cancel is safe to repeat and Wait returns immediately.
}

func TestSchedulerRefreshOnAPIFailure(t *testing.T) {
	var requestCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("service unavailable"))
	}))
	defer server.Close()

	store := newDataStore(t.TempDir() + "/test.gob")
	store.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	}, `W/"old"`)

	testScheduler(t, server.URL, store).doRefresh()

	if requestCount.Load() == 0 {
		t.Fatal("expected API to be called")
	}

	rec := store.Lookup(netip.MustParseAddr("10.1.2.3"))
	if rec == nil || rec.Country != "US" {
		t.Errorf("expected existing data preserved, got %+v", rec)
	}
}

func TestSchedulerRefreshOnAPITimeout(t *testing.T) {
	// API that hangs without ever sending headers.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	store := newDataStore(t.TempDir() + "/test.gob")
	store.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	}, `W/"old"`)

	sched, err := newScheduler("03:00", newFetcher(server.URL, nil, nil, logger), store, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	// Bound the wait: refreshTimeout is 10 minutes, which is right in
	// production and far too long for a test. Cancelling the scheduler's own
	// context is what a Caddy shutdown does, and doRefresh derives from it.
	go func() {
		time.Sleep(200 * time.Millisecond)
		sched.cancel()
	}()
	sched.doRefresh()

	rec := store.Lookup(netip.MustParseAddr("10.1.2.3"))
	if rec == nil || rec.Country != "US" {
		t.Errorf("expected data preserved after timeout, got %+v", rec)
	}
}

// A stream that dies partway through must leave the previous dataset in place.
// This is the streaming counterpart of the old mid-pagination failure case: the
// lines that did arrive are valid JSON, so only the missing terminator marks
// the download as unusable.
func TestSchedulerRefreshOnTruncatedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"s":"10.0.0.0","e":"10.255.255.255","c":"US","r":"arin","oid":"A","on":"A"}`+"\n")
	}))
	defer server.Close()

	store := newDataStore(t.TempDir() + "/test.gob")
	store.Replace([]cidrEntry{
		{PrefixStr: "192.168.0.0/16", Record: geoRecord{Country: "DE"}},
	}, `W/"old"`)

	testScheduler(t, server.URL, store).doRefresh()

	rec := store.Lookup(netip.MustParseAddr("192.168.1.1"))
	if rec == nil || rec.Country != "DE" {
		t.Errorf("expected old data preserved after truncated stream, got %+v", rec)
	}
}

func TestSchedulerRefreshSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"geo-new"`)
		fmt.Fprint(w, exportBody(
			`{"s":"172.16.0.0","e":"172.31.255.255","c":"FR","r":"ripencc","oid":"NEW","on":"NewOrg"}`,
		))
	}))
	defer server.Close()

	store := newDataStore(t.TempDir() + "/test.gob")
	store.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	}, `W/"old"`)

	testScheduler(t, server.URL, store).doRefresh()

	rec := store.Lookup(netip.MustParseAddr("172.16.5.5"))
	if rec == nil || rec.Country != "FR" {
		t.Errorf("expected new data after successful refresh, got %+v", rec)
	}

	if rec := store.Lookup(netip.MustParseAddr("10.1.2.3")); rec != nil {
		t.Errorf("expected old data removed after refresh, got %+v", rec)
	}

	if store.ETag() != `W/"geo-new"` {
		t.Errorf("expected the new ETag to be stored, got %q", store.ETag())
	}
}

// A 304 must leave the dataset and its ETag untouched rather than clearing them.
func TestSchedulerRefreshNotModified(t *testing.T) {
	var served atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		if r.Header.Get("If-None-Match") == `W/"geo-current"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, exportBody())
	}))
	defer server.Close()

	store := newDataStore(t.TempDir() + "/test.gob")
	store.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	}, `W/"geo-current"`)

	testScheduler(t, server.URL, store).doRefresh()

	if served.Load() != 1 {
		t.Fatalf("expected exactly one request, got %d", served.Load())
	}
	rec := store.Lookup(netip.MustParseAddr("10.1.2.3"))
	if rec == nil || rec.Country != "US" {
		t.Errorf("expected data kept on 304, got %+v", rec)
	}
	if store.ETag() != `W/"geo-current"` {
		t.Errorf("expected ETag kept on 304, got %q", store.ETag())
	}
}

func TestFetcherContextCancelMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Sends one line, then blocks so the body is still open when the context is
	// cancelled.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"s":"10.0.0.0","e":"10.255.255.255","c":"US","r":"arin","oid":"A","on":"A"}`+"\n")
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	f := newFetcher(server.URL, nil, nil, logger)

	if _, err := f.FetchAll(ctx, ""); err == nil {
		t.Fatal("expected error on context cancellation")
	}
}
