# GeoIP

GeoIP adds geographic and autonomous-system information to IPv4 and IPv6 lookups using local MaxMind GeoLite2 databases. Once loaded, lookups need no network access. The component can run independently or enrich the registration data returned by `Client.LookupIP`.

[Back to README](../README.md#documentation)

## Load and query local databases

Place `GeoLite2-City.mmdb`, `GeoLite2-Country.mmdb`, and `GeoLite2-ASN.mmdb` in one directory. All three files are required, although an individual address may be present in only some databases. Existing flat directories, including files downloaded by `geoipupdate`, are supported.

```go
geo, err := reginfo.NewGeoIP(reginfo.GeoIPConfig{
    Directory: "./build/geoip",
})
if err != nil {
    return err
}
defer geo.Close()

if err := geo.LoadDatabases(ctx); err != nil {
    return err
}

location, err := geo.Lookup(ctx, "108.61.78.59")
if err != nil {
    return err
}
fmt.Println(location.Country, location.City, location.ASN)

client := reginfo.NewClient(reginfo.WithGeoIP(geo))
info, err := client.LookupIP(ctx, "108.61.78.59")
if err != nil {
    return err
}
fmt.Println(info.Country, info.GeoIP)
```

`NewGeoIP` validates configuration without opening files or making network requests. Use it to construct the component; an empty directory is rejected. `LoadDatabases` opens local files, checks their database types, and activates the complete set. `geo.Lookup` and `client.LookupGeoIP` perform the same local lookup and return `*GeoIPInfo`. `client.LookupIP` also queries RDAP/WHOIS and fills `IPInfo.GeoIP` (`geo_ip` in JSON). Domain lookups do not perform DNS resolution or geolocation.

Reuse one `GeoIP` instance across clients and goroutines. The caller owns it and must call `Close` after the clients finish using it. Closing is idempotent. Updates and loads are serialized within the instance; lookups keep using the previous set while a replacement is prepared. Reader replacement and close wait for active reads to finish.

`Metadata()` returns each active database's edition, build time, and IP version. Its build time is the database release timestamp, not the time it was downloaded.

The [GeoIP example](../example/geoip/main.go) looks up `108.61.78.59` using databases in `./build/geoip`. Run it from the repository root without arguments:

```sh
go run ./example/geoip
```

Edit the `directory` and `ip` constants in the example to use another database directory or IPv4/IPv6 address.

## Download and update databases

To download the three GeoLite2 editions, provide a MaxMind account ID and license key. The library does not read environment variables itself; the example reads `MAXMIND_ACCOUNT_ID` and `MAXMIND_LICENSE_KEY`.

```go
httpClient := resty.New().SetTimeout(5 * time.Minute).SetCookieJar(nil)
defer httpClient.Close()

geo, err := reginfo.NewGeoIP(reginfo.GeoIPConfig{
    HTTPClient:  httpClient,
    Directory:   "./build/geoip",
    AccountID:   os.Getenv("MAXMIND_ACCOUNT_ID"),
    LicenseKey:  os.Getenv("MAXMIND_LICENSE_KEY"),
    RateLimiter: reginfo.NewDefaultRateLimiter(),
})
if err != nil {
    return err
}
defer geo.Close()

ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
defer cancel()
if err := geo.UpdateDatabases(ctx); err != nil {
    return err
}
location, err := geo.Lookup(ctx, "108.61.78.59")
```

`UpdateDatabases` downloads and activates all three editions; calling `LoadDatabases` afterward is unnecessary. To download databases with the example, set `updateDatabases = true` in `example/geoip/main.go`, set the two environment variables in your shell, and run:

```sh
go run ./example/geoip
```

Set `updateDatabases` back to `false` for subsequent local lookups.

Downloads use Resty v3 and MaxMind's [direct-download API](https://dev.maxmind.com/geoip/updating-databases/#directly-downloading-databases): `GET https://download.maxmind.com/geoip/databases/<edition>/download?suffix=tar.gz` with HTTP Basic authentication. HTTPS redirects are followed; the account credentials are removed when the host changes. Redirects to HTTP are rejected. Automatic retries are disabled.

The default download client has a five-minute timeout, including quota waits, redirects, and body reads. `GeoIPConfig.HTTPClient` accepts `*resty.Client` from `resty.dev/v3`; its timeout, headers, query parameters, and middleware are preserved. Configure its transport and redirect policies before attaching it. The library wraps those settings once and applies HTTPS and credential protections to GeoIP requests. See [HTTP client configuration](client.md#http-transport-and-timeouts) for sharing and ownership. This setting is independent of the registry client's `WithHTTPClient` option, although both can receive the same Resty instance. The overall context bounds the network update; an operation queued behind another load or update checks its context once it acquires the update lock.

Archives are streamed into a staging directory with limits of 256 MiB downloaded, 512 MiB decompressed, and 256 MiB per archive entry. Only the expected regular `.mmdb` file is extracted to a fixed path. Duplicate entries, incomplete gzip streams, and invalid database types cause an error.

The active files are never overwritten. After all three new files open successfully, the library atomically replaces the directory's `CURRENT` pointer file and switches the active readers. Download or preparation failures preserve the old set both in memory and on disk. A successful update removes the previous managed generation. If cleanup fails after activation, the error explicitly says that the new databases are already active. Legacy flat files are left in place; once `CURRENT` exists, future loads follow it. Use only one GeoIP instance to update a given directory, including across processes, and do not edit active files in place. No background scheduler runs; call `UpdateDatabases` when your application decides to refresh. Every call downloads all three files; it does not check whether the release changed.

The default limiter allows **30 download requests per rolling 24 hours** to `download.maxmind.com`, spaced at least one second apart. A complete update uses three requests. MaxMind publishes a 30-download daily limit for GeoLite accounts and 1,000 for other accounts in its [download policy](https://support.maxmind.com/knowledge-base/articles/download-and-update-maxmind-databases). Local counters are per server and limiter, shared across accounts using that limiter; MaxMind counts across all applications using the account. Reuse the same limiter across instances, or configure a shared [RateLimitStore](rate-limit-store.md) to coordinate processes. Memory counters reset on restart. Downloads by programs outside the store remain uncounted. Local GeoIP lookups consume no quota. See [rate limits](rate-limits.md) for overrides and HTTP 429 handling.

## Result fields, cache, and errors

| Field                        | Meaning                                                                  |
| ---------------------------- | ------------------------------------------------------------------------ |
| `IP`                         | Normalized IPv4 or IPv6 address                                          |
| `Country`, `CountryName`     | Geographic country: ISO code and English name                            |
| `RegisteredCountry`          | ISP registration country reported by MaxMind                             |
| `City`                       | English city name                                                        |
| `Continent`, `ContinentCode` | English continent name and code                                          |
| `Subdivisions`               | Regions ordered from largest to smallest, each with `Name` and `ISOCode` |
| `PostalCode`                 | Postal code, potentially partial                                         |
| `Latitude`, `Longitude`      | Approximate coordinates; pointers distinguish absent values from zero    |
| `AccuracyRadius`             | Approximate accuracy radius in kilometers                                |
| `TimeZone`                   | IANA time-zone name                                                      |
| `ASN`, `ASNOrganization`     | Autonomous-system number and organization                                |
| `Network`, `ASNNetwork`      | Matching location and ASN prefixes; these may differ                     |
| `IsAnycast`                  | Whether the City or Country record marks the address as anycast          |

City data supplies location fields; Country data supplies country, registered-country, and continent information when missing from City. ASN data is independent. Missing fields remain empty. MaxMind describes these records in its [City/Country schema](https://dev.maxmind.com/geoip/docs/databases/city-and-country/) and [ASN schema](https://dev.maxmind.com/geoip/docs/databases/asn/). Geolocation is approximate. `IPInfo.Country` continues to mean the country in the registry record and is never replaced with `GeoIP.Country`.

The registry [IP cache](cache.md) stores registration data before GeoIP enrichment. Both fresh responses and cache hits use the currently loaded databases, so updates are visible immediately without clearing that cache. Local GeoIP results do not have a separate TTL cache. Clients sharing an `IPCache` may use different GeoIP instances. A cached `GeoIP` field from a custom cache is discarded and recomputed for the current client.

Use `errors.Is` to check errors:

| Error                                          | Behavior                                                                                 |
| ---------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `ErrInvalidIP`                                 | Input is not an accepted IPv4 or IPv6 address                                            |
| `ErrGeoIPNotLoaded`                            | Databases have not been loaded, or `Client.LookupGeoIP` has no configured component      |
| `ErrGeoIPNotFound`                             | No data for this address in any of the three databases                                   |
| `ErrGeoIPClosed`                               | The component has been closed                                                            |
| `ErrRateLimited`                               | A download returned HTTP 429; the responding host enters cooldown                        |
| `ErrRateLimitStore`                            | The quota store returned an error; a failed cooldown write also matches `ErrRateLimited` |
| `context.Canceled`, `context.DeadlineExceeded` | The operation was canceled or exceeded its deadline                                      |

`LookupIP` succeeds with `GeoIP == nil` when GeoIP is disabled or no local record exists. Other errors from a configured GeoIP component are returned; they do not silently omit enrichment. A successful registry response may already be cached when enrichment fails. `LookupGeoIP` works independently of registry availability and reports `ErrGeoIPNotFound` directly.
