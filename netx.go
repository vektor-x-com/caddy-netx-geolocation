package caddy_netx_geolocation

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(NetxGeolocation{})
}

// NetxGeolocation is a Caddy HTTP handler that provides IP geolocation
// using bulk-fetched data from the net.vektor-x.com API.
// Data is fetched daily and stored locally — no API calls on each request.
type NetxGeolocation struct {
	// API base URL (default: https://net.vektor-x.com)
	APIURL string `json:"api_url,omitempty"`

	// Directory for the local data file (default: caddy AppDataDir)
	DataDir string `json:"data_dir,omitempty"`

	// Daily refresh time in HH:MM local time (default: 03:00)
	//
	// The upstream dataset is rebuilt at 04:00 UTC, so a refresh scheduled
	// before that consistently downloads the previous day's snapshot. Pick a
	// time that lands after 04:00 UTC in the server's local zone.
	RefreshTime string `json:"refresh_time,omitempty"`

	// DownloadCountries and DownloadRegistries narrow what is downloaded, as
	// opposed to the Allow/Deny fields below which filter requests against a
	// dataset already in memory.
	//
	// These trade coverage for size: an address outside the downloaded slice
	// resolves to no record at all, so its placeholders read "-" and it is
	// treated exactly like an address the dataset has never heard of. Set them
	// only when that is the intended behaviour — for a deny-list deployment it
	// is usually not, since unknown addresses pass a deny-list.
	DownloadCountries  []string `json:"download_countries,omitempty"`
	DownloadRegistries []string `json:"download_registries,omitempty"`

	// Matcher fields
	AllowCountries  []string `json:"allow_countries,omitempty"`
	DenyCountries   []string `json:"deny_countries,omitempty"`
	AllowOrgs       []string `json:"allow_orgs,omitempty"`
	DenyOrgs        []string `json:"deny_orgs,omitempty"`
	AllowRegistries []string `json:"allow_registries,omitempty"`
	DenyRegistries  []string `json:"deny_registries,omitempty"`

	logger  *zap.Logger
	fetcher *fetcher

	// store is the shared dataset's trie, held directly so ServeHTTP does not
	// hop through shared on every request.
	store *dataStore
	// shared is reference-counted in geoPool and released by Cleanup; key
	// identifies it there.
	shared *sharedDataset
	key    datasetKey
}

// CaddyModule returns the Caddy module information.
func (NetxGeolocation) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.netx_geolocation",
		New: func() caddy.Module { return new(NetxGeolocation) },
	}
}

// Provision sets up the module.
func (n *NetxGeolocation) Provision(ctx caddy.Context) error {
	n.logger = ctx.Logger()

	// Defaults
	apiURL := "https://net.vektor-x.com"
	if n.APIURL != "" {
		apiURL = strings.TrimRight(n.APIURL, "/")
	}

	refreshTime := "03:00"
	if n.RefreshTime != "" {
		refreshTime = n.RefreshTime
	}

	dataDir := n.DataDir
	if dataDir == "" {
		dataDir = filepath.Join(caddy.AppDataDir(), "netx_geolocation")
	}
	dataFile := filepath.Join(dataDir, "netx_geo_data.gob")

	// Initialize components. The fetcher is per-instance and cheap; the
	// dataset behind it is not, so it is shared — see shared.go.
	n.fetcher = newFetcher(apiURL, n.DownloadCountries, n.DownloadRegistries, n.logger)

	n.key = newDatasetKey(apiURL, dataFile, refreshTime, n.DownloadCountries, n.DownloadRegistries)
	ds, err := loadDataset(n.key, n.fetcher, n.logger)
	if err != nil {
		return err
	}
	n.shared = ds
	n.store = ds.store

	return nil
}

// Validate ensures the module configuration is valid.
func (n *NetxGeolocation) Validate() error {
	for _, c := range n.AllowCountries {
		if len(c) != 2 {
			return fmt.Errorf("invalid country code %q: must be 2-letter ISO code", c)
		}
	}
	for _, c := range n.DenyCountries {
		if len(c) != 2 {
			return fmt.Errorf("invalid country code %q: must be 2-letter ISO code", c)
		}
	}
	// Validated here rather than left to the API: these go into the export
	// query string, and a typo would otherwise surface as a 400 inside a
	// background refresh — logged, but with the module already loaded and
	// serving an empty dataset.
	for _, c := range n.DownloadCountries {
		if len(c) != 2 {
			return fmt.Errorf("invalid download_countries value %q: must be 2-letter ISO code", c)
		}
	}
	for _, r := range n.DownloadRegistries {
		switch strings.ToLower(r) {
		case "arin", "ripencc", "apnic", "lacnic", "afrinic":
		default:
			return fmt.Errorf("invalid download_registries value %q: allowed are arin, ripencc, apnic, lacnic, afrinic", r)
		}
	}
	if n.RefreshTime != "" {
		if _, _, err := parseTime(n.RefreshTime); err != nil {
			return err
		}
	}
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (n *NetxGeolocation) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	clientIP := n.getClientIP(r)
	if clientIP == "" {
		return next.ServeHTTP(w, r)
	}

	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		n.logger.Debug("could not parse client IP", zap.String("ip", clientIP), zap.Error(err))
		return next.ServeHTTP(w, r)
	}

	record := n.store.Lookup(addr)

	// Set placeholders
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if record != nil {
		repl.Set("netx_geo.country", record.Country)
		repl.Set("netx_geo.registry", record.Registry)
		repl.Set("netx_geo.org_name", record.OrgName)
		repl.Set("netx_geo.org_id", record.OrgID)
	} else {
		repl.Set("netx_geo.country", "-")
		repl.Set("netx_geo.registry", "-")
		repl.Set("netx_geo.org_name", "")
		repl.Set("netx_geo.org_id", "")
	}

	// Apply filters
	if !n.matchesFilters(record) {
		w.WriteHeader(http.StatusForbidden)
		return nil
	}

	return next.ServeHTTP(w, r)
}

// Cleanup releases this handler's reference to the shared dataset. The
// dataset — and the refresh loop that maintains it — is torn down only when
// the last handler using it goes away, which for a multi-site config is the
// final one to be cleaned up rather than the first.
func (n *NetxGeolocation) Cleanup() error {
	if n.shared == nil {
		return nil
	}
	return releaseDataset(n.key)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (n *NetxGeolocation) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name

	for d.NextBlock(0) {
		switch d.Val() {
		case "api_url":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n.APIURL = d.Val()
		case "data_dir":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n.DataDir = d.Val()
		case "refresh_time":
			if !d.NextArg() {
				return d.ArgErr()
			}
			n.RefreshTime = d.Val()
		case "download_countries":
			n.DownloadCountries = append(n.DownloadCountries, d.RemainingArgs()...)
		case "download_registries":
			n.DownloadRegistries = append(n.DownloadRegistries, d.RemainingArgs()...)
		case "allow_countries":
			n.AllowCountries = append(n.AllowCountries, d.RemainingArgs()...)
		case "deny_countries":
			n.DenyCountries = append(n.DenyCountries, d.RemainingArgs()...)
		case "allow_orgs":
			n.AllowOrgs = append(n.AllowOrgs, d.RemainingArgs()...)
		case "deny_orgs":
			n.DenyOrgs = append(n.DenyOrgs, d.RemainingArgs()...)
		case "allow_registries":
			n.AllowRegistries = append(n.AllowRegistries, d.RemainingArgs()...)
		case "deny_registries":
			n.DenyRegistries = append(n.DenyRegistries, d.RemainingArgs()...)
		default:
			return d.Errf("unknown subdirective: %s", d.Val())
		}
	}

	return nil
}

func (n *NetxGeolocation) getClientIP(r *http.Request) string {
	if val := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey); val != nil {
		if ip, ok := val.(string); ok && ip != "" {
			return ip
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (n *NetxGeolocation) matchesFilters(record *geoRecord) bool {
	if record == nil {
		return len(n.AllowCountries) == 0 && len(n.AllowOrgs) == 0 && len(n.AllowRegistries) == 0
	}

	if !checkAllowed(record.Country, n.AllowCountries, n.DenyCountries) {
		return false
	}
	if !checkAllowed(record.OrgName, n.AllowOrgs, n.DenyOrgs) {
		return false
	}
	if !checkAllowed(record.Registry, n.AllowRegistries, n.DenyRegistries) {
		return false
	}

	return true
}

// checkAllowed returns true if the item passes the allow/deny logic.
// Deny takes precedence over allow.
func checkAllowed(item string, allow, deny []string) bool {
	if item == "" {
		item = "-"
	}
	upperItem := strings.ToUpper(item)

	for _, d := range deny {
		if strings.ToUpper(d) == upperItem {
			return false
		}
	}

	if len(allow) == 0 {
		return true
	}

	for _, a := range allow {
		if strings.ToUpper(a) == upperItem {
			return true
		}
	}

	return false
}

// geoRecord is the internal representation of a resolved IP.
type geoRecord struct {
	Country  string
	Registry string
	OrgName  string
	OrgID    string
}

// Interface guards
var (
	_ caddy.Module                = (*NetxGeolocation)(nil)
	_ caddy.Provisioner           = (*NetxGeolocation)(nil)
	_ caddy.Validator             = (*NetxGeolocation)(nil)
	_ caddy.CleanerUpper          = (*NetxGeolocation)(nil)
	_ caddyhttp.MiddlewareHandler = (*NetxGeolocation)(nil)
	_ caddyfile.Unmarshaler       = (*NetxGeolocation)(nil)
)
