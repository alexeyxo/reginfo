package reginfo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
	schemeWHOIS = "whois"
)

var ErrRateLimited = errors.New("server rate limit reached")

var ErrRateLimitStore = errors.New("rate limit store error")

type RateLimitError struct {
	Server  string
	RetryAt time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s: %v (HTTP 429); retry at %s", e.Server, ErrRateLimited, e.RetryAt.UTC().Format(time.RFC3339))
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

type RateLimit struct {
	Requests int
	Interval time.Duration
}

type RateLimitConfig struct {
	HTTP    []RateLimit
	WHOIS   []RateLimit
	Servers map[string][]RateLimit
	Store   RateLimitStore
}

type RateLimiter struct {
	http    []RateLimit
	whois   []RateLimit
	servers map[string][]RateLimit
	store   RateLimitStore
	memory  MemoryRateLimitStore
}

var sharedRateLimiter RateLimiter

func NewDefaultRateLimiter() *RateLimiter {
	return &RateLimiter{}
}

func NewRateLimiter(config RateLimitConfig) (*RateLimiter, error) {
	if err := validateRateLimits(config.HTTP); err != nil {
		return nil, fmt.Errorf("HTTP limits: %w", err)
	}
	if err := validateRateLimits(config.WHOIS); err != nil {
		return nil, fmt.Errorf("WHOIS limits: %w", err)
	}
	l := &RateLimiter{
		http: slices.Clone(config.HTTP), whois: slices.Clone(config.WHOIS),
		servers: make(map[string][]RateLimit, len(config.Servers)),
		store:   config.Store,
	}
	for service, limits := range config.Servers {
		u, err := url.Parse(service)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != schemeHTTP && u.Scheme != schemeHTTPS && u.Scheme != schemeWHOIS) {
			return nil, fmt.Errorf("invalid rate limit service %q", service)
		}
		if len(limits) == 0 {
			return nil, fmt.Errorf("rate limits for %s must not be empty", service)
		}
		if limitsErr := validateRateLimits(limits); limitsErr != nil {
			return nil, fmt.Errorf("limits for %s: %w", service, limitsErr)
		}
		key := serviceKey(u.Scheme, u.Hostname())
		if _, exists := l.servers[key]; exists {
			return nil, fmt.Errorf("duplicate rate limit service %q", key)
		}
		l.servers[key] = slices.Clone(limits)
	}
	return l, nil
}

func WithRateLimiter(l *RateLimiter) Option {
	return func(c *Client) { c.rateLimiter = l }
}

func validateRateLimits(limits []RateLimit) error {
	for _, limit := range limits {
		if limit.Requests <= 0 || limit.Interval <= 0 {
			return errors.New("requests and interval must be positive")
		}
	}
	return nil
}

func serviceKey(protocol, host string) string {
	if protocol == schemeHTTP {
		protocol = schemeHTTPS
	}
	return protocol + "://" + strings.TrimSuffix(strings.ToLower(host), ".")
}

func (l *RateLimiter) policy(key string) []RateLimit {
	if limits, ok := l.servers[key]; ok {
		return limits
	}
	switch key {
	case "https://download.maxmind.com":
		return []RateLimit{{Requests: 1, Interval: time.Second}, {Requests: 30, Interval: 24 * time.Hour}}
	case "whois://whois.tcinet.ru":
		return []RateLimit{{Requests: 1, Interval: 500 * time.Millisecond}, {Requests: 120, Interval: time.Minute}}
	case "whois://whois.nic.uk":
		return []RateLimit{{Requests: 1, Interval: 200 * time.Millisecond}, {Requests: 1000, Interval: 24 * time.Hour}}
	}
	if strings.HasPrefix(key, "whois://") {
		if len(l.whois) > 0 {
			return l.whois
		}
	} else if len(l.http) > 0 {
		return l.http
	}
	return []RateLimit{{Requests: 1, Interval: time.Second}}
}

func (l *RateLimiter) storage() RateLimitStore {
	if l.store != nil {
		return l.store
	}
	return &l.memory
}

func (l *RateLimiter) wait(ctx context.Context, key string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		delay, err := l.storage().Acquire(ctx, key, slices.Clone(l.policy(key)))
		if err != nil {
			return fmt.Errorf("%w: acquire %s: %w", ErrRateLimitStore, key, err)
		}
		if delay < 0 {
			return fmt.Errorf("%w: acquire %s returned a negative delay", ErrRateLimitStore, key)
		}
		if delay == 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *RateLimiter) block(ctx context.Context, key string, until time.Time) (time.Time, error) {
	blocked, err := l.storage().Block(ctx, key, until)
	if err != nil {
		return until, fmt.Errorf("%w: block %s: %w", ErrRateLimitStore, key, err)
	}
	if blocked.Before(until) {
		return until, fmt.Errorf("%w: block %s returned a shorter cooldown", ErrRateLimitStore, key)
	}
	return blocked, nil
}

func isRateLimitError(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrRateLimitStore)
}

func retryAfter(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if value != "" && strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		const maxDuration time.Duration = 1<<63 - 1
		if err != nil || seconds > uint64(maxDuration/time.Second) {
			return now.Add(maxDuration)
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil {
		if date.Before(now) {
			return now
		}
		return date
	}
	return now.Add(time.Minute)
}

type rateLimitedTransport struct {
	base    http.RoundTripper
	limiter *RateLimiter
}

func (t *rateLimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := serviceKey(req.URL.Scheme, req.URL.Hostname())
	if err := t.limiter.wait(req.Context(), key); err != nil {
		return nil, err
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return response, err
	}
	if response == nil {
		return nil, errors.New("HTTP transport returned no response")
	}
	if response.StatusCode == http.StatusTooManyRequests {
		until := retryAfter(response.Header.Get("Retry-After"), time.Now())
		if response.Body != nil {
			_ = response.Body.Close()
		}
		until, blockErr := t.limiter.block(req.Context(), key, until)
		return nil, errors.Join(&RateLimitError{Server: key, RetryAt: until}, blockErr)
	}
	return response, nil
}

func (t *rateLimitedTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
