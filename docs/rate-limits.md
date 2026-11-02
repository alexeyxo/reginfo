# Rate limits

The rate limiter spaces out network requests and shares quotas across goroutines and clients. It runs before every HTTP request, including redirects, and before opening a WHOIS connection. When a quota is exhausted, the request waits for capacity or returns a context cancellation or timeout error. A request canceled while waiting does not consume a slot. State is stored in memory by default; a custom [RateLimitStore](rate-limit-store.md) can coordinate quotas across processes.

[Back to README](../README.md#documentation)

## Limits by service

A quota belongs to a protocol and server hostname. All paths, domain zones, and IP families using that server share it. HTTP and HTTPS share a counter; WHOIS has a separate counter. Ports, hostname case, and a trailing dot do not create separate quotas.

Provider references and server mappings below were reviewed on **October 21, 2026**. Servers are discovered dynamically through IANA, so the policy follows the actual server hostname. Domain endpoints are listed in the [IANA DNS bootstrap](https://data.iana.org/rdap/dns.json); the reviewed snapshot was published on September 30, 2026.

| Domains or service                                                             | Published policy or reference                                                                                                                                                                    | Library limit                                                        |
| ------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------- |
| `.ru`, `.рф`: WHOIS `whois.tcinet.ru`                                          | 120 queries/minute per IP, [TCI, section 3.1](https://www.tcinet.ru/documents/whois_ru_rf.pdf)                                                                                                   | 120 per rolling minute; at least 500 ms between requests             |
| `.su`: WHOIS `whois.tcinet.ru`                                                 | The [.SU guide, version 2.0, section 4.1](https://www.tcinet.ru/documents/whois_su.pdf) refers to a separate limits policy without a numeric quota                                               | Shares the same server quota: 120/minute and at least 500 ms spacing |
| `.uk`, including `.co.uk` and `.org.uk`: WHOIS `whois.nic.uk`, when discovered | 5 queries/second and 1,000 per rolling 24 hours **per user**, [Nominet WHOIS AUP](https://registrars.nominet.uk/uk-namespace/registration-and-domain-management/acceptable-use-policy/)          | 1,000 per rolling 24 hours; at least 200 ms spacing                  |
| `.uk`: public RDAP `rdap.nominet.uk`                                           | A numeric quota for this public endpoint has not been confirmed; WHOIS quotas do not apply                                                                                                       | 1 request/second, local policy                                       |
| `.com`, `.net`: RDAP `rdap.verisign.com`                                       | No fixed numeric quota in the reviewed [Verisign RDAP terms](https://www.verisign.com/legal-center/rdap-terms/)                                                                                  | Shared 1 request/second, local policy                                |
| `.app`, `.dev`, and other zones on `pubapi.registry.google`                    | No fixed numeric quota in the reviewed [Google Registry RDAP terms](https://www.registry.google/policies/rdap-terms/)                                                                            | Shared 1 request/second, local policy                                |
| `.org`: RDAP `rdap.publicinterestregistry.org`                                 | No separate service policy configured in the library                                                                                                                                             | 1 request/second, local policy                                       |
| `.xyz` and other zones on `rdap.centralnic.com`                                | No separate service policy configured in the library                                                                                                                                             | Shared 1 request/second, local policy                                |
| IANA WHOIS `whois.iana.org`                                                    | No numeric quota in the reviewed [IANA WHOIS instructions](https://www.iana.org/help/whois)                                                                                                      | 1 request/second, local policy                                       |
| GeoLite2 downloads: `download.maxmind.com`                                     | 30 direct downloads/24 hours per GeoLite account; 1,000 for other accounts, [MaxMind download policy](https://support.maxmind.com/knowledge-base/articles/download-and-update-maxmind-databases) | 30 per rolling 24 hours; at least 1 second spacing                   |
| Other RDAP/WHOIS services and HTTP `data.iana.org`                             | Provider-specific rules may apply; no separate policy configured                                                                                                                                 | 1 request/second per service, local policy                           |

IANA lists `whois.tcinet.ru` for [.ru](https://www.iana.org/domains/root/db/ru.html), [.рф](https://www.iana.org/domains/root/db/xn--p1ai.html), and [.su](https://www.iana.org/domains/root/db/su.html). These zones were absent from the reviewed DNS RDAP bootstrap, so lookups use WHOIS. For example, 80 `.ru` requests and 40 `.рф` requests consume the same minute's quota; switching to `.su` does not reset it. Unicode and punycode spellings also share a quota.

The `.su` value is the library's policy for the shared TCI server, not a confirmed numeric quota from the October 5, 2026 edition of its guide. Nominet's WHOIS limits are per user, not per IP address. The library also spaces those WHOIS requests at least 200 ms apart.

For `.uk`, the client tries RDAP first. Its local one-request-per-second setting is independent of the WHOIS daily quota. The reviewed [IANA .uk record](https://www.iana.org/domains/root/db/uk.html) lists RDAP without a WHOIS server; the `whois.nic.uk` policy applies only if that server is discovered. Nominet's published one-response-per-second limit for its [Credentialled RDAP Testbed](https://registrars.nominet.uk/registry/dot-uk/credentialled-rdap/) concerns an authenticated test service, not the public endpoint used here.

IP registry selection uses the IANA [IPv4](https://data.iana.org/rdap/ipv4.json) and [IPv6](https://data.iana.org/rdap/ipv6.json) bootstrap files:

| Registry | RDAP endpoint                     | Library limit    |
| -------- | --------------------------------- | ---------------- |
| ARIN     | `https://rdap.arin.net/registry/` | 1 request/second |
| RIPE NCC | `https://rdap.db.ripe.net/`       | 1 request/second |
| APNIC    | `https://rdap.apnic.net/`         | 1 request/second |
| LACNIC   | `https://rdap.lacnic.net/rdap/`   | 1 request/second |
| AFRINIC  | `https://rdap.afrinic.net/rdap/`  | 1 request/second |

These are local settings, not confirmed numeric quotas for all five RDAP services. Discovered IP WHOIS servers also use the general one-request-per-second policy. IPv4 and IPv6 share a server's counter. Fetches of `dns.json`, `ipv4.json`, and `ipv6.json` share the `data.iana.org` quota; domain and IP discovery share the `whois.iana.org` quota.

[GeoIP](geoip.md) reads are local and consume no request quota. `UpdateDatabases` downloads three GeoLite2 editions, using three slots from the MaxMind download quota. The local limit is shared per hostname and limiter across accounts; MaxMind's daily limit is per account across applications. Storage redirects use that host's general one-request-per-second policy. Each update downloads every edition; the library does not issue HEAD requests to check for a new release. An exhausted local quota waits for a slot until the HTTP timeout or context ends.

Some quotas depend on response content. The [RIPE Database AUP](https://www.ripe.net/manage-ips-and-asns/db/support/documentation/ripe-database-acceptable-use-policy/) specifies 1,000 personal data sets per 24 hours. This counts returned data sets rather than requests. The limiter counts network requests and does not implement that content-based quota; one request per second alone does not guarantee compliance.

All quota windows are rolling. A daily slot becomes available 24 hours after that request, not at midnight. Requests count once admitted for sending, including requests that subsequently fail on the network. Requests already in flight are not canceled when a concurrent request receives HTTP 429.

Quotas count network calls, not completed lookups. A cold lookup may fetch a bootstrap, query IANA WHOIS, and contact multiple registry services. A [result cache hit](cache.md) consumes no quota. `ScanProviders` shares the IANA WHOIS quota across all workers. Provider-specific restrictions may exceed these local settings; configure custom rules for the services you use.

## Configuration

`reginfo.Lookup`, `reginfo.LookupIP`, `NewClient()`, and the zero-value `Client` use the process-wide default limiter. To explicitly create a separate limiter, as in the [example application](../example/reginfo/main.go):

```go
limiter := reginfo.NewDefaultRateLimiter()
client := reginfo.NewClient(reginfo.WithRateLimiter(limiter))
```

`NewDefaultRateLimiter()` takes no arguments and returns no error. It creates independent counters with the policies listed above. A supplied limiter applies to `Lookup`, `LookupIP`, and `ScanProviders`.

`NewGeoIP` also uses the process-wide limiter unless `GeoIPConfig.RateLimiter` is supplied. To share custom counters with a registry client, pass the same instance in `GeoIPConfig.RateLimiter` and `WithRateLimiter`.

To customize policies and share counters between clients:

```go
limiter, err := reginfo.NewRateLimiter(reginfo.RateLimitConfig{
    HTTP: []reginfo.RateLimit{
        {Requests: 1, Interval: 2 * time.Second},
    },
    Servers: map[string][]reginfo.RateLimit{
        "whois://whois.tcinet.ru": {
            {Requests: 1, Interval: time.Second},
            {Requests: 60, Interval: time.Minute},
        },
    },
})
if err != nil {
    return err
}

client := reginfo.NewClient(reginfo.WithRateLimiter(limiter))
anotherClient := reginfo.NewClient(reginfo.WithRateLimiter(limiter))
```

`HTTP` and `WHOIS` configure services without a specific policy. Empty lists keep the defaults. `Servers` replaces a service's policy; its key is an `https://`, `http://`, or `whois://` URL containing the hostname and no path. All limits for a service apply simultaneously. Request counts and intervals must be positive. The constructor copies policy slices and maps and retains the supplied `Store` instance.

Without a custom store, each call to `NewDefaultRateLimiter()` or `NewRateLimiter(config)` creates independent memory counters that reset on restart. Reuse one limiter across clients, or give separate limiters the same `RateLimitConfig.Store`. `NewMemoryRateLimitStore()` provides explicit local storage; implement `RateLimitStore` to coordinate processes through Redis or another backend. See [rate-limit storage](rate-limit-store.md) for configuration, atomic admission, and cooldown requirements. Requests made by programs outside that store remain uncounted.

## HTTP 429 and timeouts

On HTTP 429, the client returns `reginfo.ErrRateLimited` and puts the responding server on cooldown. `Retry-After` accepts either a number of seconds or an HTTP date, as described in [RFC 7480, section 5.5](https://datatracker.ietf.org/doc/html/rfc7480#section-5.5). A missing or invalid header uses a one-minute cooldown. A shorter new cooldown does not shorten an existing one.

```go
info, err := client.Lookup(ctx, "example.com")
if errors.Is(err, reginfo.ErrRateLimited) {
    var limited *reginfo.RateLimitError
    if errors.As(err, &limited) {
        fmt.Println(limited.Server, limited.RetryAt)
    }
    return err
}
if err != nil {
    return err
}
fmt.Println(info.Domain)
```

The current lookup is not retried and does not continue to another RDAP endpoint or WHOIS. A subsequent network request using the same limiter waits for both the cooldown and local quota. This applies to the server that returned 429, including a redirect target.

A custom store shares those cooldowns across all participating limiters. Storage errors return `ErrRateLimitStore` and stop lookup fallback. If saving a cooldown fails, the error matches both `ErrRateLimited` and `ErrRateLimitStore`; the server's requested cooldown is still available in `RateLimitError`.

Context cancellation interrupts waits. The default Resty client limits the entire HTTP request to 12 seconds, including quota waiting, redirects, and body reads. `WithHTTPClient` accepts `*resty.Client` and preserves its `SetTimeout` setting. To wait longer, configure an appropriate HTTP timeout and an overall context deadline.

GeoIP downloads instead default to five minutes per HTTP request; customize that timeout with `GeoIPConfig.HTTPClient`. The same HTTP 429 handling and cooldown apply. Failed downloads preserve the active database set.

WHOIS waits for a quota before connecting; its five-second connection timeout and eight-second exchange deadline start after quota admission. Textual WHOIS limit messages have no uniform format and are not automatically recognized as HTTP 429 equivalents. See [Client configuration](client.md#http-transport-and-timeouts) for transport details.
