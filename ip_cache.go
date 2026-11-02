package reginfo

import (
	"context"
	"errors"
	"slices"
)

type IPCache interface {
	Get(ctx context.Context, ip string) (*IPInfo, error)
	Set(ctx context.Context, ip string, info *IPInfo) error
}

type IPMemoryCache struct {
	store memoryCache[IPInfo]
}

func NewDefaultIPMemoryCache() *IPMemoryCache {
	return &IPMemoryCache{}
}

func NewIPMemoryCache(config MemoryCacheConfig) (*IPMemoryCache, error) {
	if config.TTL < 0 || config.MaxEntries < 0 {
		return nil, errors.New("cache TTL and max entries must not be negative")
	}
	return &IPMemoryCache{store: memoryCache[IPInfo]{ttl: config.TTL, maxEntries: config.MaxEntries}}, nil
}

func WithIPCache(cache IPCache) Option {
	return func(c *Client) {
		c.ipCache = cache
		c.ipCacheConfigured = true
	}
}

func (c *IPMemoryCache) Get(ctx context.Context, ip string) (*IPInfo, error) {
	info, err := c.store.get(ctx, ip)
	return cloneIPInfo(info), err
}

func (c *IPMemoryCache) Set(ctx context.Context, ip string, info *IPInfo) error {
	return c.store.set(ctx, ip, cloneIPInfo(info))
}

func cloneIPInfo(info *IPInfo) *IPInfo {
	if info == nil {
		return nil
	}
	cloned := *info
	if info.GeoIP != nil {
		geo := *info.GeoIP
		geo.Subdivisions = slices.Clone(info.GeoIP.Subdivisions)
		if geo.Latitude != nil {
			latitude := *geo.Latitude
			geo.Latitude = &latitude
		}
		if geo.Longitude != nil {
			longitude := *geo.Longitude
			geo.Longitude = &longitude
		}
		cloned.GeoIP = &geo
	}
	if info.CreatedAt != nil {
		value := *info.CreatedAt
		cloned.CreatedAt = &value
	}
	if info.UpdatedAt != nil {
		value := *info.UpdatedAt
		cloned.UpdatedAt = &value
	}
	cloned.CIDRs = slices.Clone(info.CIDRs)
	cloned.Status = slices.Clone(info.Status)
	cloned.Contacts = slices.Clone(info.Contacts)
	for i := range cloned.Contacts {
		contact := &cloned.Contacts[i]
		contact.Roles = slices.Clone(contact.Roles)
		contact.Emails = slices.Clone(contact.Emails)
		contact.Phones = slices.Clone(contact.Phones)
	}
	return &cloned
}
