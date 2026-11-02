package reginfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
	"resty.dev/v3"
)

const (
	bootstrapURL = "https://data.iana.org/rdap/dns.json"
	tldListURL   = "https://data.iana.org/TLD/tlds-alpha-by-domain.txt"
	sourceRDAP   = "rdap"
	sourceWHOIS  = "whois"
)

var ErrInvalidDomain = errors.New("invalid domain")

var defaultClient = NewClient()

type Client struct {
	once              sync.Once
	http              *resty.Client
	rateLimiter       *RateLimiter
	cache             Cache
	cacheConfigured   bool
	ipCache           IPCache
	ipCacheConfigured bool
	geoIP             *GeoIP
	ipv4Bootstrap     ipBootstrap
	ipv6Bootstrap     ipBootstrap
	dialContext       func(context.Context, string, string) (net.Conn, error)
	mu                sync.Mutex
	providers         map[string]Provider
	bootstrapLoaded   bool
	bootstrapLoading  chan struct{}
}

type Option func(*Client)

func WithHTTPClient(h *resty.Client) Option {
	return func(c *Client) { c.http = h }
}

func NewClient(options ...Option) *Client {
	c := &Client{}
	for _, option := range options {
		option(c)
	}
	c.init()
	return c
}

func (c *Client) init() {
	c.once.Do(func() {
		c.http = prepareHTTPClient(c.http, 12*time.Second)
		if c.rateLimiter == nil {
			c.rateLimiter = &sharedRateLimiter
		}
		if !c.cacheConfigured {
			c.cache = NewDefaultMemoryCache()
		}
		if !c.ipCacheConfigured {
			c.ipCache = NewDefaultIPMemoryCache()
		}
		c.dialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
		c.providers = make(map[string]Provider)
	})
}

func Lookup(ctx context.Context, domain string) (*DomainInfo, error) {
	return defaultClient.Lookup(ctx, domain)
}

func (c *Client) Lookup(ctx context.Context, domain string) (*DomainInfo, error) {
	domain, tld, err := normalize(domain)
	if err != nil {
		return nil, err
	}
	unicodeName, err := domainProfile.ToUnicode(domain)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDomain, err)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	c.init()
	if c.cache != nil {
		info, cacheErr := c.cache.Get(ctx, domain)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if cacheErr == nil && info != nil {
			info = cloneDomainInfo(info)
			info.UnicodeName = unicodeName
			return info, nil
		}
	}
	info, err := c.lookup(ctx, domain, tld)
	if err != nil {
		return nil, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	info.UnicodeName = unicodeName
	if c.cache != nil {
		_ = c.cache.Set(ctx, domain, cloneDomainInfo(info)) //nolint:errcheck // A cache error does not discard a successful registry response.
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
	}
	return info, nil
}

func (c *Client) lookup(ctx context.Context, domain, tld string) (*DomainInfo, error) {
	var failures []error
	if err := c.bootstrap(ctx); err != nil {
		if isRateLimitError(err) {
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
		failures = append(failures, fmt.Errorf("bootstrap: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := c.cachedProvider(tld)
	if len(p.RDAP) > 0 {
		info, err := c.lookupRDAP(ctx, p, domain)
		if err == nil {
			return info, nil
		}
		if isRateLimitError(err) {
			return nil, fmt.Errorf("RDAP: %w", err)
		}
		failures = append(failures, fmt.Errorf("RDAP: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, discoveryErr := c.provider(ctx, tld)
	if isRateLimitError(discoveryErr) {
		return nil, fmt.Errorf("WHOIS discovery: %w", discoveryErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Error != "" {
		failures = append(failures, fmt.Errorf("WHOIS discovery: %s", p.Error))
	}
	info, err := c.lookupWHOIS(ctx, p, domain, tld)
	if err == nil {
		return info, nil
	}
	failures = append(failures, fmt.Errorf("WHOIS: %w", err))
	return nil, fmt.Errorf("lookup %s: %w", domain, errors.Join(failures...))
}

func (c *Client) get(ctx context.Context, endpoint string, maxBytes int64) ([]byte, error) {
	response, err := newHTTPRequest(ctx, c.http, c.rateLimiter, false).
		SetHeader("Accept", "application/rdap+json, application/json").Get(endpoint)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if response.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", endpoint, response.StatusCode())
	}
	return readLimited(response.Body, maxBytes)
}

func readLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBytes)
	}
	return b, nil
}

func (c *Client) bootstrap(ctx context.Context) error {
	for {
		c.mu.Lock()
		if c.bootstrapLoaded {
			c.mu.Unlock()
			return nil
		}
		if pending := c.bootstrapLoading; pending != nil {
			c.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		c.bootstrapLoading = make(chan struct{})
		c.mu.Unlock()
		break
	}

	b, err := c.get(ctx, bootstrapURL, 2<<20)
	var response bootstrapResponse
	if err == nil {
		err = json.Unmarshal(b, &response)
	}
	if err == nil && response.Services == nil {
		err = errors.New("bootstrap response missing services")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		for _, service := range response.Services {
			if len(service) != 2 {
				continue
			}
			for _, tld := range service[0] {
				tld = strings.ToLower(tld)
				p := c.providers[tld]
				p.TLD = tld
				p.RDAP = append(p.RDAP, service[1]...)
				if len(p.RDAP) > 0 {
					p.Status = sourceRDAP
				}
				c.providers[tld] = p
			}
		}
		c.bootstrapLoaded = true
	}
	close(c.bootstrapLoading)
	c.bootstrapLoading = nil
	return err
}

func (c *Client) cachedProvider(tld string) Provider {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.providers[tld]
	p.TLD = tld
	p.RDAP = slices.Clone(p.RDAP)
	return p
}

func (c *Client) provider(ctx context.Context, tld string) (Provider, error) {
	p := c.cachedProvider(tld)
	if p.WHOIS != "" || p.Status == "unavailable" {
		return p, nil
	}
	body, err := c.whois(ctx, "whois.iana.org", tld)
	if err != nil {
		p.Error = err.Error()
		if len(p.RDAP) == 0 {
			p.Status = "discovery_error"
		}
		return p, err
	}
	host := strings.TrimSuffix(strings.ToLower(field(body, "whois", "refer")), ".")
	c.mu.Lock()
	defer c.mu.Unlock()
	p = c.providers[tld]
	p.TLD = tld
	if host != "" && !strings.ContainsAny(host, " /:@\r\n\t") && net.ParseIP(host) == nil {
		p.WHOIS = host
	}
	switch {
	case len(p.RDAP) > 0:
		p.Status = sourceRDAP
	case p.WHOIS != "":
		p.Status = sourceWHOIS
	default:
		p.Status = "unavailable"
	}
	c.providers[tld] = p
	p.RDAP = slices.Clone(p.RDAP)
	return p, nil
}

var domainProfile = idna.New(idna.MapForLookup(), idna.BidiRule())

func normalize(raw string) (string, string, error) {
	domain, err := domainProfile.ToASCII(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrInvalidDomain, err)
	}
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	if len(domain) > 253 || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil {
		return "", "", ErrInvalidDomain
	}
	labels := strings.Split(domain, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", "", ErrInvalidDomain
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", "", ErrInvalidDomain
			}
		}
	}
	suffix, icann := publicsuffix.PublicSuffix(domain)
	for candidate := domain; !icann && strings.Contains(suffix, "."); {
		_, candidate, _ = strings.Cut(candidate, ".")
		suffix, icann = publicsuffix.PublicSuffix(candidate)
	}
	if !icann {
		return "", "", fmt.Errorf("%w: unknown public suffix %q", ErrInvalidDomain, suffix)
	}
	if len(domain) <= len(suffix) {
		return "", "", fmt.Errorf("%w: public suffix %q has no registrable label", ErrInvalidDomain, suffix)
	}
	prefix := domain[:len(domain)-len(suffix)-1]
	registered := domain[strings.LastIndexByte(prefix, '.')+1:]
	return registered, labels[len(labels)-1], nil
}
