# Client configuration

Reuse a `Client` to share registry discovery, cached results, and request limits across lookups. A client can query both domains and IP addresses and is safe to use from multiple goroutines.

[Back to README](../README.md#documentation)

## Client lifetime

```go
client := reginfo.NewClient()
domain, err := client.Lookup(ctx, "example.com")
```

`NewClient` performs no network I/O. The zero value of `Client` also works. Package-level `reginfo.Lookup` and `reginfo.LookupIP` use a shared default client.

IANA's DNS, IPv4, and IPv6 bootstrap files are loaded on demand and cached separately for the lifetime of the client. Concurrent lookups share an in-progress load of the same file. A failed load can be retried by a later lookup.

Default clients use the process-wide [rate limiter](rate-limits.md) but have their own [domain and IP result caches](cache.md). `WithRateLimiter`, `WithCache`, and `WithIPCache` allow explicit sharing. A `NewDefaultRateLimiter()` creates independent counters, so reuse that instance when several clients should share a quota.

`NewRateLimiter(RateLimitConfig{Store: myStore})` accepts a custom [RateLimitStore](rate-limit-store.md), including an application-provided Redis implementation. Sharing that store across limiters shares quotas and cooldowns while preserving the configured service policies.

`WithGeoIP(geo)` attaches an optional [GeoIP component](geoip.md). The caller loads its databases and closes it after all clients finish using it. GeoIP download settings belong to `GeoIPConfig`; the registry client's HTTP and limiter options do not replace them. Pass the same limiter explicitly to both configurations when sharing custom counters.

## HTTP transport and timeouts

All HTTP requests, including RDAP, IANA discovery, and GeoIP downloads, use [Resty v3](https://pkg.go.dev/resty.dev/v3@v3.0.0-rc.4), pinned to `resty.dev/v3 v3.0.0-rc.4`. Library requests disable automatic retries and use streaming response bodies. Registry requests set `Accept: application/rdap+json, application/json`; all library requests send `User-Agent: reginfo-go/1.0`. WHOIS uses its TCP protocol on port 43.

To customize HTTP behavior:

```go
httpClient := resty.New().
    SetTimeout(30 * time.Second).
    SetCookieJar(nil).
    SetHeader("X-Application", "my-app")
defer httpClient.Close()

client := reginfo.NewClient(reginfo.WithHTTPClient(httpClient))
```

Import `resty.dev/v3`. `WithHTTPClient` accepts `*resty.Client` directly; migrate existing `*http.Client` arguments to a Resty client. `WithHTTPClient(nil)` selects the default Resty client. `GeoIPConfig.HTTPClient` accepts the same type.

Configure the Resty client's transport, TLS settings, and redirect policies before passing it to `reginfo`. During construction, the library wraps the transport and redirect policy once. It uses the supplied Resty instance, preserving its headers, query parameters, middleware, and timeout. Request-specific headers, retries, and streaming settings do not overwrite the client's defaults. Response middleware must leave the streaming body available for the library to read.

The same configured Resty client can be shared by multiple registry clients and GeoIP components. Each request uses its component's rate limiter; sharing Resty does not merge independent limiters or apply a limiter twice. GeoIP's HTTPS and credential protections apply only to download requests. Direct requests made through the supplied Resty instance keep their usual behavior and do not use `reginfo`'s limiter. The caller owns the supplied client and closes it after its users finish.

| Operation                    | Default limit                                                       |
| ---------------------------- | ------------------------------------------------------------------- |
| HTTP request                 | 12 seconds, including rate-limit waiting, redirects, and body reads |
| WHOIS connection             | 5 seconds after rate-limit admission                                |
| WHOIS exchange               | 8 seconds after connecting, shortened by the context deadline       |
| IANA RDAP bootstrap response | 2 MiB                                                               |
| IANA TLD list                | 200 KiB                                                             |
| RDAP response                | 4 MiB                                                               |
| WHOIS response               | 2 MiB                                                               |

HTTP body limits apply after decompression. An oversized response returns an error instead of partial data.

Use a context to bound the complete lookup, since one lookup can contact several servers:

```go
ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
defer cancel()

info, err := client.LookupIP(ctx, "8.8.8.8")
```

For a supplied Resty client, `SetTimeout` controls each request, including the response body read. The configured value is preserved, including zero for no Resty timeout. If the same instance is used for both registration and GeoIP, that timeout applies to both. To wait longer for a quota, configure both the HTTP timeout and the overall context accordingly. WHOIS quota waiting happens before connecting; the network timeouts start after admission. See [rate-limit waits and HTTP 429](rate-limits.md#http-429-and-timeouts).

## Provider discovery

Ordinary lookups discover only the providers they need. To inspect domain registry coverage, use `ScanProviders`:

```go
providers, err := client.ScanProviders(ctx, 3)
if err != nil {
    return err
}
for _, provider := range providers {
    fmt.Println(provider.TLD, provider.Status, provider.RDAP, provider.WHOIS)
}
```

The worker count must be between 1 and 5. The method loads the IANA DNS bootstrap and TLD list, discovers WHOIS servers, and returns providers sorted by TLD. Each `Provider` includes `TLD`, `RDAP`, `WHOIS`, `Status`, and any discovery `Error`.

Scanning uses the client's limiter and does not use domain or IP result caches. IANA WHOIS requests share the default quota of one request per second regardless of worker count, so scanning every TLD can take substantial time. Pass a suitable context; on cancellation, the method may return partial results alongside the context error.
