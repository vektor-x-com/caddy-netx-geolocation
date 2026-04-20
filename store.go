package caddy_netx_geolocation

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
)

const storeVersion byte = 1

// dataStore manages the in-memory IP trie and its persistence to disk.
type dataStore struct {
	mu       sync.RWMutex
	trie     *ipTrie
	entries  []cidrEntry
	filePath string
}

// cidrEntry is a flat record for persistence and trie building.
type cidrEntry struct {
	PrefixStr string // stored as string for gob compatibility
	Record    geoRecord
}

func (e cidrEntry) prefix() (netip.Prefix, error) {
	return netip.ParsePrefix(e.PrefixStr)
}

func newDataStore(filePath string) *dataStore {
	return &dataStore{
		trie:     newIPTrie(),
		filePath: filePath,
	}
}

// Lookup finds the geo record for the given IP.
func (ds *dataStore) Lookup(ip netip.Addr) *geoRecord {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.trie.Lookup(ip)
}

// Replace rebuilds the trie from new entries under a write lock.
func (ds *dataStore) Replace(entries []cidrEntry) (int, int) {
	newTrie := newIPTrie()
	loaded := 0
	skipped := 0

	for i := range entries {
		prefix, err := entries[i].prefix()
		if err != nil {
			skipped++
			continue
		}
		newTrie.Insert(prefix, entries[i].Record)
		loaded++
	}

	ds.mu.Lock()
	ds.trie = newTrie
	ds.entries = entries
	ds.mu.Unlock()

	return loaded, skipped
}

// EntryCount returns the number of entries currently loaded.
func (ds *dataStore) EntryCount() int {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return len(ds.entries)
}

// SaveToFile persists the current entries to the gob file.
func (ds *dataStore) SaveToFile() error {
	ds.mu.RLock()
	entries := ds.entries
	ds.mu.RUnlock()

	if len(entries) == 0 {
		return nil
	}

	// Ensure directory exists
	dir := filepath.Dir(ds.filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteByte(storeVersion)

	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(entries); err != nil {
		return fmt.Errorf("encoding entries: %w", err)
	}

	// Write atomically via temp file + rename
	tmpPath := ds.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, ds.filePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file: %w", err)
	}

	return nil
}

// LoadFromFile reads and rebuilds the trie from the gob file.
func (ds *dataStore) LoadFromFile() error {
	data, err := os.ReadFile(ds.filePath)
	if err != nil {
		return fmt.Errorf("reading data file: %w", err)
	}

	if len(data) < 2 {
		return fmt.Errorf("data file too short")
	}

	version := data[0]
	if version != storeVersion {
		return fmt.Errorf("unsupported data file version: %d", version)
	}

	var entries []cidrEntry
	dec := gob.NewDecoder(bytes.NewReader(data[1:]))
	if err := dec.Decode(&entries); err != nil {
		return fmt.Errorf("decoding entries: %w", err)
	}

	ds.Replace(entries)
	return nil
}
