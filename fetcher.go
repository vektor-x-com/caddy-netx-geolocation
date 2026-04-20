package caddy_netx_geolocation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	fetchPageSize    = 1000
	fetchTimeout     = 30 * time.Second
	logEveryNPages   = 10
	fetchPageDelay   = 1100 * time.Millisecond // stay under 60 req/min API rate limit
)

// fetcher handles paginated bulk downloads from the API.
type fetcher struct {
	apiURL string
	logger *zap.Logger
	client *http.Client
}

func newFetcher(apiURL string, logger *zap.Logger) *fetcher {
	return &fetcher{
		apiURL: apiURL,
		logger: logger,
		client: &http.Client{Timeout: fetchTimeout},
	}
}

// FetchAll paginates through the API and returns all IP→geo entries.
func (f *fetcher) FetchAll(ctx context.Context) ([]cidrEntry, error) {
	var allEntries []cidrEntry
	offset := 0
	total := -1
	page := 0

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		url := fmt.Sprintf("%s/api/data?limit=%d&offset=%d", f.apiURL, fetchPageSize, offset)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}

		resp, err := f.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("request at offset %d: %w", offset, err)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20)) // 50MB limit per page
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("API returned %d at offset %d: %s", resp.StatusCode, offset, string(body[:min(len(body), 200)]))
		}

		if err != nil {
			return nil, fmt.Errorf("reading body at offset %d: %w", offset, err)
		}

		var apiResp apiResponse
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return nil, fmt.Errorf("parsing response at offset %d: %w", offset, err)
		}

		if total < 0 {
			total = apiResp.Total
			f.logger.Info("starting bulk fetch", zap.Int("total_orgs", total))
		}

		if len(apiResp.Data) == 0 {
			break
		}

		entries := f.extractEntries(apiResp.Data)
		allEntries = append(allEntries, entries...)

		page++
		if page%logEveryNPages == 0 {
			f.logger.Info("fetch progress",
				zap.Int("page", page),
				zap.Int("offset", offset),
				zap.Int("entries_so_far", len(allEntries)),
			)
		}

		offset += fetchPageSize
		if offset >= total {
			break
		}

		// Rate limit: stay under 60 req/min
		select {
		case <-time.After(fetchPageDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	f.logger.Info("bulk fetch complete",
		zap.Int("total_entries", len(allEntries)),
		zap.Int("pages_fetched", page),
	)

	return allEntries, nil
}

// extractEntries converts org records to flat CIDR entries.
func (f *fetcher) extractEntries(orgs []orgRecord) []cidrEntry {
	var entries []cidrEntry

	for i := range orgs {
		org := &orgs[i]

		// Process IPv4 ranges
		for _, r := range org.IPRanges.IPv4 {
			entry := f.rangeToEntry(r, org)
			if entry != nil {
				entries = append(entries, *entry)
			}
		}

		// Process IPv6 ranges
		for _, r := range org.IPRanges.IPv6 {
			entry := f.rangeToEntry(r, org)
			if entry != nil {
				entries = append(entries, *entry)
			}
		}
	}

	return entries
}

func (f *fetcher) rangeToEntry(r ipRange, org *orgRecord) *cidrEntry {
	cidr := r.StartIP
	if cidr == "" {
		return nil
	}

	// Ensure it's a valid CIDR (some may be plain IPs without mask)
	if !strings.Contains(cidr, "/") {
		cidr += "/32"
	}

	return &cidrEntry{
		PrefixStr: cidr,
		Record: geoRecord{
			Country:  strings.ToUpper(r.Country),
			Registry: r.Registry,
			OrgName:  org.OrgName,
			OrgID:    org.OrgID,
		},
	}
}

// --- API Response Types ---

type apiResponse struct {
	Data  []orgRecord `json:"data"`
	Total int         `json:"total"`
}

type orgRecord struct {
	OrgName  string    `json:"organization_name"`
	OrgID    string    `json:"organization_id"`
	ASNs     []asnInfo `json:"asns"`
	IPRanges ipRanges  `json:"ip_ranges"`
}

type asnInfo struct {
	ASN      int    `json:"asn"`
	ASNName  string `json:"asn_name"`
	Registry string `json:"registry"`
	Country  string `json:"country"`
}

type ipRanges struct {
	IPv4 []ipRange `json:"ipv4"`
	IPv6 []ipRange `json:"ipv6"`
}

type ipRange struct {
	StartIP  string `json:"start_ip"`
	EndIP    string `json:"end_ip"`
	Registry string `json:"registry"`
	Country  string `json:"country"`
}
