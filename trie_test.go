package caddy_netx_geolocation

import (
	"fmt"
	"net/netip"
	"testing"
)

func TestTrieInsertAndLookup(t *testing.T) {
	trie := newIPTrie()

	trie.Insert(netip.MustParsePrefix("192.168.1.0/24"), geoRecord{Country: "US"})
	trie.Insert(netip.MustParsePrefix("10.0.0.0/8"), geoRecord{Country: "DE"})

	tests := []struct {
		name    string
		ip      string
		country string
		found   bool
	}{
		{"match /24", "192.168.1.100", "US", true},
		{"match /24 start", "192.168.1.0", "US", true},
		{"match /24 end", "192.168.1.255", "US", true},
		{"no match outside /24", "192.168.2.1", "", false},
		{"match /8", "10.5.5.5", "DE", true},
		{"no match", "8.8.8.8", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := trie.Lookup(netip.MustParseAddr(tt.ip))
			if tt.found {
				if rec == nil {
					t.Fatal("expected match, got nil")
				}
				if rec.Country != tt.country {
					t.Errorf("expected country %s, got %s", tt.country, rec.Country)
				}
			} else {
				if rec != nil {
					t.Errorf("expected nil, got %+v", rec)
				}
			}
		})
	}
}

func TestTrieOverlappingCIDRs(t *testing.T) {
	trie := newIPTrie()

	// Insert broad range first, then more specific
	trie.Insert(netip.MustParsePrefix("10.0.0.0/8"), geoRecord{Country: "US"})
	trie.Insert(netip.MustParsePrefix("10.1.0.0/16"), geoRecord{Country: "DE"})
	trie.Insert(netip.MustParsePrefix("10.1.1.0/24"), geoRecord{Country: "FR"})

	tests := []struct {
		ip      string
		country string
	}{
		{"10.1.1.5", "FR"},   // matches /24 (most specific)
		{"10.1.2.5", "DE"},   // matches /16
		{"10.2.0.1", "US"},   // matches /8
		{"10.1.1.0", "FR"},   // exact start of /24
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			rec := trie.Lookup(netip.MustParseAddr(tt.ip))
			if rec == nil {
				t.Fatalf("expected match for %s, got nil", tt.ip)
			}
			if rec.Country != tt.country {
				t.Errorf("for %s: expected %s, got %s", tt.ip, tt.country, rec.Country)
			}
		})
	}
}

func TestTrieIPv6(t *testing.T) {
	trie := newIPTrie()

	trie.Insert(netip.MustParsePrefix("2001:db8::/32"), geoRecord{Country: "JP"})
	trie.Insert(netip.MustParsePrefix("2001:db8:1::/48"), geoRecord{Country: "KR"})

	tests := []struct {
		ip      string
		country string
		found   bool
	}{
		{"2001:db8:1::1", "KR", true},
		{"2001:db8:2::1", "JP", true},
		{"2001:db9::1", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			rec := trie.Lookup(netip.MustParseAddr(tt.ip))
			if tt.found {
				if rec == nil {
					t.Fatalf("expected match for %s", tt.ip)
				}
				if rec.Country != tt.country {
					t.Errorf("expected %s, got %s", tt.country, rec.Country)
				}
			} else if rec != nil {
				t.Errorf("expected nil for %s, got %+v", tt.ip, rec)
			}
		})
	}
}

func TestTrieIPv4MappedIPv6(t *testing.T) {
	trie := newIPTrie()
	trie.Insert(netip.MustParsePrefix("192.168.0.0/16"), geoRecord{Country: "GB"})

	// Lookup with IPv4-mapped IPv6 address
	ip := netip.MustParseAddr("::ffff:192.168.1.1")
	rec := trie.Lookup(ip)
	if rec == nil {
		t.Fatal("expected match for IPv4-mapped IPv6")
	}
	if rec.Country != "GB" {
		t.Errorf("expected GB, got %s", rec.Country)
	}
}

func BenchmarkTrieLookup(b *testing.B) {
	trie := newIPTrie()

	// Insert a bunch of prefixes
	for i := 0; i < 256; i++ {
		prefix := netip.MustParsePrefix(fmt.Sprintf("%d.0.0.0/8", i))
		trie.Insert(prefix, geoRecord{Country: "XX"})
	}

	ip := netip.MustParseAddr("128.5.10.20")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		trie.Lookup(ip)
	}
}
