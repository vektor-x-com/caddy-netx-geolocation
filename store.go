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

// storeVersion is 2 because the on-disk payload gained the export ETag. A
// version-1 file decodes as a bare []cidrEntry and has no ETag to condition on;
// LoadFromFile rejects it, which costs one full re-download on upgrade and then
// never again. That is cheaper than the alternative of guessing.
const storeVersion byte = 2

// dataStore manages the in-memory IP trie and its persistence to disk.
type dataStore struct {
	mu       sync.RWMutex
	trie     *ipTrie
	entries  []cidrEntry
	etag     string
	filePath string
}

// persistedStore is the gob payload. Named fields rather than a bare slice so
// later additions do not need another version bump — gob tolerates unknown and
// missing fields within a struct.
type persistedStore struct {
	ETag    string
	Entries []cidrEntry
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

// Replace rebuilds the trie from new entries under a write lock. etag is the
// export snapshot the entries came from and is stored alongside them.
//
// The trie is built before the lock is taken so lookups keep serving the
// previous dataset for the duration — with ~465k prefixes the build is not
// instant, and a geolocation handler blocking on it would stall request
// handling rather than briefly return stale-but-valid answers.
func (ds *dataStore) Replace(entries []cidrEntry, etag string) (int, int) {
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
	ds.etag = etag
	ds.mu.Unlock()

	return loaded, skipped
}

// ETag returns the export snapshot identifier of the loaded data, or empty if
// none is known. Sent as If-None-Match on the next fetch.
func (ds *dataStore) ETag() string {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.etag
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
	payload := persistedStore{ETag: ds.etag, Entries: ds.entries}
	ds.mu.RUnlock()

	if len(payload.Entries) == 0 {
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
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("encoding entries: %w", err)
	}

	// Write atomically via temp file + rename.
	//
	// The temp name is unique per call rather than a fixed "<file>.tmp". Caddy
	// provisions a handler once per place it appears in a config, and two
	// instances pointed at the same data_dir shared that one path: both wrote
	// it, the first rename consumed it, and the second failed with ENOENT
	// ("renaming temp file: ... no such file or directory"). A unique name per
	// write makes concurrent savers independent — each renames its own file,
	// and whichever lands last wins with a complete copy.
	//
	// CreateTemp also creates in the destination directory, which keeps the
	// rename on one filesystem and therefore atomic.
	tmp, err := os.CreateTemp(dir, filepath.Base(ds.filePath)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	// From here every failure path removes the temp file; a partially written
	// one must never be left behind for a later run to trip over.
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	// Sync before the rename so a crash cannot leave the destination pointing
	// at a file whose contents never reached disk. Without it the rename can be
	// durable while the data is not, which surfaces as an unreadable cache on
	// the next boot rather than a missing one.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}
	// CreateTemp makes the file 0600; the cache is world-readable like the
	// previous WriteFile(0644) left it.
	if err := os.Chmod(tmpPath, 0644); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("setting temp file mode: %w", err)
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

	var payload persistedStore
	dec := gob.NewDecoder(bytes.NewReader(data[1:]))
	if err := dec.Decode(&payload); err != nil {
		return fmt.Errorf("decoding entries: %w", err)
	}

	ds.Replace(payload.Entries, payload.ETag)
	return nil
}
