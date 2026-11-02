# Rate-limit storage

`RateLimitStore` lets several limiters share request counters and HTTP 429 cooldowns. The built-in implementation keeps state in memory; an application can supply its own Redis or database implementation to coordinate multiple processes. Service policies, request waiting, and `Retry-After` parsing remain in `RateLimiter`.

[Back to README](../README.md#documentation) · [Service limits](rate-limits.md)

## Configuration

Pass your store to `NewRateLimiter` through `RateLimitConfig.Store`, then use the limiter with a registry client or GeoIP:

```go
func newClientWithStore(store reginfo.RateLimitStore) (*reginfo.Client, error) {
    limiter, err := reginfo.NewRateLimiter(reginfo.RateLimitConfig{
        Store: store,
    })
    if err != nil {
        return nil, err
    }
    return reginfo.NewClient(reginfo.WithRateLimiter(limiter)), nil
}
```

Set `GeoIPConfig.RateLimiter` to the same limiter to share download quotas. Alternatively, construct separate limiters with the same store and policies. Registry and download requests have separate service keys; local GeoIP reads never access the store. `Lookup`, `LookupIP`, and `ScanProviders` use the store for every network request, including WHOIS and HTTP redirects. Result-cache hits do not access it.

For explicit local storage, use `reginfo.NewMemoryRateLimitStore()`. Its zero value also works and supports concurrent access. The [example application](../example/reginfo/main.go) configures it explicitly. A nil `RateLimitConfig.Store` uses an independent memory store inside that limiter. The zero-value `RateLimiter` and `NewDefaultRateLimiter()` keep their existing behavior. `WithRateLimiter(nil)` and a nil `GeoIPConfig.RateLimiter` select the process-wide default; they do not disable limiting.

Default constructors perform no storage I/O. The caller owns an injected store and any connections it uses. Configure it before sharing the limiter and close it only after its users finish. The library includes no Redis client or Redis implementation.

## Store contract

The interface, shown from an importing package, is:

```go
type RateLimitStore interface {
    Acquire(ctx context.Context, key string, limits []reginfo.RateLimit) (time.Duration, error)
    Block(ctx context.Context, key string, until time.Time) (time.Time, error)
}
```

Both methods must honor context cancellation and support concurrent calls. A key is a normalized service such as `https://rdap.verisign.com` or `whois://whois.tcinet.ru`. HTTP and HTTPS share a key; ports, hostname case, and a trailing dot are ignored. Paths, domain names, and IP addresses are not part of the key.

`Acquire` atomically checks the cooldown and **all** supplied rolling windows:

- Return `0, nil` only after consuming one request in every window.
- When capacity is unavailable, return a positive duration until the next possible admission, without consuming a request or reserving future capacity. The limiter waits with the request context, then calls `Acquire` again; another caller may have consumed the available slot meanwhile.
- Count admissions in `(now - Interval, now]`. A window allows at most `Requests` admissions. Use the longest wait required by any window or the cooldown.
- Return an error when admission cannot be determined. Negative delays are invalid. Do not sleep until capacity becomes available inside the store; the limiter handles that wait.

The limiter passes a copy of the effective policy on each call, including the built-in service overrides. Every limiter sharing a key must use the same ordered policy. `MemoryRateLimitStore` rejects conflicting policies; custom implementations should also reject them. Coordinate policy changes rather than resetting active counters.

`Block` atomically stores the later of the existing cooldown and `until`, and returns that effective timestamp. It must not shorten a cooldown, clear request history, or consume a slot. It may be called before any admission. A successful response with a timestamp earlier than `until` is rejected by the limiter. Cooldowns and admissions must use the same shared state so that `Acquire` observes blocks written by any process.

Storage errors stop the affected operation and remain available through `errors.Is(err, reginfo.ErrRateLimitStore)` and `errors.Is(err, originalError)`. Domain and IP lookups do not try another RDAP endpoint or WHOIS after such an error. `ScanProviders` returns storage errors together with any collected provider results. No local fallback or automatic storage retry occurs on error.

If writing a cooldown after HTTP 429 fails, the response body is still closed and the returned error matches both `ErrRateLimited` and `ErrRateLimitStore`. `errors.As` still exposes `*RateLimitError`, but the cooldown could not be confirmed in shared storage. A later request consults the store again. Context cancellation while waiting does not reserve capacity; a successful admission remains counted even if cancellation or a network error follows it. An uncertain backend response can also leave an admission counted, so automatic retries inside a custom store need care.

## Implementing a Redis store

Keep the normalized service key inside an application-controlled namespace. Processes that consume the same provider quota must use the same namespace. For example, MaxMind download state should be shared by all instances using the same account; applications outside this store remain uncounted.

Implement admission and cooldown updates as atomic operations. Redis guarantees atomic execution of [Lua scripts](https://redis.io/docs/latest/develop/programmability/eval-intro/), which can combine the required checks and writes. Use a shared clock for admission calculations, such as Redis [`TIME`](https://redis.io/docs/latest/commands/time/), and return a relative wait duration. `Block` receives an absolute timestamp, so account for application/server clock differences when persisting it.

Retain admitted timestamps for at least the longest configured interval and retain a cooldown until it expires. Expiring or evicting active state would reset the quota. Give each admission a unique identity so simultaneous requests are counted separately. Preserve the policy alongside its state to detect incompatible configurations. Configure backend timeouts, persistence, and state retention according to the quota guarantees your application needs.

Use a compile-time assertion for your implementation:

```go
var _ reginfo.RateLimitStore = (*RedisRateLimitStore)(nil)
```

Verify it with concurrent callers, multiple rolling windows, two limiter instances, cooldown extensions, cancellation, and backend errors before deploying it. The library's tests cover these behaviors with memory storage and injected stores; they do not exercise a Redis deployment.
