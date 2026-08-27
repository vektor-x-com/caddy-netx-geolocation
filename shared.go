package caddy_netx_geolocation

import (
	"fmt"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// geoPool holds one dataset per distinct data source, shared by every handler
// instance that reads it.
//
// Caddy provisions a handler once per place a directive appears, and a
// geolocation dataset is large: at 1.5M ranges one trie is ~348MB. A config
// serving 27 domains therefore built 27 identical copies and needed ~9.4GB,
// which the kernel's OOM killer ended at around the sixteenth. Nothing about
// that config was unreasonable — the dataset simply is not per-site state, and
// treating it as such does not scale with the number of sites.
//
// UsagePool is Caddy's own answer to this: it reference-counts a shared value
// and destructs it when the last holder releases it, which also gives correct
// behaviour across config reloads, where new instances are provisioned before
// the old ones are cleaned up.
var geoPool = caddy.NewUsagePool()

// sharedDataset is one dataset plus the scheduler that keeps it current.
type sharedDataset struct {
	store     *dataStore
	scheduler *refreshScheduler
}

// Destruct stops the refresh loop. Called by UsagePool when the last handler
// using this dataset is cleaned up.
func (s *sharedDataset) Destruct() error {
	s.scheduler.Stop()
	return nil
}

// datasetKey identifies a dataset by everything that determines its contents.
//
// The Allow/Deny fields are deliberately absent. Those filter requests against
// a dataset rather than changing it, so two sites that download identical data
// and apply different country rules share one copy — which is the common case
// and the one that was multiplying memory.
type datasetKey struct {
	apiURL      string
	dataFile    string
	refreshTime string
	countries   string
	registries  string
}

func newDatasetKey(apiURL, dataFile, refreshTime string, countries, registries []string) datasetKey {
	return datasetKey{
		apiURL:      apiURL,
		dataFile:    dataFile,
		refreshTime: refreshTime,
		// Joined rather than compared as slices so the key stays comparable,
		// which UsagePool requires of map keys.
		countries:  strings.Join(countries, ","),
		registries: strings.Join(registries, ","),
	}
}

// loadDataset returns the dataset for key, creating and starting it if this is
// the first handler to ask for it. Every caller must pair this with
// releaseDataset.
func loadDataset(key datasetKey, f *fetcher, logger *zap.Logger) (*sharedDataset, error) {
	val, loaded, err := geoPool.LoadOrNew(key, func() (caddy.Destructor, error) {
		store := newDataStore(key.dataFile)

		loadErr := store.LoadFromFile()
		if loadErr != nil {
			logger.Warn("could not load local data file, will fetch from API",
				zap.String("file", key.dataFile),
				zap.Error(loadErr),
			)
		} else {
			logger.Info("loaded data from local file",
				zap.String("file", key.dataFile),
				zap.Int("entries", store.EntryCount()),
			)
		}

		sched, err := newScheduler(key.refreshTime, f, store, logger)
		if err != nil {
			return nil, err
		}
		sched.Start()

		// Populate in the background when there was no local file to start
		// from. Provisioning must not block on this: Caddy loads a new config
		// before releasing the previous one's listeners, so waiting here holds
		// the whole reload open — which showed up as "bind: address already in
		// use" after the handler spent minutes in provisioning.
		if loadErr != nil {
			sched.RefreshNow()
		}

		return &sharedDataset{store: store, scheduler: sched}, nil
	})
	if err != nil {
		return nil, err
	}

	ds, ok := val.(*sharedDataset)
	if !ok {
		return nil, fmt.Errorf("unexpected value in dataset pool: %T", val)
	}
	if loaded {
		logger.Debug("reusing dataset already loaded by another site",
			zap.String("file", key.dataFile),
			zap.Int("entries", ds.store.EntryCount()),
		)
	}
	return ds, nil
}

// releaseDataset drops this handler's reference. The dataset is destructed
// once the last holder releases it.
func releaseDataset(key datasetKey) error {
	_, err := geoPool.Delete(key)
	return err
}
