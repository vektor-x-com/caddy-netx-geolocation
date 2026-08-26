package caddy_netx_geolocation

import (
	"fmt"
	"math/bits"
	"net/netip"
)

// rangeToPrefixes decomposes an inclusive address range into the minimal set of
// CIDR prefixes that covers exactly it — no more, no less.
//
// The API reports allocations as start/end pairs because that is what the RIRs
// publish, and such a range is frequently not expressible as a single prefix:
// 192.0.2.4–192.0.2.11 is 192.0.2.4/30 plus 192.0.2.8/30. The trie indexes
// prefixes, so something has to do this conversion, and doing it here rather
// than server-side keeps the export faithful to the source data.
//
// This replaces the previous approach of appending "/32" to the start address,
// which claimed a single host for every allocation and silently dropped the
// rest — a /24 resolved for exactly one of its 256 addresses.
//
// The result is bounded: a range needs at most 2*bits-2 prefixes (62 for IPv4,
// 254 for IPv6), reached only by maximally misaligned ranges.
func rangeToPrefixes(start, end netip.Addr) ([]netip.Prefix, error) {
	start = start.Unmap()
	end = end.Unmap()

	if !start.IsValid() || !end.IsValid() {
		return nil, fmt.Errorf("invalid address in range %s-%s", start, end)
	}
	if start.Is4() != end.Is4() {
		return nil, fmt.Errorf("mixed address families in range %s-%s", start, end)
	}
	if end.Less(start) {
		return nil, fmt.Errorf("reversed range %s-%s", start, end)
	}

	bitLen := start.BitLen()
	var out []netip.Prefix
	cur := start

	for {
		// The largest block that can start at cur is limited by cur's own
		// alignment: a block of 2^n addresses must begin on a 2^n boundary, so
		// n can be at most the number of trailing zero bits in cur.
		n := trailingZeroBits(cur)
		if n > bitLen {
			n = bitLen
		}

		// Shrink until the block fits inside the range. setLowBits(cur, n) is
		// the block's last address — cur + 2^n - 1 — computable without
		// arithmetic precisely because cur's low n bits are already zero.
		for n > 0 && end.Less(setLowBits(cur, n)) {
			n--
		}

		out = append(out, netip.PrefixFrom(cur, bitLen-n))

		last := setLowBits(cur, n)
		if !last.Less(end) {
			break
		}
		next := last.Next()
		if !next.IsValid() {
			// Ran off the top of the address space. Unreachable given the
			// !last.Less(end) break above, but the range would be complete
			// either way.
			break
		}
		cur = next
	}

	return out, nil
}

// addrBytes returns the address's octets. As4/As16 return arrays by value, so
// the slice returned here is backed by a fresh copy on every call and callers
// may mutate it freely.
func addrBytes(a netip.Addr) []byte {
	if a.Is4() {
		b := a.As4()
		return b[:]
	}
	b := a.As16()
	return b[:]
}

// trailingZeroBits counts the low-order zero bits of an address, which is the
// exponent of the largest CIDR block that can begin at it. Returns BitLen for
// the all-zero address.
func trailingZeroBits(a netip.Addr) int {
	b := addrBytes(a)
	n := 0
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == 0 {
			n += 8
			continue
		}
		n += bits.TrailingZeros8(b[i])
		break
	}
	return n
}

// setLowBits returns a with its n lowest bits set to 1. Where a's low n bits
// are already zero this is exactly a + 2^n - 1, i.e. the last address of the
// 2^n-sized block starting at a.
func setLowBits(a netip.Addr, n int) netip.Addr {
	b := addrBytes(a)
	for i := 0; i < n; i++ {
		b[len(b)-1-i/8] |= 1 << (i % 8)
	}
	out, ok := netip.AddrFromSlice(b)
	if !ok {
		return a
	}
	return out
}
