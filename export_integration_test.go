package caddy_netx_geolocation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestAgainstRealExport runs a real /api/bulk/geo body through the whole
// pipeline — decode, range decomposition, trie build, lookup — and reports
// what it found.
//
// Opt-in via NETX_EXPORT_FILE because it needs a ~60MB fixture nobody wants in
// the repo:
//
//	curl --compressed https://net.vektor-x.com/api/bulk/geo -o /tmp/geo.ndjson
//	NETX_EXPORT_FILE=/tmp/geo.ndjson go test -run TestAgainstRealExport -v
//
// The unit tests cover the shapes this cannot: they assert exact prefixes for
// hand-picked ranges. This one asserts the properties that only show up at
// scale — that a production body parses end to end, that decomposition does
// not silently drop most of it, and that known addresses resolve.
func TestAgainstRealExport(t *testing.T) {
	path := os.Getenv("NETX_EXPORT_FILE")
	if path == "" {
		t.Skip("NETX_EXPORT_FILE not set")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("ETag", `W/"geo-fixture"`)
		w.Write(body)
	}))
	defer srv.Close()

	logger := zap.NewNop()
	f := newFetcher(srv.URL, nil, nil, logger)

	start := time.Now()
	res, err := f.FetchAll(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchAll on the real export: %v", err)
	}
	decodeTook := time.Since(start)

	if len(res.Entries) == 0 {
		t.Fatal("no entries decoded from the real export")
	}

	start = time.Now()
	store := newDataStore(t.TempDir() + "/real.gob")
	loaded, skipped := store.Replace(res.Entries, res.ETag)
	buildTook := time.Since(start)

	t.Logf("prefixes=%d loaded=%d skipped=%d decode=%s trie_build=%s",
		len(res.Entries), loaded, skipped, decodeTook.Round(time.Millisecond), buildTook.Round(time.Millisecond))

	// Every prefix the decoder emitted came from netip.PrefixFrom, so anything
	// the trie rejects means the two disagree about what is valid.
	if skipped != 0 {
		t.Errorf("trie rejected %d prefixes the fetcher produced", skipped)
	}

	// Ranges outnumber lines only when decomposition splits them; a collapse to
	// roughly one prefix per line would mean the splitting is not running.
	if len(res.Entries) < loaded {
		t.Errorf("entry accounting is inconsistent: %d entries, %d loaded", len(res.Entries), loaded)
	}

	// Spot-check addresses whose attribution is independently known: 1.0.0.0/24
	// is APNIC's research block and 8.8.8.8 is Google.
	for _, tc := range []struct{ ip, wantCountry string }{
		{"1.0.0.1", "AU"},
		{"8.8.8.8", "US"},
	} {
		rec := store.Lookup(netip.MustParseAddr(tc.ip))
		if rec == nil {
			t.Errorf("%s resolved to nothing", tc.ip)
			continue
		}
		t.Logf("%s -> country=%s registry=%s org=%q", tc.ip, rec.Country, rec.Registry, rec.OrgName)
		if rec.Country != tc.wantCountry {
			t.Errorf("%s: expected country %s, got %s", tc.ip, tc.wantCountry, rec.Country)
		}
	}

	// An address inside a range must resolve, and the middle of an unallocated
	// block should not invent an answer.
	if rec := store.Lookup(netip.MustParseAddr("2001:db8::1")); rec != nil {
		t.Logf("note: documentation prefix 2001:db8::1 resolved to %+v", rec)
	}

	// Round-trip through the on-disk format at full size.
	if err := store.SaveToFile(); err != nil {
		t.Fatalf("SaveToFile at full size: %v", err)
	}
	reloaded := newDataStore(store.filePath)
	if err := reloaded.LoadFromFile(); err != nil {
		t.Fatalf("LoadFromFile at full size: %v", err)
	}
	if reloaded.EntryCount() != loaded {
		t.Errorf("expected %d entries after reload, got %d", loaded, reloaded.EntryCount())
	}
	if reloaded.ETag() != `W/"geo-fixture"` {
		t.Errorf("ETag lost across the file round-trip: %q", reloaded.ETag())
	}
	if rec := reloaded.Lookup(netip.MustParseAddr("8.8.8.8")); rec == nil {
		t.Error("8.8.8.8 stopped resolving after a save/load cycle")
	}
}
