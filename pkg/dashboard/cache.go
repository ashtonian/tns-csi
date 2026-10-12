package dashboard

import (
	"context"
	"sync"
	"time"

	"github.com/fenio/tns-csi/pkg/tnsapi"
	"golang.org/x/sync/singleflight"
)

// dashboardCacheTTL bounds how stale the dashboard's dataset listings may be. One page
// refresh fires several partials in parallel, and each of them used to run the same
// managed-dataset query against TrueNAS.
const dashboardCacheTTL = 3 * time.Second

// cachedClient wraps a TrueNAS client and caches FindDatasetsByProperty results for a
// short TTL, collapsing concurrent identical lookups into a single request. The
// dashboard is read-only, so a few seconds of staleness is acceptable.
type cachedClient struct {
	tnsapi.ClientInterface
	entries map[string]cachedDatasets
	now     func() time.Time
	group   singleflight.Group
	ttl     time.Duration
	mu      sync.Mutex
}

type cachedDatasets struct {
	expires  time.Time
	datasets []tnsapi.DatasetWithProperties
}

func newCachedClient(client tnsapi.ClientInterface, ttl time.Duration) *cachedClient {
	return &cachedClient{ClientInterface: client, ttl: ttl, now: time.Now, entries: map[string]cachedDatasets{}}
}

// FindDatasetsByProperty returns a cached result when one is fresh, and otherwise
// performs (or joins) a single lookup. Callers receive their own copy of the slice.
func (c *cachedClient) FindDatasetsByProperty(ctx context.Context, prefix, propertyName, propertyValue string) ([]tnsapi.DatasetWithProperties, error) {
	key := prefix + "\x00" + propertyName + "\x00" + propertyValue

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expires) {
		c.mu.Unlock()
		return append([]tnsapi.DatasetWithProperties(nil), e.datasets...), nil
	}
	c.mu.Unlock()

	v, err, _ := c.group.Do(key, func() (interface{}, error) {
		datasets, err := c.ClientInterface.FindDatasetsByProperty(ctx, prefix, propertyName, propertyValue)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.entries[key] = cachedDatasets{datasets: datasets, expires: c.now().Add(c.ttl)}
		c.mu.Unlock()
		return datasets, nil
	})
	if err != nil {
		return nil, err
	}
	if datasets, ok := v.([]tnsapi.DatasetWithProperties); ok {
		return append([]tnsapi.DatasetWithProperties(nil), datasets...), nil
	}
	return nil, nil
}

// FindManagedDatasets goes through the cache too (the embedded client's version would
// bypass it).
func (c *cachedClient) FindManagedDatasets(ctx context.Context, prefix string) ([]tnsapi.DatasetWithProperties, error) {
	return c.FindDatasetsByProperty(ctx, prefix, tnsapi.PropertyManagedBy, tnsapi.ManagedByValue)
}
