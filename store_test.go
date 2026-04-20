package caddy_netx_geolocation

import (
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreReplaceAndLookup(t *testing.T) {
	ds := newDataStore(filepath.Join(t.TempDir(), "test.gob"))

	entries := []cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
		{PrefixStr: "192.168.0.0/16", Record: geoRecord{Country: "DE"}},
	}

	loaded, skipped := ds.Replace(entries)
	if loaded != 2 || skipped != 0 {
		t.Fatalf("expected loaded=2 skipped=0, got loaded=%d skipped=%d", loaded, skipped)
	}

	rec := ds.Lookup(netip.MustParseAddr("10.5.5.5"))
	if rec == nil || rec.Country != "US" {
		t.Errorf("expected US for 10.5.5.5, got %+v", rec)
	}

	rec = ds.Lookup(netip.MustParseAddr("192.168.1.1"))
	if rec == nil || rec.Country != "DE" {
		t.Errorf("expected DE for 192.168.1.1, got %+v", rec)
	}

	rec = ds.Lookup(netip.MustParseAddr("8.8.8.8"))
	if rec != nil {
		t.Errorf("expected nil for 8.8.8.8, got %+v", rec)
	}
}

func TestStoreReplaceSkipsMalformedCIDR(t *testing.T) {
	ds := newDataStore(filepath.Join(t.TempDir(), "test.gob"))

	entries := []cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
		{PrefixStr: "not-a-cidr", Record: geoRecord{Country: "XX"}},
		{PrefixStr: "999.999.999.0/24", Record: geoRecord{Country: "YY"}},
		{PrefixStr: "172.16.0.0/12", Record: geoRecord{Country: "DE"}},
	}

	loaded, skipped := ds.Replace(entries)
	if loaded != 2 {
		t.Errorf("expected 2 loaded, got %d", loaded)
	}
	if skipped != 2 {
		t.Errorf("expected 2 skipped, got %d", skipped)
	}
}

func TestStoreSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.gob")

	// Create and save
	ds1 := newDataStore(filePath)
	entries := []cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US", OrgName: "TestOrg"}},
		{PrefixStr: "2001:db8::/32", Record: geoRecord{Country: "JP"}},
	}
	ds1.Replace(entries)
	if err := ds1.SaveToFile(); err != nil {
		t.Fatalf("SaveToFile failed: %v", err)
	}

	// Load into fresh store
	ds2 := newDataStore(filePath)
	if err := ds2.LoadFromFile(); err != nil {
		t.Fatalf("LoadFromFile failed: %v", err)
	}

	if ds2.EntryCount() != 2 {
		t.Fatalf("expected 2 entries after load, got %d", ds2.EntryCount())
	}

	rec := ds2.Lookup(netip.MustParseAddr("10.1.2.3"))
	if rec == nil || rec.Country != "US" || rec.OrgName != "TestOrg" {
		t.Errorf("lookup after load failed: %+v", rec)
	}

	rec = ds2.Lookup(netip.MustParseAddr("2001:db8::1"))
	if rec == nil || rec.Country != "JP" {
		t.Errorf("IPv6 lookup after load failed: %+v", rec)
	}
}

func TestStoreLoadFromFileMissing(t *testing.T) {
	ds := newDataStore(filepath.Join(t.TempDir(), "nonexistent.gob"))
	err := ds.LoadFromFile()
	if err == nil {
		t.Fatal("expected error loading nonexistent file")
	}
}

func TestStoreLoadFromFileCorrupt(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "corrupt.gob")

	// Write garbage
	os.WriteFile(filePath, []byte{storeVersion, 0xFF, 0xFE, 0xFD}, 0644)

	ds := newDataStore(filePath)
	err := ds.LoadFromFile()
	if err == nil {
		t.Fatal("expected error loading corrupt file")
	}
}

func TestStoreLoadFromFileWrongVersion(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "wrong_version.gob")

	os.WriteFile(filePath, []byte{99, 0x00, 0x00}, 0644)

	ds := newDataStore(filePath)
	err := ds.LoadFromFile()
	if err == nil {
		t.Fatal("expected error for wrong version")
	}
}

func TestStoreLoadFromFileTooShort(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "short.gob")

	os.WriteFile(filePath, []byte{0x01}, 0644)

	ds := newDataStore(filePath)
	err := ds.LoadFromFile()
	if err == nil {
		t.Fatal("expected error for file too short")
	}
}

func TestStoreSaveEmptyNoFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "empty.gob")

	ds := newDataStore(filePath)
	// Don't replace anything — entries is nil
	if err := ds.SaveToFile(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// File should not exist since no entries
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Error("file should not be created for empty entries")
	}
}

func TestStoreSaveToReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	readOnlyDir := filepath.Join(dir, "readonly")
	os.MkdirAll(readOnlyDir, 0555)
	defer os.Chmod(readOnlyDir, 0755) // cleanup

	filePath := filepath.Join(readOnlyDir, "subdir", "test.gob")
	ds := newDataStore(filePath)
	ds.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	})

	err := ds.SaveToFile()
	if err == nil {
		t.Fatal("expected error writing to read-only directory")
	}
}

// --- Race condition tests (run with -race flag) ---

func TestStoreConcurrentLookupDuringReplace(t *testing.T) {
	ds := newDataStore(filepath.Join(t.TempDir(), "race.gob"))

	// Initial data
	ds.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip := netip.MustParseAddr("10.1.2.3")
			for {
				select {
				case <-stop:
					return
				default:
					rec := ds.Lookup(ip)
					// Record should always be valid (US or DE, never corrupted)
					if rec != nil && rec.Country != "US" && rec.Country != "DE" {
						t.Errorf("corrupted record: %+v", rec)
					}
				}
			}
		}()
	}

	// Concurrent writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			ds.Replace([]cidrEntry{
				{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "DE"}},
			})
			ds.Replace([]cidrEntry{
				{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
			})
		}
		close(stop)
	}()

	wg.Wait()
}

func TestStoreConcurrentSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "concurrent.gob")
	ds := newDataStore(filePath)

	ds.Replace([]cidrEntry{
		{PrefixStr: "10.0.0.0/8", Record: geoRecord{Country: "US"}},
		{PrefixStr: "172.16.0.0/12", Record: geoRecord{Country: "DE"}},
	})

	var wg sync.WaitGroup

	// Concurrent saves
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ds.SaveToFile()
		}()
	}

	// Concurrent lookups during save
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				ds.Lookup(netip.MustParseAddr("10.1.2.3"))
			}
		}()
	}

	wg.Wait()

	// Verify file is valid after concurrent writes
	ds2 := newDataStore(filePath)
	if err := ds2.LoadFromFile(); err != nil {
		t.Fatalf("file corrupted after concurrent saves: %v", err)
	}
}
