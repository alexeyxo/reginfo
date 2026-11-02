# Domain lookups

`Lookup` retrieves a domain's registration record and returns a `DomainInfo`. It discovers the registry through IANA, prefers RDAP, and uses WHOIS when RDAP is unavailable or its response cannot be parsed. The same interface supports ASCII, Unicode, and punycode names, including `.ru` and `.рф`.

[Back to README](../README.md#documentation)

## Input and normalization

```go
client := reginfo.NewClient()
info, err := client.Lookup(ctx, "www.ДОМ.РФ.")
if err != nil {
    return err
}
fmt.Println(info.Domain)      // xn--d1aqf.xn--p1ai
fmt.Println(info.UnicodeName) // дом.рф
```

The client trims surrounding whitespace, applies IDNA lookup mapping, lowercases the ASCII name, and removes a trailing dot. It then validates the name against the ICANN portion of the Public Suffix List and selects the registrable domain (eTLD+1).

| Input                     | Queried domain       |
| ------------------------- | -------------------- |
| `www.example.co.uk`       | `example.co.uk`      |
| `дом.рф`                  | `xn--d1aqf.xn--p1ai` |
| `xn--d1aqf.xn--p1ai`      | `xn--d1aqf.xn--p1ai` |
| `github.io`               | `github.io`          |
| `user.github.io`          | `github.io`          |
| `www.user.blogspot.com`   | `blogspot.com`       |
| `bucket.s3.amazonaws.com` | `amazonaws.com`      |

PRIVATE rules are ignored when determining registration boundaries. This returns the platform owner's domain registration for names such as `user.github.io`. ICANN wildcard rules and exceptions still apply. The distinction uses [PublicSuffix from golang.org/x/net/publicsuffix](https://pkg.go.dev/golang.org/x/net/publicsuffix#PublicSuffix); its snapshot updates with the `golang.org/x/net` dependency.

URLs, IP addresses, invalid labels, unknown suffixes, and bare ICANN suffixes such as `com` or `co.uk` return `ErrInvalidDomain` before any network request. Valid syntax does not guarantee that a registration exists or that its registry is reachable.

## DomainInfo fields

| Field                                 | Meaning                                                    |
| ------------------------------------- | ---------------------------------------------------------- |
| `Domain`                              | Normalized registrable domain in ASCII                     |
| `UnicodeName`                         | Readable Unicode form of the normalized domain             |
| `RegistryID`                          | Registry identifier, when available                        |
| `CreatedAt`, `UpdatedAt`, `ExpiresAt` | Registration, update, and expiration timestamps            |
| `AgeDays`                             | Age in days calculated from the creation date              |
| `Registrar`                           | Registrar name, IANA ID, URL, and published abuse contacts |
| `Registrant`                          | Published organization, country, and state                 |
| `NameServers`                         | Normalized, deduplicated nameservers                       |
| `DNSSEC`                              | Signing information, when reported by the registry         |
| `Status`                              | Registration statuses                                      |
| `Registry`                            | Selected RDAP base URL and discovered WHOIS server         |
| `Source`                              | `rdap` or `whois`                                          |
| `CollectedAt`, `Raw`                  | Collection time and original registry response             |

The types have JSON tags. Missing dates and age are `nil`; other unavailable fields remain empty. Registries differ in which fields they disclose. `Raw` preserves additional information that the structured fields do not expose.

`UnicodeName` is filled by the library after every successful RDAP or WHOIS lookup, even when the registry omits it. ASCII domains have identical `Domain` and `UnicodeName` values. Both fields identify the normalized registrable domain: original capitalization and subdomains are not retained.

For `дом.рф` and `xn--d1aqf.xn--p1ai`, the name fields are:

```json
{
  "domain": "xn--d1aqf.xn--p1ai",
  "unicode_name": "дом.рф"
}
```

An RDAP success does not trigger a separate WHOIS discovery request. `Registry.WHOISServer` is filled only when the client has already discovered it. WHOIS parsing recognizes known text fields; unusual registry formats may not be recognized.

## Errors and cached results

Use `errors.Is(err, reginfo.ErrInvalidDomain)` to detect invalid input. Context cancellation and deadlines remain detectable through `context.Canceled` and `context.DeadlineExceeded`.

A successful WHOIS fallback returns its result without the preceding RDAP error. If neither method succeeds, the returned error retains the failure causes. HTTP 429 stops the lookup immediately: the client does not retry, try another RDAP endpoint, or fall back to WHOIS. See [rate-limit behavior](rate-limits.md#http-429-and-timeouts).

Successful results are cached under the normalized ASCII domain. Unicode, punycode, and subdomain spellings of the same registrable domain share a cache entry. The default TTL is five minutes, with at most 256 domain entries per client. `CollectedAt` and `AgeDays` remain part of the stored snapshot and are not recalculated on a cache hit. See [Caching](cache.md) for custom implementations and expiration behavior.
