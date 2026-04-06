package generic

import (
	"container/list"
	"sync"
	"time"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// LRU Cache Version - increment with each fix
const LRUCacheVersion = "v4-lazy-evaluation"

// PolicyCacheMetrics tracks cache performance
type PolicyCacheMetrics struct {
	Hits          int64
	Misses        int64
	Evictions     int64
	CurrentSize   int64
	CurrentCount  int64
	MaxSize       int64
}

// CacheEntry represents a single compiled policy in the LRU cache
type CacheEntry struct {
	Key         types.NamespacedName
	Policy      interface{}
	Size        int64
	LastAccess  time.Time
	Accessed    int64
	element     *list.Element
}

// LRUPolicyCache implements an LRU cache with size-based eviction
type LRUPolicyCache struct {
	entries      map[types.NamespacedName]*CacheEntry
	lru          *list.List
	maxSizeBytes int64
	currentSize  int64
	mu           sync.RWMutex
	metrics      *PolicyCacheMetrics
}

// NewLRUPolicyCache creates a new LRU cache with specified max size
func NewLRUPolicyCache(maxSizeBytes int64) *LRUPolicyCache {
	cache := &LRUPolicyCache{
		entries:      make(map[types.NamespacedName]*CacheEntry),
		lru:          list.New(),
		maxSizeBytes: maxSizeBytes,
		metrics: &PolicyCacheMetrics{
			MaxSize: maxSizeBytes,
		},
	}
	klog.Infof("🔧 LRU Cache initialized: version=%s, maxSize=%dMB, features=[eviction,metrics,O1-ops,thread-safe]", 
		LRUCacheVersion, maxSizeBytes/(1024*1024))
	return cache
}

// Get retrieves a compiled policy from the cache
func (c *LRUPolicyCache) Get(key types.NamespacedName) (interface{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, exists := c.entries[key]; exists {
		entry.LastAccess = time.Now()
		entry.Accessed++
		c.lru.MoveToFront(entry.element)
		c.metrics.Hits++
		klog.Infof("CEL_POLICY_TRACE: [Cache] Hit for policy %s/%s (hits: %d, size: %dMB)", 
			key.Namespace, key.Name, c.metrics.Hits, c.currentSize/(1024*1024))
		return entry.Policy, true
	}

	c.metrics.Misses++
	klog.Infof("CEL_POLICY_TRACE: [Cache] Miss for policy %s/%s", key.Namespace, key.Name)
	return nil, false
}

// Put inserts or updates a compiled policy in the cache
func (c *LRUPolicyCache) Put(key types.NamespacedName, policy interface{}, estimatedSize int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If entry already exists, remove it
	if oldEntry, exists := c.entries[key]; exists {
		c.currentSize -= oldEntry.Size
		c.lru.Remove(oldEntry.element)
		c.metrics.CurrentCount--
	}

	// Evict LRU entries if needed to make room
	for c.currentSize+estimatedSize > c.maxSizeBytes && c.lru.Len() > 0 {
		c.evictOneLocked()
	}

	// Add new entry - store the entry in the list element's Value
	entry := &CacheEntry{
		Key:         key,
		Policy:      policy,
		Size:        estimatedSize,
		LastAccess:  time.Now(),
		Accessed:    1,
	}
	element := c.lru.PushFront(entry)  // Store entire entry in list
	entry.element = element
	c.entries[key] = entry
	c.currentSize += estimatedSize
	c.metrics.CurrentCount++
	c.metrics.CurrentSize = c.currentSize

	klog.Infof("CEL_POLICY_TRACE: [Cache] Put policy %s/%s (size: %dMB, total: %dMB, count: %d, limit: %dMB)", 
		key.Namespace, key.Name, estimatedSize/(1024*1024), c.currentSize/(1024*1024), c.metrics.CurrentCount, c.maxSizeBytes/(1024*1024))
		
	// Print current cached policy keys
	keys := make([]string, 0, len(c.entries))
	for k := range c.entries {
		keys = append(keys, k.String())
	}
	klog.Infof("CEL_POLICY_TRACE: [Cache] Current keys: %v", keys)
}

// evictOneLocked removes the least recently used entry (must be called under lock)
func (c *LRUPolicyCache) evictOneLocked() {
	back := c.lru.Back()
	if back == nil {
		return
	}

	// Get the key from the element's Value (which is a CacheEntry pointer)
	if cachedEntry, ok := back.Value.(*CacheEntry); ok {
		keyToRemove := cachedEntry.Key
		
		// Remove the entry
		if entry, exists := c.entries[keyToRemove]; exists {
			c.currentSize -= entry.Size
			delete(c.entries, keyToRemove)
			c.lru.Remove(back)
			c.metrics.CurrentCount--
			c.metrics.Evictions++
			klog.Infof("CEL_POLICY_TRACE: [Cache] Eviction policy %s/%s (accessed %d times, size: %dMB, cache at %dMB/%dMB)", 
				keyToRemove.Namespace, keyToRemove.Name, entry.Accessed, entry.Size/(1024*1024), c.currentSize/(1024*1024), c.maxSizeBytes/(1024*1024))
			
			// Print current cached policy keys after eviction
			keys := make([]string, 0, len(c.entries))
			for k := range c.entries {
				keys = append(keys, k.String())
			}
			klog.Infof("CEL_POLICY_TRACE: [Cache] Current keys after eviction: %v", keys)
		}
	}
}

// Remove explicitly removes an entry from the cache
func (c *LRUPolicyCache) Remove(key types.NamespacedName) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, exists := c.entries[key]; exists {
		c.currentSize -= entry.Size
		delete(c.entries, key)
		c.lru.Remove(entry.element)
		c.metrics.CurrentCount--
		
		// Print current cached policy keys after removal
		keys := make([]string, 0, len(c.entries))
		for k := range c.entries {
			keys = append(keys, k.String())
		}
		klog.Infof("CEL_POLICY_TRACE: [Cache] Remove policy %s/%s, remaining keys: %v", key.Namespace, key.Name, keys)
		
		return true
	}
	return false
}

// Clear removes all entries from the cache
func (c *LRUPolicyCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[types.NamespacedName]*CacheEntry)
	c.lru.Init()
	c.currentSize = 0
	c.metrics.CurrentCount = 0
	c.metrics.CurrentSize = 0
	klog.V(2).Infof("CEL cache cleared")
}

// Keys returns a list of all current cached policy keys
func (c *LRUPolicyCache) Keys() []types.NamespacedName {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := make([]types.NamespacedName, 0, len(c.entries))
	for k := range c.entries {
		keys = append(keys, k)
	}
	return keys
}

// GetMetrics returns current cache metrics
func (c *LRUPolicyCache) GetMetrics() PolicyCacheMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return PolicyCacheMetrics{
		Hits:         c.metrics.Hits,
		Misses:       c.metrics.Misses,
		Evictions:    c.metrics.Evictions,
		CurrentSize:  c.currentSize,
		CurrentCount: c.metrics.CurrentCount,
		MaxSize:      c.maxSizeBytes,
	}
}

// Size returns the current cache size in bytes
func (c *LRUPolicyCache) Size() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentSize
}

// Count returns the number of entries in the cache
func (c *LRUPolicyCache) Count() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.metrics.CurrentCount
}

// HitRate returns the cache hit rate
func (c *LRUPolicyCache) HitRate() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := c.metrics.Hits + c.metrics.Misses
	if total == 0 {
		return 0
	}
	return float64(c.metrics.Hits) / float64(total)
}

// ResizeCache updates the maximum cache size
func (c *LRUPolicyCache) ResizeCache(newMaxSizeBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.maxSizeBytes = newMaxSizeBytes
	c.metrics.MaxSize = newMaxSizeBytes

	// Evict entries if new limit is smaller than current size
	for c.currentSize > c.maxSizeBytes && c.lru.Len() > 0 {
		c.evictOneLocked()
	}

	klog.V(2).Infof("CEL cache resized to %dMB", newMaxSizeBytes/(1024*1024))
}
