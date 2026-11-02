# Caching

Result caches store successful `DomainInfo` and `IPInfo` responses from RDAP or WHOIS. A cache hit avoids registry discovery, network requests, and rate-limit waits.

[Back to README](../README.md#documentation)

## Default behavior

`NewClient()` and the zero-value `Client` create separate domain and IP caches. Each has a **five-minute TTL** and a **256-entry capacity**: up to 256 domains and 256 IP addresses per client. Package-level `reginfo.Lookup` and `reginfo.LookupIP` reuse the caches of their shared default client. All entries live in process memory and are lost on restart.

Domain cache keys are normalized registrable names in ASCII. `дом.рф`, `xn--d1aqf.xn--p1ai`, and `www.ДОМ.РФ.` share an entry. `github.io` and `user.github.io` also share an entry because registration boundaries use ICANN rules.

IP cache keys are normalized individual addresses. Equivalent IPv6 spellings share an entry; `::ffff:8.8.8.8` and `8.8.8.8` also share an entry. Different addresses in the same returned network remain separate keys, since an individual address may have a more specific registration.

The TTL starts when an entry is written. Reads do not extend it, and the entry is unavailable at the exact expiration time. Replacing an entry resets its TTL. Cached results are complete snapshots: `CollectedAt`, `AgeDays`, `Source`, and `Raw` are not recalculated on a hit.

[GeoIP enrichment](geoip.md) runs after the registry cache is read or written. The client stores registration fields without `IPInfo.GeoIP`, then recomputes geolocation from the current databases on every successful lookup. Updating GeoIP databases therefore takes effect immediately, including on cache hits. A GeoIP field returned by a custom cache is discarded before enrichment. Clients sharing the same cache can use separate GeoIP components.

When full, the cache removes expired entries first, then evicts the least recently used entry. There is no background cleanup goroutine. Expired entries are removed when read or when space is needed for another key. Capacity limits the number of entries, not their byte size; `Raw` is included in each entry.

The built-in caches support concurrent access. Reads and writes copy the result, including dates, CIDRs, and nested contact lists, so changing a returned value does not change the stored entry. Concurrent misses for the same key may issue multiple requests; each passes through the rate limiter.

Lookup failures, missing records, and HTTP 429 responses are not cached. Expired data is not served when a registry request fails. A canceled context returns an error even on a cache hit. Result caches do not control IANA bootstrap caching or `ScanProviders`.

## Configuration

The [example application](../example/reginfo/main.go) explicitly configures both caches and the limiter:

```go
limiter := reginfo.NewDefaultRateLimiter()
domainCache := reginfo.NewDefaultMemoryCache()
ipCache := reginfo.NewDefaultIPMemoryCache()

client := reginfo.NewClient(
    reginfo.WithRateLimiter(limiter),
    reginfo.WithCache(domainCache),
    reginfo.WithIPCache(ipCache),
)
```

Reuse a cache instance across clients to share results. `NewDefaultMemoryCache()`, `NewDefaultIPMemoryCache()`, and the zero values of `MemoryCache` and `IPMemoryCache` use the default TTL and capacity.

Both cache types accept `MemoryCacheConfig`:

```go
domainCache, err := reginfo.NewMemoryCache(reginfo.MemoryCacheConfig{
    TTL:        15 * time.Minute,
    MaxEntries: 1000,
})
if err != nil {
    return err
}

ipCache, err := reginfo.NewIPMemoryCache(reginfo.MemoryCacheConfig{
    TTL:        10 * time.Minute,
    MaxEntries: 500,
})
if err != nil {
    return err
}

client := reginfo.NewClient(
    reginfo.WithCache(domainCache),
    reginfo.WithIPCache(ipCache),
)
```

A zero configuration field selects its default; a negative value returns an error. `WithCache(nil)` disables domain caching, and `WithIPCache(nil)` disables IP caching. Pass both options to disable both result caches. IANA bootstrap data continues to be cached separately.

## Custom implementations

Implement `reginfo.Cache` for domains or `reginfo.IPCache` for IP addresses. The interfaces, shown from an importing package, are:

```go
type Cache interface {
    Get(ctx context.Context, domain string) (*reginfo.DomainInfo, error)
    Set(ctx context.Context, domain string, info *reginfo.DomainInfo) error
}

type IPCache interface {
    Get(ctx context.Context, ip string) (*reginfo.IPInfo, error)
    Set(ctx context.Context, ip string, info *reginfo.IPInfo) error
}
```

Connect an implementation using `reginfo.WithCache(myCache)` or `reginfo.WithIPCache(myIPCache)`. The interfaces are independent, so an existing domain cache does not need IP-specific methods.

The contract is the same for both interfaces:

- `Get` returns `nil, nil` for a miss or an expired entry. A non-nil result with no error is treated as a valid cached result.
- `Set` receives a copy of a successful lookup result. Domain names, including `UnicodeName`, are already populated.
- Keys are already normalized. Values must correspond to the supplied key; the cache does not need to repeat IDNA or IP normalization.
- The implementation controls TTL, capacity, and eviction. The client does not impose the built-in cache's TTL on a custom cache.
- Both methods receive the lookup context. Honor cancellation, bound storage waits, and support concurrent calls.
- The client copies values before passing them to `Set` and before returning a cached result to its caller. The cache must still provide safe reads: do not mutate a value returned by `Get`, or its nested fields, concurrently with the client reading it.

A `Get` error is treated as a miss. A `Set` error does not discard a successful registry response. Cache errors are not returned by `Lookup` or `LookupIP`; add logging or metrics inside the implementation if needed. Cancellation of the lookup context itself is returned to the caller, including cancellation during cache access.

An interface assertion can check a custom implementation at compile time:

```go
var _ reginfo.Cache = (*MyCache)(nil)
var _ reginfo.IPCache = (*MyIPCache)(nil)
```
