# reginfo

A Go library for domain and IP registration lookups. Pass a domain name, IPv4 address, or IPv6 address and receive structured registration data. The client discovers registry servers through IANA, queries RDAP, and falls back to WHOIS when needed.

- Domain names in Unicode or punycode, with ICANN public suffix validation.
- IPv4 and IPv6 network information, including address ranges, CIDRs, organizations, and published contacts.
- Optional local GeoIP lookups with City, Country, and ASN data, plus GeoLite2 database downloads and reloads.
- Built-in rate limiting with replaceable storage, plus separate, replaceable caches for domains and IP addresses.
- Context cancellation, timeouts, and JSON-ready result types with the original registry response.

## Documentation

- [Domain lookups](docs/domains.md) — normalization, public suffix rules, and `DomainInfo` fields.
- [IP lookups](docs/ip.md) — registry discovery, IPv4/IPv6 handling, and `IPInfo` fields.
- [GeoIP](docs/geoip.md) — local geolocation, ASN information, database loading, and MaxMind downloads.
- [Client configuration](docs/client.md) — HTTP transport, timeouts, and provider discovery.
- [Rate limits](docs/rate-limits.md) — service policies, limits by registry, custom rules, and HTTP 429 handling.
- [Rate-limit storage](docs/rate-limit-store.md) — memory storage and the interface for custom stores such as Redis.
- [Caching](docs/cache.md) — TTL, LRU eviction, and custom `Cache` / `IPCache` implementations.
- [Example application](example/reginfo/main.go) — a complete domain, IPv4, and IPv6 lookup example.

## Requirements and installation

Requires **Go 1.27 or later**.

Install the module:

```sh
go get github.com/alexeyxo/reginfo
```

## Quick start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/alexeyxo/reginfo"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    client := reginfo.NewClient()

    domain, err := client.Lookup(ctx, "github.io")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(domain.Domain, domain.UnicodeName, domain.Registrar.Name)

    ip, err := client.LookupIP(ctx, "8.8.8.8")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(ip.IP, ip.CIDRs, ip.Organization)
}
```

Use `Lookup` for domain names and `LookupIP` for IPv4 or IPv6. Package-level `reginfo.Lookup` and `reginfo.LookupIP` functions are also available; they share a default client. Clients are safe to use from multiple goroutines.

Domain lookups return `*reginfo.DomainInfo`; IP lookups return `*reginfo.IPInfo`. Both include `Registry`, `Source`, `CollectedAt`, and `Raw` to identify the registry, retrieval method, collection time, and original response. Fields that a registry does not disclose remain empty; missing dates are `nil`.

For local geolocation, construct a `GeoIP` component, call `LoadDatabases` or `UpdateDatabases`, and use `geo.Lookup(ctx, ip)`. Attach it with `WithGeoIP(geo)` to fill `IPInfo.GeoIP` during registration lookups, or call `client.LookupGeoIP` for local data alone. See the [GeoIP guide](docs/geoip.md) and [runnable example](example/geoip/main.go). GeoIP is disabled by default.

For Unicode domains, `Domain` contains the ASCII name and `UnicodeName` contains the readable name. For example, `дом.рф` and `xn--d1aqf.xn--p1ai` resolve to the same registration. Subdomains are reduced to their registrable domain: `www.example.co.uk` becomes `example.co.uk`. PRIVATE public suffix entries are ignored, so `github.io` is accepted and `user.github.io` resolves to `github.io`. See [domain normalization](docs/domains.md#input-and-normalization).

## Defaults and configuration

| Setting              | Default                                                                       |
| -------------------- | ----------------------------------------------------------------------------- |
| Rate limiter         | Shared across default clients in the process                                  |
| General request rate | 1 request/second per server; specific policies for TCI, Nominet, and MaxMind  |
| Domain cache         | Per client, 5-minute TTL, 256 entries, LRU eviction                           |
| IP cache             | Separate per-client cache, 5-minute TTL, 256 entries, LRU eviction            |
| HTTP timeout         | 12 seconds per request, including rate-limit waits                            |
| WHOIS timeouts       | 5 seconds to connect; 8 seconds for the exchange after quota admission        |
| GeoIP                | Disabled until configured; local reads have no network quota                  |

The general request rate is a local policy, not a guaranteed quota published by every provider. See [documented service limits](docs/rate-limits.md#limits-by-service) before configuring higher rates.

To explicitly share a limiter or caches between clients:

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

`WithCache(nil)` disables domain caching; `WithIPCache(nil)` disables IP caching. Custom cache interfaces, TTLs, and capacities are described in [Caching](docs/cache.md). Both `WithHTTPClient` and `GeoIPConfig.HTTPClient` accept `*resty.Client` from `resty.dev/v3`, for example `resty.New().SetTimeout(30 * time.Second)`. See [Client configuration](docs/client.md) for transport settings, sharing, and ownership.

To replace the limiter's storage, pass your `RateLimitStore` implementation in `NewRateLimiter(RateLimitConfig{Store: myStore})`. Memory storage remains the default. A custom Redis store can share counters and HTTP 429 cooldowns across processes while retaining the service policies. The same limiter can be used with `WithRateLimiter` and `GeoIPConfig.RateLimiter`. See [rate-limit storage](docs/rate-limit-store.md) for the contract and setup example.

Invalid inputs return `ErrInvalidDomain` or `ErrInvalidIP` before any network request. Use `errors.Is` to check these errors, `ErrRateLimited`, `ErrRateLimitStore`, `context.Canceled`, and `context.DeadlineExceeded`. An HTTP 429 stops the current lookup without a retry or WHOIS fallback; subsequent requests through the same store respect `Retry-After`. A storage error also stops lookup fallback; a failed cooldown write preserves both error types. See [HTTP 429 and timeouts](docs/rate-limits.md#http-429-and-timeouts).

## Run the example

From the repository root:

```sh
go run ./example/reginfo
```

`make run` runs the same application. No arguments are required.

The [example](example/reginfo/main.go) sequentially looks up `github.io`, `108.61.78.59`, and `2001:4860:4860::8888` using one client, explicit domain and IP caches, and a shared limiter. A 60-second context bounds the entire run. Edit the values in `example/reginfo/main.go` to query other names or addresses.

The application writes one formatted JSON object with `domain`, `ipv4`, and `ipv6` results to stdout, including their raw responses. On error, it writes the cause to stderr and exits with status 1. Running the example requires access to IANA and registry services.

## Development

```sh
make install-linter
make check
```

`make check` runs linting, table-driven tests with the race detector, and a build of the example. The binary is written to `build/reginfo`; coverage is written to `build/coverage.out`. Automated tests use local HTTP/TCP fixtures and generated MMDB files; they do not depend on public registries, MaxMind credentials, or downloaded databases.

Individual commands are available as `make lint`, `make test` (or `make tests`), `make build`, and `make fmt`. Run `make help` for the command list. HTTP requests use the pinned [Resty v3](https://pkg.go.dev/resty.dev/v3@v3.0.0-rc.4) dependency.

[GitHub Actions CI](.github/workflows/ci.yml) runs `make test` and `make lint` as separate jobs on every push and pull request, and supports manual runs. It uses the Go version from `go.mod` and the golangci-lint version pinned in `Makefile`.
