package caddy_netx_geolocation

import (
	"net/netip"
	"strings"
	"testing"
)

func TestRangeToPrefixes(t *testing.T) {
	tests := []struct {
		name  string
		start string
		end   string
		want  []string
	}{
		{
			name:  "aligned /8",
			start: "10.0.0.0",
			end:   "10.255.255.255",
			want:  []string{"10.0.0.0/8"},
		},
		{
			name:  "single host",
			start: "192.0.2.1",
			end:   "192.0.2.1",
			want:  []string{"192.0.2.1/32"},
		},
		{
			// The canonical case the old "/32 on start_ip" logic destroyed:
			// eight addresses that are two /30s, not one host.
			name:  "unaligned range splits",
			start: "192.0.2.4",
			end:   "192.0.2.11",
			want:  []string{"192.0.2.4/30", "192.0.2.8/30"},
		},
		{
			// Starts one address into a block, so it cannot begin with a large
			// prefix and has to climb: /32, /31, /30 ... then descend again.
			name:  "start misaligned by one",
			start: "192.0.2.1",
			end:   "192.0.2.15",
			want: []string{
				"192.0.2.1/32", "192.0.2.2/31", "192.0.2.4/30", "192.0.2.8/29",
			},
		},
		{
			name:  "ends one short of a block",
			start: "192.0.2.0",
			end:   "192.0.2.14",
			want: []string{
				"192.0.2.0/29", "192.0.2.8/30", "192.0.2.12/31", "192.0.2.14/32",
			},
		},
		{
			name:  "crosses an octet boundary",
			start: "10.0.0.255",
			end:   "10.0.1.0",
			want:  []string{"10.0.0.255/32", "10.0.1.0/32"},
		},
		{
			name:  "entire IPv4 space",
			start: "0.0.0.0",
			end:   "255.255.255.255",
			want:  []string{"0.0.0.0/0"},
		},
		{
			name:  "aligned IPv6 /32",
			start: "2001:db8::",
			end:   "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff",
			want:  []string{"2001:db8::/32"},
		},
		{
			name:  "IPv6 single host",
			start: "2001:db8::1",
			end:   "2001:db8::1",
			want:  []string{"2001:db8::1/128"},
		},
		{
			name:  "IPv6 unaligned",
			start: "2001:db8::4",
			end:   "2001:db8::b",
			want:  []string{"2001:db8::4/126", "2001:db8::8/126"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rangeToPrefixes(
				netip.MustParseAddr(tt.start),
				netip.MustParseAddr(tt.end),
			)
			if err != nil {
				t.Fatalf("rangeToPrefixes(%s, %s): %v", tt.start, tt.end, err)
			}

			var gotStr []string
			for _, p := range got {
				gotStr = append(gotStr, p.String())
			}
			if strings.Join(gotStr, ",") != strings.Join(tt.want, ",") {
				t.Errorf("expected %v, got %v", tt.want, gotStr)
			}
		})
	}
}

// The prefixes must cover the range exactly: every address inside is matched by
// one of them, and neither neighbour outside is.
func TestRangeToPrefixesCoversExactly(t *testing.T) {
	start := netip.MustParseAddr("192.0.2.4")
	end := netip.MustParseAddr("192.0.2.11")

	prefixes, err := rangeToPrefixes(start, end)
	if err != nil {
		t.Fatal(err)
	}

	covered := func(a netip.Addr) bool {
		for _, p := range prefixes {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}

	for a := start; a.Compare(end) <= 0; a = a.Next() {
		if !covered(a) {
			t.Errorf("%s is inside the range but not covered", a)
		}
	}
	if covered(start.Prev()) {
		t.Errorf("%s is below the range but covered", start.Prev())
	}
	if covered(end.Next()) {
		t.Errorf("%s is above the range but covered", end.Next())
	}
}

func TestRangeToPrefixesRejectsBadInput(t *testing.T) {
	tests := []struct {
		name       string
		start, end string
	}{
		{"reversed", "10.0.0.10", "10.0.0.1"},
		{"mixed families", "10.0.0.0", "2001:db8::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rangeToPrefixes(
				netip.MustParseAddr(tt.start),
				netip.MustParseAddr(tt.end),
			)
			if err == nil {
				t.Fatalf("expected an error for %s-%s", tt.start, tt.end)
			}
		})
	}
}

// An IPv4-mapped IPv6 address must be treated as IPv4 so it lands in the v4
// side of the trie, matching how Lookup unmaps client addresses.
func TestRangeToPrefixesUnmapsV4In6(t *testing.T) {
	got, err := rangeToPrefixes(
		netip.MustParseAddr("::ffff:10.0.0.0"),
		netip.MustParseAddr("::ffff:10.255.255.255"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "10.0.0.0/8" {
		t.Fatalf("expected [10.0.0.0/8], got %v", got)
	}
}
