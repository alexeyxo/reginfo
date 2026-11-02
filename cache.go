package reginfo

import (
	"container/list"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

const (
	defaultCacheTTL        = 5 * time.Minute
	defaultCacheMaxEntries = 256
)

type Cache interface {
	Get(ctx context.Context, domain string) (*DomainInfo, error)
	Set(ctx context.Context, domain string, info *DomainInfo) error
}

type MemoryCacheConfig struct {
	TTL        time.Duration
	MaxEntries int
}

type MemoryCache struct {
	store memoryCache[DomainInfo]
}

type memoryCache[T any] struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	entries    map[string]*list.Element
	recent     list.List
}

type cacheEntry[T any] struct {
	key       string
	info      *T
	expiresAt time.Time
}

func NewDefaultMemoryCache() *MemoryCache {
	return &MemoryCache{}
}

func NewMemoryCache(config MemoryCacheConfig) (*MemoryCache, error) {
	if config.TTL < 0 || config.MaxEntries < 0 {
		return nil, errors.New("cache TTL and max entries must not be negative")
	}
	return &MemoryCache{store: memoryCache[DomainInfo]{ttl: config.TTL, maxEntries: config.MaxEntries}}, nil
}

func WithCache(cache Cache) Option {
	return func(c *Client) {
		c.cache = cache
		c.cacheConfigured = true
	}
}

func (c *MemoryCache) Get(ctx context.Context, domain string) (*DomainInfo, error) {
	info, err := c.store.get(ctx, domain)
	return cloneDomainInfo(info), err
}

func (c *MemoryCache) Set(ctx context.Context, domain string, info *DomainInfo) error {
	return c.store.set(ctx, domain, cloneDomainInfo(info))
}

//nolint:nilnil // The cache contract returns nil without an error on a miss.
func (c *memoryCache[T]) get(ctx context.Context, key string) (*T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	element := c.entries[key]
	if element == nil {
		return nil, nil
	}
	entry := memoryCacheEntry[T](element)
	if !time.Now().Before(entry.expiresAt) {
		c.remove(element)
		return nil, nil
	}
	c.recent.MoveToFront(element)
	return entry.info, nil
}

func (c *memoryCache[T]) set(ctx context.Context, key string, info *T) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if info == nil {
		return errors.New("cache value must not be nil")
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if c.ttl == 0 {
		c.ttl = defaultCacheTTL
	}
	if c.maxEntries == 0 {
		c.maxEntries = defaultCacheMaxEntries
	}
	now := time.Now()
	entry := &cacheEntry[T]{key: key, info: info, expiresAt: now.Add(c.ttl)}
	if element := c.entries[key]; element != nil {
		element.Value = entry
		c.recent.MoveToFront(element)
		return nil
	}
	if len(c.entries) >= c.maxEntries {
		for element := c.recent.Back(); element != nil; {
			previous := element.Prev()
			if !now.Before(memoryCacheEntry[T](element).expiresAt) {
				c.remove(element)
			}
			element = previous
		}
		if len(c.entries) >= c.maxEntries {
			c.remove(c.recent.Back())
		}
	}
	c.entries[key] = c.recent.PushFront(entry)
	return nil
}

func (c *memoryCache[T]) remove(element *list.Element) {
	delete(c.entries, memoryCacheEntry[T](element).key)
	c.recent.Remove(element)
}

func memoryCacheEntry[T any](element *list.Element) *cacheEntry[T] {
	entry, ok := element.Value.(*cacheEntry[T])
	if !ok {
		panic("invalid memory cache entry")
	}
	return entry
}

func cloneDomainInfo(info *DomainInfo) *DomainInfo {
	if info == nil {
		return nil
	}
	cloned := *info
	if info.CreatedAt != nil {
		value := *info.CreatedAt
		cloned.CreatedAt = &value
	}
	if info.UpdatedAt != nil {
		value := *info.UpdatedAt
		cloned.UpdatedAt = &value
	}
	if info.ExpiresAt != nil {
		value := *info.ExpiresAt
		cloned.ExpiresAt = &value
	}
	if info.AgeDays != nil {
		value := *info.AgeDays
		cloned.AgeDays = &value
	}
	cloned.NameServers = slices.Clone(info.NameServers)
	cloned.Status = slices.Clone(info.Status)
	return &cloned
}
