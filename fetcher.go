package caddy_netx_geolocation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	// bulkGeoPath streams the whole dataset as newline-delimited JSON, one line
	// per allocated IP range, terminated by a summary line.
	//
	// This replaced a 172-page crawl of /api/data that could not work: that
	// endpoint omits ip_ranges from list responses, expanding them only for a
	// single-organization query, so the crawl downloaded ~230MB of contact data
	// and produced zero geolocation entries. It was also slow for an unrelated
	// reason — each page runs a correlated domain-count subquery per row, about
	// 7s per uncached page, or ~20 minutes for a full pass.
	bulkGeoPath = "/api/bulk/geo"

	// responseHeaderTimeout bounds the wait for response headers only, not the
	// body transfer. The export is one long streamed response, so a
	// whole-request deadline (http.Client.Timeout) would put a ceiling on the
	// download size rather than detect a stall. Total duration is bounded by
	// the caller's context instead.
	responseHeaderTimeout = 60 * time.Second

	// maxExportBytes caps the decompressed body. The export is ~50MB today;
	// this is headroom for growth while still refusing to fill memory if the
	// endpoint ever returns something unbounded.
	maxExportBytes = 512 << 20

	logEveryNEntries = 100_000
)

// errIncompleteExport reports a body that ended without its terminator line, or
// with one whose count disagrees with what was decoded. See exportLine.
var errIncompleteExport = errors.New("incomplete export")

// fetcher downloads the bulk geolocation export.
type fetcher struct {
	apiURL string
	// countries/registries narrow the download server-side. Empty means the
	// full dataset.
	countries  []string
	registries []string
	logger     *zap.Logger
	client     *http.Client
}

func newFetcher(apiURL string, countries, registries []string, logger *zap.Logger) *fetcher {
	return &fetcher{
		apiURL:     apiURL,
		countries:  countries,
		registries: registries,
		logger:     logger,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ResponseHeaderTimeout: responseHeaderTimeout,
				TLSHandshakeTimeout:   15 * time.Second,
				// DisableCompression stays false so the transport advertises
				// gzip and decompresses transparently. The export compresses to
				// roughly a sixth of its size, and Caddy is configured to encode
				// application/x-ndjson.
			},
		},
	}
}

// fetchResult carries the outcome of one export download.
type fetchResult struct {
	Entries []cidrEntry
	// ETag identifies the server-side snapshot. Pass it back on the next fetch
	// to get NotModified instead of a re-download.
	ETag string
	// NotModified means the server matched the caller's ETag and sent no body.
	// Entries is nil in that case and the existing dataset should be kept.
	NotModified bool
}

// exportLine is one decoded NDJSON record.
//
// The stream mixes two shapes: range records, and a single terminator at the
// end carrying the number of range records that preceded it. Both decode into
// this struct — a range record has no "eof" field, a terminator has no "s".
//
// The terminator is what makes truncation detectable. Every line of a cut-short
// response is individually valid JSON, so without it a partial download decodes
// cleanly as a smaller dataset and would silently replace a complete one.
type exportLine struct {
	StartIP  string `json:"s"`
	EndIP    string `json:"e"`
	Country  string `json:"c"`
	Registry string `json:"r"`
	OrgID    string `json:"oid"`
	OrgName  string `json:"on"`

	EOF   bool `json:"eof"`
	Count int  `json:"n"`
}

// FetchAll downloads the export and converts it to trie entries.
//
// prevETag, when non-empty, is sent as If-None-Match; a matching snapshot comes
// back as 304 with NotModified set and no entries. The daily refresh therefore
// costs one conditional request on any day the upstream data did not change.
func (f *fetcher) FetchAll(ctx context.Context, prevETag string) (*fetchResult, error) {
	endpoint := strings.TrimRight(f.apiURL, "/") + bulkGeoPath
	if q := f.query(); q != "" {
		endpoint += "?" + q
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/x-ndjson")
	if prevETag != "" {
		req.Header.Set("If-None-Match", prevETag)
	}

	start := time.Now()
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		f.logger.Info("export unchanged since last fetch", zap.String("etag", prevETag))
		return &fetchResult{ETag: prevETag, NotModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	entries, count, err := f.decode(resp.Body)
	if err != nil {
		return nil, err
	}

	f.logger.Info("bulk export downloaded",
		zap.Int("ranges", count),
		zap.Int("prefixes", len(entries)),
		zap.Duration("took", time.Since(start)),
		zap.String("etag", resp.Header.Get("ETag")),
	)

	return &fetchResult{Entries: entries, ETag: resp.Header.Get("ETag")}, nil
}

// query renders the optional server-side filters.
func (f *fetcher) query() string {
	v := url.Values{}
	if len(f.countries) > 0 {
		v.Set("country", strings.Join(f.countries, ","))
	}
	if len(f.registries) > 0 {
		v.Set("registry", strings.Join(f.registries, ","))
	}
	return v.Encode()
}

// decode streams the NDJSON body into trie entries, returning the entries and
// the number of range records read.
//
// Malformed ranges are counted and skipped rather than failing the download: a
// single bad row upstream should not cost the whole dataset. A truncated body
// is a different matter and does fail — see errIncompleteExport.
func (f *fetcher) decode(body io.Reader) ([]cidrEntry, int, error) {
	dec := json.NewDecoder(io.LimitReader(body, maxExportBytes))

	var entries []cidrEntry
	count := 0
	skipped := 0
	sawTerminator := false

	for {
		var line exportLine
		if err := dec.Decode(&line); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, 0, fmt.Errorf("decoding export at record %d: %w", count, err)
		}

		if line.EOF {
			if line.Count != count {
				return nil, 0, fmt.Errorf("%w: server reported %d ranges, decoded %d",
					errIncompleteExport, line.Count, count)
			}
			sawTerminator = true
			break
		}

		count++
		prefixes, err := f.lineToPrefixes(&line)
		if err != nil {
			skipped++
			// Debug, not Warn: a handful of unparseable rows is normal registry
			// noise, and at this volume warning on each would flood the log.
			f.logger.Debug("skipping unusable range",
				zap.String("start", line.StartIP),
				zap.String("end", line.EndIP),
				zap.Error(err),
			)
			continue
		}

		rec := geoRecord{
			Country:  strings.ToUpper(line.Country),
			Registry: line.Registry,
			OrgName:  line.OrgName,
			OrgID:    line.OrgID,
		}
		for _, p := range prefixes {
			entries = append(entries, cidrEntry{PrefixStr: p.String(), Record: rec})
		}

		if count%logEveryNEntries == 0 {
			f.logger.Info("export progress",
				zap.Int("ranges", count),
				zap.Int("prefixes", len(entries)),
			)
		}
	}

	if !sawTerminator {
		return nil, 0, fmt.Errorf("%w: body ended after %d ranges without a terminator record",
			errIncompleteExport, count)
	}

	if skipped > 0 {
		f.logger.Warn("some ranges could not be parsed",
			zap.Int("skipped", skipped),
			zap.Int("total", count),
		)
	}

	return entries, count, nil
}

// lineToPrefixes converts one exported range into CIDR prefixes.
func (f *fetcher) lineToPrefixes(line *exportLine) ([]netip.Prefix, error) {
	start, err := netip.ParseAddr(line.StartIP)
	if err != nil {
		return nil, fmt.Errorf("start_ip: %w", err)
	}
	end, err := netip.ParseAddr(line.EndIP)
	if err != nil {
		return nil, fmt.Errorf("end_ip: %w", err)
	}
	return rangeToPrefixes(start, end)
}
