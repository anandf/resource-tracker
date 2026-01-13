package dynamic

import (
	"context"
	"fmt"
	"sync"

	"github.com/emirpasic/gods/sets/hashset"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/rest"
)

// DynamicTracker handles the analysis of ArgoCD application resources
type DynamicTracker struct {
	ResourceMapperStore map[string]*ResourceMapper
	// shared cache across ALL clusters: parentKey -> set(childKey)
	SharedRelationsCache map[string]*hashset.Set
	CacheMu              sync.RWMutex
	// per-cluster sync locks to avoid concurrent resyncs for the same cluster
	syncLocks map[string]*sync.Mutex
	// logger for the tracker
	logger *log.Entry
}

// NewDynamicTracker creates a new resource tracker instance
func NewDynamicTracker(logger *log.Entry) *DynamicTracker {
	return &DynamicTracker{
		ResourceMapperStore:  make(map[string]*ResourceMapper),
		SharedRelationsCache: make(map[string]*hashset.Set),
		syncLocks:            make(map[string]*sync.Mutex),
		logger:               logger,
	}
}

// GetClusterSyncLock returns a per-cluster mutex, creating it if needed.
// It is safe to call concurrently.
func (rt *DynamicTracker) GetClusterSyncLock(server string) *sync.Mutex {
	rt.CacheMu.Lock()
	defer rt.CacheMu.Unlock()

	if rt.syncLocks == nil {
		rt.syncLocks = make(map[string]*sync.Mutex)
	}
	if l, ok := rt.syncLocks[server]; ok {
		return l
	}
	mu := &sync.Mutex{}
	rt.syncLocks[server] = mu
	return mu
}

// EnsureSyncedSharedCacheOnHost ensures the shared cache is synced on the given server.
func (rt *DynamicTracker) EnsureSyncedSharedCacheOnHost(ctx context.Context, server string) {

	mapper, ok := rt.ResourceMapperStore[server]
	if !ok || mapper == nil {
		rt.logger.Warningf("No mapper for host %s", server)
		return
	}
	rt.logger.Infof("Querying relations host=%s", server)
	rel, err := mapper.GetClusterResourcesRelation(ctx)
	if err != nil {
		rt.logger.Warningf("Dynamic scan on %s failed: %v", server, err)
		return
	}
	rt.CacheMu.Lock()
	mergeInto(rt.SharedRelationsCache, rel)
	rt.CacheMu.Unlock()
}

// SyncResourceMapper syncs the resource mapper for the given server.
func (rt *DynamicTracker) SyncResourceMapper(server string, restCfg *rest.Config) error {
	if _, exists := rt.ResourceMapperStore[server]; !exists {
		mapper, err := NewResourceMapper(restCfg)
		if err != nil {
			return fmt.Errorf("failed to create ResourceMapper: %w", err)
		}
		// Start CRD informer so add/update events invoke addToResourceList
		go mapper.StartInformer()
		rt.ResourceMapperStore[server] = mapper
	}
	return nil
}

// mergeInto adds rel (parent -> children) into dst
func mergeInto(dst, rel map[string]*hashset.Set) {
	for p, set := range rel {
		if _, ok := dst[p]; !ok {
			dst[p] = hashset.New()
		}
		for _, v := range set.Values() {
			if s, ok := v.(string); ok {
				dst[p].Add(s)
			}
		}
	}
}
