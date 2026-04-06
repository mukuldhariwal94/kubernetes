package generic

import (
	"container/list"
	"sync"
	"time"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

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
	return &LRUPolicyCache{
		entries:      make(map[types.NamespacedName]*CacheEntry),
		lru:          list.New(),
		maxSizeBytes: maxSizeBytes,
		metrics: &PolicyCacheMetrics{
			MaxSize: maxSizeBytes,
		},
	}
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
		klog.V(4).Infof("CEL cache hit for policy %s/%s (hits: %d, size: %dMB)", 
			key.Namespace, key.Name, c.metrics.Hits, c.currentSize/(1024*1024))
		return entry.Policy, true
	}

	c.metrics.Misses++
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

	// Add new entry
	entry := &CacheEntry{
		Policy:     policy,
		Size:       estimatedSize,
		LastAccess: time.Now(),
		Accessed:   1,
	}
	element := c.lru.PushFront(key)
	entry.element = element
	c.entries[key] = entry
	c.currentSize += estimatedSize
	c.metrics.CurrentCount++
	c.metrics.CurrentSize = c.currentSize

	klog.V(2).Infof("CEL cache put policy %s/%s (size: %dKB, total: %dMB, count: %d)", 
		key.Namespace, key.Name, estimatedSize/1024, c.currentSize/(1024*1024), c.metrics.CurrentCount)
}

// evictOneLocked removes the least recently used entry (must be called under lock)
func (c *LRUPolicyCache) evictOneLocked() {
	back := c.lru.Back()
	if back == nil {
		return
	}

	// Find the key associated with this element
	var keyToRemove types.NamespacedName
	for k, v := range c.entries {
		if v.element == back {
			keyToRemove = k
			break
		}
	}

	// Remove the entry
	if entry, exists := c.entries[keyToRemove]; exists {
		c.currentSize -= entry.Size
		delete(c.entries, keyToRemove)
		c.lru.Remove(back)
		c.metrics.CurrentCount--
		c.metrics.Evictions++
		klog.V(2).Infof("CEL cache eviction: policy %s/%s (accessed %d times, size: %dKB)", 
			keyToRemove.Namespace, keyToRemove.Name, entry.Accessed, entry.Size/1024)
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
