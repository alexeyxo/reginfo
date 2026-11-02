# IP lookups

`LookupIP` retrieves the registration record for a network containing an IPv4 or IPv6 address. It returns an `IPInfo` with the allocated range, organization, published contacts, and registration dates. IP lookups use the same HTTP client and rate limiter as domain lookups.

[Back to README](../README.md#documentation)

## Usage and registry discovery

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

client := reginfo.NewClient()
info, err := client.LookupIP(ctx, "2001:4860:4860::8888")
if err != nil {
    return err
}
fmt.Println(info.IP, info.CIDRs, info.Organization)
```

`reginfo.LookupIP(ctx, ip)` uses the package's shared default client. Clients support concurrent domain and IP lookups, including the zero-value `Client`.

For a complete domain, IPv4, and IPv6 example, see [example/reginfo/main.go](../example/reginfo/main.go). Run `go run ./example/reginfo` or `make run` from the repository root. The application explicitly configures both result caches and a limiter, then writes one JSON result.

The client trims surrounding whitespace and normalizes IPv6 to a compressed, lowercase spelling. IPv4-mapped IPv6 is converted to IPv4. URLs, CIDR prefixes, addresses with ports, scoped IPv6 addresses such as `fe80::1%eth0`, and domain names return `ErrInvalidIP` before any network request. Syntactically valid private and special-purpose addresses are accepted, but a registration record may not be available.

The client loads the appropriate IANA bootstrap file for [IPv4](https://data.iana.org/rdap/ipv4.json) or [IPv6](https://data.iana.org/rdap/ipv6.json) and selects the longest matching network prefix, as specified by [RFC 9224, section 5](https://www.rfc-editor.org/rfc/rfc9224.html#section-5). The files are cached separately for the lifetime of the client. Concurrent requests for the same family share one load; a failed load can be retried later.

An RDAP request uses `GET <base>/ip/<address>` as defined by [RFC 9082, section 3.1.1](https://www.rfc-editor.org/rfc/rfc9082.html#section-3.1.1). Registry URL prefixes, such as ARIN's `/registry/`, are preserved. The client uses HTTPS endpoints from the bootstrap; Resty handles HTTP redirects. A response must have the `ip network` object class, the expected IP version, and an address range containing the queried IP.

When RDAP is unavailable or the response cannot be parsed, the client queries `whois.iana.org` and follows its `refer` or `whois` field to the registry. [IANA documents this referral mechanism](https://www.iana.org/help/whois). Subsequent `refer` and `ReferralServer` values are supported for WHOIS on port 43. Cycles are rejected, and the chain is limited to five queries.

For ARIN, the query is `n + <IP>` to request full network records, following [ARIN's CLI documentation](https://www.arin.net/resources/registry/whois/rws/cli/). RWhois referrals are not followed; in that case, the current WHOIS response is parsed.

## IPInfo fields

| Field                          | Meaning                                                                               |
| ------------------------------ | ------------------------------------------------------------------------------------- |
| `IP`, `IPVersion`              | Normalized address and version, `v4` or `v6`                                          |
| `StartAddress`, `EndAddress`   | Inclusive boundaries of the registered network                                        |
| `CIDRs`                        | Minimal list of CIDR prefixes covering the exact address range                        |
| `RegistryID`, `Name`, `Type`   | Registry identifier, network name, and allocation type                                |
| `ParentHandle`                 | Reference to the parent network from the registry record                              |
| `Country`, `Organization`      | Registration country and available organization name                                  |
| `GeoIP`                        | Optional local geolocation and ASN data; see [GeoIP](geoip.md)                        |
| `Contacts`                     | Published contacts: `Handle`, `Name`, `Organization`, `Roles`, `Emails`, and `Phones` |
| `CreatedAt`, `UpdatedAt`       | Creation and update timestamps; `nil` if missing or unrecognized                      |
| `Status`                       | Registry statuses, when available                                                     |
| `Registry`                     | Selected RDAP base URL and published or queried WHOIS server                          |
| `Source`, `CollectedAt`, `Raw` | `rdap` or `whois`, collection time, and original response                             |

RDAP parsing follows the [IP network object in RFC 9083, section 5.4](https://www.rfc-editor.org/rfc/rfc9083.html#section-5.4). The organization name comes from an entity with the `registrant` role. Nested entities are collected into a flat contact list; linked entities are not fetched separately. Unavailable fields remain empty, and additional attributes are preserved in `Raw`.

WHOIS parsing recognizes fields such as `NetRange`, `inetnum`, `inet6num`, and `CIDR`, along with organization names, contacts, and dates. If several network objects contain the queried IP, the most specific is selected. Unusual text formats may not be recognized.

The country describes the registration record, not the address's physical location. Configure `WithGeoIP` to add local geolocation and ASN information in a separate `GeoIP` field. The library does not infer BGP routes or domain names from an IP registration.

## Caching, limits, and errors

IP results use a separate `IPMemoryCache` with a five-minute TTL, a 256-entry capacity, and LRU eviction. Keys are individual normalized addresses, not returned network ranges. Dates and nested contact slices are copied so callers cannot alter cached values. Implement `IPCache` and pass `WithIPCache` to use your own store; `WithIPCache(nil)` disables IP result caching. See the [cache contract](cache.md#custom-implementations).

The limiter supplied through `WithRateLimiter`, or the shared default limiter, applies to every network request. IP RDAP and WHOIS services use the local default of one request per second per server. IPv4 and IPv6 share a server's quota; cache hits consume no quota. These are library settings, not guaranteed quotas from every provider. See [rate limits by service](rate-limits.md#limits-by-service).

HTTP 429 returns `ErrRateLimited` and respects `Retry-After`. The current request is not retried, and lookup does not move to another RDAP endpoint or WHOIS. Context cancellation and deadlines remain detectable with `errors.Is(err, context.Canceled)` and `errors.Is(err, context.DeadlineExceeded)`, including during quota waits and response reads.

A custom [RateLimitStore](rate-limit-store.md) can coordinate counters and cooldowns across processes. Storage failures return `ErrRateLimitStore` and also stop endpoint and WHOIS fallback.

If both RDAP and WHOIS fail, the error retains the causes from both stages. Failed lookups and missing records are not cached.

With `WithGeoIP`, each successful registration result is enriched from the active local databases, including cache hits. No GeoIP match leaves `GeoIP` nil. Other errors from the configured component are returned, although the successful registration may already be cached. For a local lookup without RDAP/WHOIS, use `client.LookupGeoIP`; see [GeoIP errors and caching](geoip.md#result-fields-cache-and-errors).
