package caddy_netx_geolocation

import (
	"testing"
)

func TestCheckAllowed(t *testing.T) {
	tests := []struct {
		name   string
		item   string
		allow  []string
		deny   []string
		expect bool
	}{
		{"no lists", "US", nil, nil, true},
		{"allowed", "US", []string{"US", "DE"}, nil, true},
		{"not allowed", "RU", []string{"US", "DE"}, nil, false},
		{"denied", "CN", nil, []string{"CN", "RU"}, false},
		{"not denied", "US", nil, []string{"CN", "RU"}, true},
		{"deny takes precedence", "US", []string{"US"}, []string{"US"}, false},
		{"empty item becomes -", "", []string{"-"}, nil, true},
		{"case insensitive", "us", []string{"US"}, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkAllowed(tt.item, tt.allow, tt.deny)
			if got != tt.expect {
				t.Errorf("checkAllowed(%q, %v, %v) = %v, want %v", tt.item, tt.allow, tt.deny, got, tt.expect)
			}
		})
	}
}

func TestMatchesFilters(t *testing.T) {
	n := &NetxGeolocation{
		AllowCountries: []string{"US", "DE"},
		DenyOrgs:       []string{"Evil Corp"},
	}

	tests := []struct {
		name   string
		record *geoRecord
		expect bool
	}{
		{"allowed country", &geoRecord{Country: "US", OrgName: "Good Inc"}, true},
		{"denied country", &geoRecord{Country: "RU", OrgName: "Good Inc"}, false},
		{"denied org", &geoRecord{Country: "US", OrgName: "Evil Corp"}, false},
		{"nil record with allow list", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := n.matchesFilters(tt.record)
			if got != tt.expect {
				t.Errorf("matchesFilters(%+v) = %v, want %v", tt.record, got, tt.expect)
			}
		})
	}
}

func TestMatchesFiltersRegistry(t *testing.T) {
	n := &NetxGeolocation{
		DenyRegistries: []string{"apnic"},
	}

	tests := []struct {
		name   string
		record *geoRecord
		expect bool
	}{
		{"allowed registry", &geoRecord{Country: "US", Registry: "arin"}, true},
		{"denied registry", &geoRecord{Country: "JP", Registry: "apnic"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := n.matchesFilters(tt.record)
			if got != tt.expect {
				t.Errorf("matchesFilters(%+v) = %v, want %v", tt.record, got, tt.expect)
			}
		})
	}
}

func TestMatchesFiltersOrg(t *testing.T) {
	n := &NetxGeolocation{
		AllowOrgs: []string{"Google LLC", "Cloudflare Inc"},
	}

	tests := []struct {
		name   string
		record *geoRecord
		expect bool
	}{
		{"allowed org", &geoRecord{Country: "US", OrgName: "Google LLC"}, true},
		{"not allowed org", &geoRecord{Country: "US", OrgName: "Random ISP"}, false},
		{"case insensitive", &geoRecord{Country: "US", OrgName: "google llc"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := n.matchesFilters(tt.record)
			if got != tt.expect {
				t.Errorf("matchesFilters(%+v) = %v, want %v", tt.record, got, tt.expect)
			}
		})
	}
}
