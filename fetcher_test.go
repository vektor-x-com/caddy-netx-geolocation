package caddy_netx_geolocation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// exportBody renders an NDJSON export body: the given range lines followed by
// the terminator the real endpoint appends.
func exportBody(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, `{"eof":true,"n":%d}`+"\n", len(lines))
	return b.String()
}

// exportServer serves body at /api/bulk/geo and records the last request seen,
// so tests can assert on the query string and conditional headers.
func exportServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func testFetcher(url string, countries, registries []string) *fetcher {
	logger, _ := zap.NewDevelopment()
	return newFetcher(url, countries, registries, logger)
}

func TestFetcherFetchAll(t *testing.T) {
	body := exportBody(
		`{"s":"10.0.0.0","e":"10.255.255.255","c":"US","r":"arin","oid":"ORGA","on":"Org A"}`,
		`{"s":"192.168.0.0","e":"192.168.255.255","c":"DE","r":"ripencc","oid":"ORGB","on":"Org B"}`,
		`{"s":"172.16.0.0","e":"172.31.255.255","c":"DE","r":"ripencc","oid":"ORGB","on":"Org B"}`,
		`{"s":"2001:db8::","e":"2001:db8:ffff:ffff:ffff:ffff:ffff:ffff","c":"JP","r":"apnic","oid":"ORGC","on":"Org C"}`,
	)

	srv, last := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("ETag", `W/"geo-123-abc"`)
		fmt.Fprint(w, body)
	})

	res, err := testFetcher(srv.URL, nil, nil).FetchAll(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchAll failed: %v", err)
	}

	if last.URL.Path != bulkGeoPath {
		t.Errorf("expected path %s, got %s", bulkGeoPath, last.URL.Path)
	}
	if res.NotModified {
		t.Error("expected a full response, got NotModified")
	}
	if res.ETag != `W/"geo-123-abc"` {
		t.Errorf("expected the response ETag to be captured, got %q", res.ETag)
	}

	// Each of these ranges is CIDR-aligned, so it decomposes to exactly one
	// prefix. Misaligned ranges are covered by TestFetcherSplitsUnalignedRange.
	want := map[string]string{
		"10.0.0.0/8":     "US",
		"192.168.0.0/16": "DE",
		"172.16.0.0/12":  "DE",
		"2001:db8::/32":  "JP",
	}
	if len(res.Entries) != len(want) {
		t.Fatalf("expected %d entries, got %d: %+v", len(want), len(res.Entries), res.Entries)
	}
	for _, e := range res.Entries {
		country, ok := want[e.PrefixStr]
		if !ok {
			t.Errorf("unexpected prefix %s", e.PrefixStr)
			continue
		}
		if e.Record.Country != country {
			t.Errorf("%s: expected country %s, got %s", e.PrefixStr, country, e.Record.Country)
		}
	}
	if res.Entries[0].Record.OrgName != "Org A" || res.Entries[0].Record.OrgID != "ORGA" {
		t.Errorf("organization not carried through: %+v", res.Entries[0].Record)
	}
}

// A start/end pair that is not a single CIDR must expand to the exact set of
// prefixes covering it. The previous fetcher appended "/32" to the start
// address, claiming one host out of the whole allocation.
func TestFetcherSplitsUnalignedRange(t *testing.T) {
	srv, _ := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, exportBody(
			`{"s":"192.0.2.4","e":"192.0.2.11","c":"US","r":"arin","oid":"ORGA","on":"Org A"}`,
		))
	})

	res, err := testFetcher(srv.URL, nil, nil).FetchAll(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchAll failed: %v", err)
	}

	got := []string{}
	for _, e := range res.Entries {
		got = append(got, e.PrefixStr)
	}
	want := []string{"192.0.2.4/30", "192.0.2.8/30"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestFetcherSendsFilters(t *testing.T) {
	srv, last := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, exportBody())
	})

	_, err := testFetcher(srv.URL, []string{"DE", "FR"}, []string{"ripencc"}).
		FetchAll(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchAll failed: %v", err)
	}

	if got := last.URL.Query().Get("country"); got != "DE,FR" {
		t.Errorf("expected country=DE,FR, got %q", got)
	}
	if got := last.URL.Query().Get("registry"); got != "ripencc" {
		t.Errorf("expected registry=ripencc, got %q", got)
	}
}

func TestFetcherConditionalRequest(t *testing.T) {
	srv, last := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `W/"geo-123-abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		fmt.Fprint(w, exportBody(`{"s":"10.0.0.0","e":"10.255.255.255","c":"US","r":"arin","oid":"A","on":"A"}`))
	})

	res, err := testFetcher(srv.URL, nil, nil).FetchAll(context.Background(), `W/"geo-123-abc"`)
	if err != nil {
		t.Fatalf("FetchAll failed: %v", err)
	}

	if last.Header.Get("If-None-Match") != `W/"geo-123-abc"` {
		t.Error("expected If-None-Match to be sent")
	}
	if !res.NotModified {
		t.Fatal("expected NotModified for a matching ETag")
	}
	if len(res.Entries) != 0 {
		t.Errorf("expected no entries on 304, got %d", len(res.Entries))
	}
	if res.ETag != `W/"geo-123-abc"` {
		t.Errorf("expected the previous ETag to be preserved, got %q", res.ETag)
	}
}

// A body cut short mid-stream decodes as a run of valid JSON lines, so only the
// missing terminator distinguishes it from a smaller dataset. Without this
// check a partial download would silently replace a complete one.
func TestFetcherRejectsTruncatedExport(t *testing.T) {
	srv, _ := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"s":"10.0.0.0","e":"10.255.255.255","c":"US","r":"arin","oid":"A","on":"A"}`+"\n")
	})

	_, err := testFetcher(srv.URL, nil, nil).FetchAll(context.Background(), "")
	if !errors.Is(err, errIncompleteExport) {
		t.Fatalf("expected errIncompleteExport, got %v", err)
	}
}

func TestFetcherRejectsCountMismatch(t *testing.T) {
	srv, _ := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"s":"10.0.0.0","e":"10.255.255.255","c":"US","r":"arin","oid":"A","on":"A"}`+"\n"+
			`{"eof":true,"n":99}`+"\n")
	})

	_, err := testFetcher(srv.URL, nil, nil).FetchAll(context.Background(), "")
	if !errors.Is(err, errIncompleteExport) {
		t.Fatalf("expected errIncompleteExport, got %v", err)
	}
}

// One unparseable row must not cost the rest of the download.
func TestFetcherSkipsBadRanges(t *testing.T) {
	srv, _ := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, exportBody(
			`{"s":"not-an-ip","e":"10.255.255.255","c":"US","r":"arin","oid":"A","on":"A"}`,
			`{"s":"10.0.0.0","e":"10.255.255.255","c":"US","r":"arin","oid":"A","on":"A"}`,
		))
	})

	res, err := testFetcher(srv.URL, nil, nil).FetchAll(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchAll failed: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].PrefixStr != "10.0.0.0/8" {
		t.Fatalf("expected only the good range, got %+v", res.Entries)
	}
}

func TestFetcherAPIDown(t *testing.T) {
	srv, _ := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	})

	if _, err := testFetcher(srv.URL, nil, nil).FetchAll(context.Background(), ""); err == nil {
		t.Fatal("expected error when API is down")
	}
}

func TestFetcherContextCancellation(t *testing.T) {
	srv, _ := exportServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, exportBody())
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	if _, err := testFetcher(srv.URL, nil, nil).FetchAll(ctx, ""); err == nil {
		t.Fatal("expected error on cancelled context")
	}
}
