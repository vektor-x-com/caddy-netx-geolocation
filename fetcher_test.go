package caddy_netx_geolocation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestFetcherFetchAll(t *testing.T) {
	// Mock API with 3 orgs (2 pages of limit=2)
	orgs := []orgRecord{
		{
			OrgName: "Org A",
			OrgID:   "ORGA",
			ASNs:    []asnInfo{{ASN: 100, ASNName: "ASN-A", Registry: "arin", Country: "US"}},
			IPRanges: ipRanges{
				IPv4: []ipRange{{StartIP: "10.0.0.0/8", Registry: "arin", Country: "US"}},
			},
		},
		{
			OrgName: "Org B",
			OrgID:   "ORGB",
			ASNs:    []asnInfo{{ASN: 200, ASNName: "ASN-B", Registry: "ripencc", Country: "DE"}},
			IPRanges: ipRanges{
				IPv4: []ipRange{
					{StartIP: "192.168.0.0/16", Registry: "ripencc", Country: "DE"},
					{StartIP: "172.16.0.0/12", Registry: "ripencc", Country: "DE"},
				},
			},
		},
		{
			OrgName: "Org C",
			OrgID:   "ORGC",
			ASNs:    []asnInfo{{ASN: 300, ASNName: "ASN-C", Registry: "apnic", Country: "JP"}},
			IPRanges: ipRanges{
				IPv6: []ipRange{{StartIP: "2001:db8::/32", Registry: "apnic", Country: "JP"}},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 2
		offset := 0
		if q.Get("limit") != "" {
			fmt.Sscanf(q.Get("limit"), "%d", &limit)
		}
		if q.Get("offset") != "" {
			fmt.Sscanf(q.Get("offset"), "%d", &offset)
		}

		end := offset + limit
		if end > len(orgs) {
			end = len(orgs)
		}
		var page []orgRecord
		if offset < len(orgs) {
			page = orgs[offset:end]
		}

		json.NewEncoder(w).Encode(apiResponse{Data: page, Total: len(orgs)})
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	f := newFetcher(server.URL, logger)

	entries, err := f.FetchAll(context.Background())
	if err != nil {
		t.Fatalf("FetchAll failed: %v", err)
	}

	// Org A: 1 IPv4, Org B: 2 IPv4, Org C: 1 IPv6 = 4 total entries
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}

	// Verify first entry
	if entries[0].PrefixStr != "10.0.0.0/8" {
		t.Errorf("expected 10.0.0.0/8, got %s", entries[0].PrefixStr)
	}
	if entries[0].Record.Country != "US" {
		t.Errorf("expected US, got %s", entries[0].Record.Country)
	}
	if entries[0].Record.OrgName != "Org A" {
		t.Errorf("expected Org A, got %s", entries[0].Record.OrgName)
	}
}

func TestFetcherAPIDown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer server.Close()

	logger, _ := zap.NewDevelopment()
	f := newFetcher(server.URL, logger)

	_, err := f.FetchAll(context.Background())
	if err == nil {
		t.Fatal("expected error when API is down")
	}
}

func TestFetcherContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(apiResponse{Data: []orgRecord{}, Total: 0})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	logger, _ := zap.NewDevelopment()
	f := newFetcher(server.URL, logger)

	_, err := f.FetchAll(ctx)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}
