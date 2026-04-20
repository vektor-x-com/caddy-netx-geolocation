package caddy_netx_geolocation

import (
	"net/netip"
)

// ipTrie is a binary patricia trie for CIDR-based IP lookups.
// It maintains separate roots for IPv4 and IPv6.
type ipTrie struct {
	v4root *trieNode
	v6root *trieNode
}

type trieNode struct {
	left   *trieNode // bit 0
	right  *trieNode // bit 1
	record *geoRecord
}

func newIPTrie() *ipTrie {
	return &ipTrie{
		v4root: &trieNode{},
		v6root: &trieNode{},
	}
}

// Insert adds a CIDR prefix with its associated geo record to the trie.
func (t *ipTrie) Insert(prefix netip.Prefix, rec geoRecord) {
	prefix = prefix.Masked() // normalize
	addr := prefix.Addr()
	bits := prefix.Bits()

	var root *trieNode
	var addrBytes []byte

	if addr.Is4() {
		root = t.v4root
		a4 := addr.As4()
		addrBytes = a4[:]
	} else {
		root = t.v6root
		a16 := addr.As16()
		addrBytes = a16[:]
	}

	node := root
	for i := 0; i < bits; i++ {
		byteIdx := i / 8
		bitIdx := 7 - (i % 8)
		bit := (addrBytes[byteIdx] >> bitIdx) & 1

		if bit == 0 {
			if node.left == nil {
				node.left = &trieNode{}
			}
			node = node.left
		} else {
			if node.right == nil {
				node.right = &trieNode{}
			}
			node = node.right
		}
	}

	node.record = &rec
}

// Lookup finds the longest-prefix match for the given IP address.
// Returns nil if no matching prefix is found.
func (t *ipTrie) Lookup(ip netip.Addr) *geoRecord {
	var root *trieNode
	var addrBytes []byte
	var maxBits int

	if ip.Is4() {
		root = t.v4root
		a4 := ip.As4()
		addrBytes = a4[:]
		maxBits = 32
	} else if ip.Is4In6() {
		// Map IPv4-mapped IPv6 to IPv4 lookup
		root = t.v4root
		a4 := ip.Unmap().As4()
		addrBytes = a4[:]
		maxBits = 32
	} else {
		root = t.v6root
		a16 := ip.As16()
		addrBytes = a16[:]
		maxBits = 128
	}

	var lastMatch *geoRecord
	node := root

	if node.record != nil {
		lastMatch = node.record
	}

	for i := 0; i < maxBits; i++ {
		byteIdx := i / 8
		bitIdx := 7 - (i % 8)
		bit := (addrBytes[byteIdx] >> bitIdx) & 1

		if bit == 0 {
			node = node.left
		} else {
			node = node.right
		}

		if node == nil {
			break
		}

		if node.record != nil {
			lastMatch = node.record
		}
	}

	return lastMatch
}
