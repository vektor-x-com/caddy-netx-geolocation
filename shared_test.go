package caddy_netx_geolocation

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func poolTestServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("ETag", `W/"pool"`)
		fmt.Fprint(w, exportBody(
			`{"s":"10.0.0.0","e":"10.255.255.255","c":"US","r":"arin","oid":"A","on":"Org A"}`,
		))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Every site block in a Caddyfile provisions its own handler. They must share
// one dataset: at 1.5M ranges a trie is ~348MB, so 27 sites each building their
// own needed ~9.4GB and were OOM-killed partway through loading.
func TestInstancesShareOneDataset(t *testing.T) {
	hits := 0
	srv := poolTestServer(t, &hits)
	dir := t.TempDir()
	file := filepath.Join(dir, "shared.gob")
	logger := zap.NewNop()

	const sites = 27
	key := newDatasetKey(srv.URL, file, "03:00", nil, nil)

	var handlers []*NetxGeolocation
	for i := 0; i < sites; i++ {
		f := newFetcher(srv.URL, nil, nil, logger)
		ds, err := loadDataset(key, f, logger)
		if err != nil {
			t.Fatalf("site %d: %v", i, err)
		}
		handlers = append(handlers, &NetxGeolocation{shared: ds, store: ds.store, key: key})
	}

	// One dataset object, not 27.
	first := handlers[0].store
	for i, h := range handlers {
		if h.store != first {
			t.Fatalf("site %d has its own store; the dataset is not shared", i)
		}
	}

	if refs, ok := geoPool.References(key); !ok || refs != sites {
		t.Errorf("expected %d references, got %d (present=%v)", sites, refs, ok)
	}

	// The initial fetch is asynchronous — Provision deliberately does not block
	// on it — so wait for the data before asserting on a lookup.
	waitFor(t, func() bool { return first.EntryCount() > 0 })

	// Releasing all but one must keep the dataset alive and usable.
	for _, h := range handlers[1:] {
		if err := h.Cleanup(); err != nil {
			t.Fatal(err)
		}
	}
	if refs, ok := geoPool.References(key); !ok || refs != 1 {
		t.Errorf("expected 1 reference after partial release, got %d (present=%v)", refs, ok)
	}
	if rec := handlers[0].store.Lookup(netip.MustParseAddr("10.1.2.3")); rec == nil || rec.Country != "US" {
		t.Errorf("dataset unusable after other sites released it: %+v", rec)
	}

	// The last release tears it down.
	if err := handlers[0].Cleanup(); err != nil {
		t.Fatal(err)
	}
	if refs, ok := geoPool.References(key); ok {
		t.Errorf("dataset still pooled after the last release (refs=%d)", refs)
	}
}

// Sharing must not cost a download per site either — that was two 16MB pulls a
// day for two sites, and 27 for a full config.
func TestSharedDatasetFetchesOnce(t *testing.T) {
	hits := 0
	srv := poolTestServer(t, &hits)
	file := filepath.Join(t.TempDir(), "once.gob")
	logger := zap.NewNop()
	key := newDatasetKey(srv.URL, file, "03:00", nil, nil)

	var handlers []*NetxGeolocation
	for i := 0; i < 5; i++ {
		ds, err := loadDataset(key, newFetcher(srv.URL, nil, nil, logger), logger)
		if err != nil {
			t.Fatal(err)
		}
		handlers = append(handlers, &NetxGeolocation{shared: ds, store: ds.store, key: key})
	}
	t.Cleanup(func() {
		for _, h := range handlers {
			h.Cleanup()
		}
	})

	// The initial fetch is asynchronous; wait for it rather than racing it.
	waitFor(t, func() bool { return handlers[0].store.EntryCount() > 0 })

	if hits != 1 {
		t.Errorf("expected exactly 1 download for 5 sites, got %d", hits)
	}
}

// Different download filters are different datasets and must not be conflated.
func TestDifferentFiltersDoNotShare(t *testing.T) {
	hits := 0
	srv := poolTestServer(t, &hits)
	dir := t.TempDir()
	logger := zap.NewNop()

	keyDE := newDatasetKey(srv.URL, filepath.Join(dir, "de.gob"), "03:00", []string{"DE"}, nil)
	keyAll := newDatasetKey(srv.URL, filepath.Join(dir, "all.gob"), "03:00", nil, nil)

	dsDE, err := loadDataset(keyDE, newFetcher(srv.URL, []string{"DE"}, nil, logger), logger)
	if err != nil {
		t.Fatal(err)
	}
	dsAll, err := loadDataset(keyAll, newFetcher(srv.URL, nil, nil, logger), logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseDataset(keyDE); releaseDataset(keyAll) })

	if dsDE.store == dsAll.store {
		t.Fatal("a filtered download shares a dataset with an unfiltered one")
	}
}
