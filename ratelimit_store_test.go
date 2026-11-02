package reginfo

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"resty.dev/v3"
)

func TestSharedRateLimitStore(t *testing.T) {
	tests := []struct {
		name, desc string
		servers    []string
		cooldown   time.Duration
		want       []time.Duration
	}{
		{name: "same_server", desc: "Separate limiters atomically consume a shared quota", servers: []string{"https://one.example"}, want: []time.Duration{0, time.Second, 2 * time.Second, 3 * time.Second}},
		{name: "different_servers", desc: "A shared store keeps server quotas separate", servers: []string{"https://one.example", "https://two.example"}, want: []time.Duration{0, 0, time.Second, time.Second}},
		{name: "cooldown", desc: "A cooldown set by one limiter applies to the others", servers: []string{"https://one.example"}, cooldown: 5 * time.Second, want: []time.Duration{5 * time.Second, 6 * time.Second, 7 * time.Second, 8 * time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				var store MemoryRateLimitStore
				first := rateLimitStoreLimiter(t, &store)
				second := rateLimitStoreLimiter(t, &store)
				start := time.Now()
				if tt.cooldown > 0 {
					if _, err := first.block(t.Context(), tt.servers[0], start.Add(tt.cooldown)); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan time.Duration, 4)
				var wg sync.WaitGroup
				for i := range 4 {
					wg.Go(func() {
						limiter := []*RateLimiter{first, second}[i%2]
						if err := limiter.wait(t.Context(), tt.servers[i%len(tt.servers)]); err != nil {
							t.Error(err)
						}
						done <- time.Since(start)
					})
				}
				wg.Wait()
				close(done)
				var got []time.Duration
				for elapsed := range done {
					got = append(got, elapsed)
				}
				slices.Sort(got)
				if !slices.Equal(got, tt.want) {
					t.Fatalf("request times = %v; want %v", got, tt.want)
				}
			})
		})
	}
}

func TestMemoryRateLimitStoreValidation(t *testing.T) {
	tests := []struct {
		name, desc string
		limits     []RateLimit
		cancel     bool
	}{
		{name: "empty", desc: "An empty policy is rejected"},
		{name: "invalid", desc: "An invalid policy is rejected", limits: []RateLimit{{Interval: time.Second}}},
		{name: "different_count", desc: "A different configuration cannot weaken a shared quota", limits: []RateLimit{{Requests: 2, Interval: time.Second}}},
		{name: "different_windows", desc: "The set of windows for a shared key cannot be replaced", limits: []RateLimit{{Requests: 1, Interval: time.Second}, {Requests: 2, Interval: time.Minute}}},
		{name: "canceled", desc: "Cancellation neither consumes quota nor sets a cooldown", limits: []RateLimit{{Requests: 1, Interval: time.Second}}, cancel: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				store := NewMemoryRateLimitStore()
				limits := []RateLimit{{Requests: 1, Interval: time.Second}}
				if _, err := store.Acquire(t.Context(), "key", limits); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				if tt.cancel {
					cancel()
					if _, err := store.Block(ctx, "key", time.Now().Add(time.Hour)); !errors.Is(err, context.Canceled) {
						t.Fatalf("Block error = %v", err)
					}
				}
				if _, err := store.Acquire(ctx, "key", tt.limits); err == nil {
					t.Fatal("Acquire accepted invalid input")
				} else if tt.cancel && !errors.Is(err, context.Canceled) {
					t.Fatalf("Acquire error = %v", err)
				}
				if delay, err := store.Acquire(t.Context(), "key", limits); err != nil || delay != 0 {
					t.Fatalf("rejected operation consumed quota: delay = %v, error = %v", delay, err)
				}
			})
		})
	}
}

func TestCustomRateLimitStoreWait(t *testing.T) {
	backendErr := errors.New("store unavailable")
	tests := []struct {
		name, desc string
		delay      time.Duration
		storeErr   error
		timeout    time.Duration
		want       time.Duration
		calls      int
		wantErr    error
	}{
		{name: "admit", desc: "A zero delay indicates successful admission", calls: 1},
		{name: "wait", desc: "The limiter requests admission again after waiting", delay: 2 * time.Second, calls: 2, want: 2 * time.Second},
		{name: "failure", desc: "A storage error is returned without retries", storeErr: backendErr, calls: 1, wantErr: ErrRateLimitStore},
		{name: "invalid_delay", desc: "A negative delay does not admit a request", delay: -time.Second, calls: 1, wantErr: ErrRateLimitStore},
		{name: "deadline", desc: "The context interrupts the wait for an external quota", delay: time.Hour, timeout: time.Second, calls: 1, want: time.Second, wantErr: context.DeadlineExceeded},
		{name: "store_deadline", desc: "A storage context error is preserved", storeErr: context.DeadlineExceeded, calls: 1, wantErr: ErrRateLimitStore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tt.timeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tt.timeout)
					t.Cleanup(cancel)
				}
				calls := 0
				store := &rateLimitTestStore{acquire: func(gotCtx context.Context, key string, limits []RateLimit) (time.Duration, error) {
					calls++
					if gotCtx != ctx || key != "https://download.maxmind.com" || !reflect.DeepEqual(limits, []RateLimit{{Requests: 1, Interval: time.Second}, {Requests: 30, Interval: 24 * time.Hour}}) {
						t.Fatalf("unexpected store arguments: key = %s, limits = %v", key, limits)
					}
					if calls == 1 {
						return tt.delay, tt.storeErr
					}
					return 0, nil
				}}
				limiter := rateLimitStoreLimiter(t, store)
				start := time.Now()
				err := limiter.wait(ctx, "https://download.maxmind.com")
				if !errors.Is(err, tt.wantErr) || (tt.storeErr != nil && !errors.Is(err, tt.storeErr)) {
					t.Fatalf("wait error = %v; want %v and %v", err, tt.wantErr, tt.storeErr)
				}
				if calls != tt.calls || time.Since(start) != tt.want {
					t.Fatalf("calls = %d, elapsed = %v; want %d, %v", calls, time.Since(start), tt.calls, tt.want)
				}
			})
		})
	}
}

func TestRateLimitStoreLookupErrors(t *testing.T) {
	backendErr := errors.New("store unavailable")
	tests := []struct {
		name, desc string
		bootstrap  bool
		ip         bool
		whois      bool
		wantCalls  int32
	}{
		{name: "domain_bootstrap", desc: "A bootstrap quota error stops a domain lookup", bootstrap: true},
		{name: "domain_rdap", desc: "An RDAP quota error does not trigger WHOIS", wantCalls: 1},
		{name: "ip_bootstrap", desc: "A bootstrap quota error stops an IP lookup", bootstrap: true, ip: true},
		{name: "ip_rdap", desc: "An RDAP quota error stops an IP lookup", ip: true, wantCalls: 1},
		{name: "whois_discovery", desc: "A storage error during WHOIS discovery retains its type", whois: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var calls atomic.Int32
			store := &rateLimitTestStore{acquire: func(ctx context.Context, key string, _ []RateLimit) (time.Duration, error) {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				if tt.bootstrap || key != "https://data.iana.org" {
					return 0, backendErr
				}
				return 0, nil
			}}
			limiter := rateLimitStoreLimiter(t, store)
			handler := func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/bootstrap/") {
					writeTestResponse(t, w, ipBootstrapFixture("https://"+r.Host))
					return
				}
				calls.Add(1)
			}
			var client *Client
			var bootstrapCalls *atomic.Int32
			if tt.ip {
				var counts *ipFixtureCalls
				client, counts = ipFixtureClient(t, handler, WithRateLimiter(limiter))
				bootstrapCalls = &counts.v4
			} else {
				client, bootstrapCalls = fixtureClient(t, http.StatusOK, handler, WithRateLimiter(limiter))
			}
			if tt.whois {
				client.bootstrapLoaded = true
			}
			var err error
			if tt.ip {
				_, err = client.LookupIP(t.Context(), "192.0.2.1")
			} else {
				_, err = client.Lookup(t.Context(), "example.com")
			}
			if !errors.Is(err, ErrRateLimitStore) || !errors.Is(err, backendErr) {
				t.Fatalf("lookup error = %v", err)
			}
			if calls.Load() != 0 || bootstrapCalls.Load() != tt.wantCalls {
				t.Fatalf("HTTP calls = %d, bootstrap calls = %d", calls.Load(), bootstrapCalls.Load())
			}
		})
	}
}

func TestRateLimitStoreStopsRDAPFallback(t *testing.T) {
	tests := []struct {
		name, desc string
		ip         bool
	}{
		{name: "domain", desc: "A second RDAP server is not queried after a storage error"},
		{name: "ip", desc: "A second IP RDAP server is not queried after a storage error", ip: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			calls := 0
			store := &rateLimitTestStore{acquire: func(context.Context, string, []RateLimit) (time.Duration, error) {
				calls++
				return 0, errors.New("store unavailable")
			}}
			client := NewClient(WithRateLimiter(rateLimitStoreLimiter(t, store)))
			servers := []string{"https://one.example", "https://two.example"}
			var err error
			if tt.ip {
				_, err = client.lookupIPRDAP(t.Context(), servers, netip.MustParseAddr("192.0.2.1"))
			} else {
				_, err = client.lookupRDAP(t.Context(), Provider{RDAP: servers}, "example.com")
			}
			if !errors.Is(err, ErrRateLimitStore) || calls != 1 {
				t.Fatalf("error = %v, store calls = %d; want storage error and one call", err, calls)
			}
		})
	}
}

func TestRateLimitStoreScanErrors(t *testing.T) {
	backendErr := errors.New("store unavailable")
	tests := []struct {
		name, desc string
		workers    int
	}{
		{name: "one_worker", desc: "A storage error is returned with collected scan results", workers: 1},
		{name: "two_workers", desc: "Storage errors from multiple workers are preserved", workers: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var networkCalls, storeCalls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				networkCalls.Add(1)
				if r.URL.Path == "/rdap/dns.json" {
					writeTestResponse(t, w, `{"services": []}`)
				} else {
					writeTestResponse(t, w, "COM\nRU\n")
				}
			}))
			t.Cleanup(server.Close)
			h := resty.New().SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				r = r.Clone(r.Context())
				r.URL.Host = server.Listener.Addr().String()
				return server.Client().Transport.RoundTrip(r)
			}))
			t.Cleanup(func() { _ = h.Close() })
			store := &rateLimitTestStore{acquire: func(_ context.Context, key string, _ []RateLimit) (time.Duration, error) {
				if key == "whois://whois.iana.org" {
					storeCalls.Add(1)
					return 0, backendErr
				}
				return 0, nil
			}}
			client := NewClient(WithRateLimiter(rateLimitStoreLimiter(t, store)), WithHTTPClient(h))
			client.dialContext = func(context.Context, string, string) (net.Conn, error) {
				t.Error("unexpected WHOIS connection")
				return nil, errors.New("unexpected dial")
			}
			providers, err := client.ScanProviders(t.Context(), tt.workers)
			if !errors.Is(err, ErrRateLimitStore) || !errors.Is(err, backendErr) {
				t.Fatalf("scan error = %v", err)
			}
			if len(providers) != 2 || networkCalls.Load() != 2 || storeCalls.Load() != 2 {
				t.Fatalf("providers = %v, network calls = %d, store calls = %d", providers, networkCalls.Load(), storeCalls.Load())
			}
			for _, provider := range providers {
				if provider.Status != "discovery_error" || provider.Error == "" {
					t.Fatalf("provider = %+v; want discovery error", provider)
				}
			}
		})
	}
}

func TestRateLimitStoreCooldownErrors(t *testing.T) {
	backendErr := errors.New("store unavailable")
	tests := []struct {
		name, desc string
		storeErr   error
		shorten    bool
		wantErr    error
	}{
		{name: "saved", desc: "An external store can extend a cooldown"},
		{name: "failure", desc: "A cooldown write error does not hide HTTP 429", storeErr: backendErr, wantErr: ErrRateLimitStore},
		{name: "shortened", desc: "The store cannot shorten Retry-After", shorten: true, wantErr: ErrRateLimitStore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			body := &rateTestBody{Reader: strings.NewReader("limited")}
			var wantUntil time.Time
			store := &rateLimitTestStore{
				RateLimitStore: NewMemoryRateLimitStore(),
				block: func(ctx context.Context, key string, until time.Time) (time.Time, error) {
					if ctx.Err() != nil || key != "https://registry.example" || !body.closed {
						t.Fatalf("incorrect block call: key = %s, body closed = %v", key, body.closed)
					}
					wantUntil = until
					if tt.storeErr != nil {
						return time.Time{}, tt.storeErr
					}
					if tt.shorten {
						return until.Add(-time.Second), nil
					}
					wantUntil = until.Add(time.Minute)
					return wantUntil, nil
				},
			}
			transport := &rateLimitedTransport{
				limiter: rateLimitStoreLimiter(t, store),
				base: roundTripFunc(func(*http.Request) (*http.Response, error) {
					response := rateTestResponse(429, "3", "")
					response.Body = body
					return response, nil
				}),
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://REGISTRY.example.:8080/test", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = transport.RoundTrip(req)
			var limited *RateLimitError
			if !errors.Is(err, ErrRateLimited) || !errors.As(err, &limited) || !limited.RetryAt.Equal(wantUntil) {
				t.Fatalf("429 error = %v", err)
			}
			if (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) || (tt.storeErr != nil && !errors.Is(err, tt.storeErr)) {
				t.Fatalf("store error = %v; want %v and %v", err, tt.wantErr, tt.storeErr)
			}
		})
	}
}

func TestRateLimitStoreGeoIPAndWHOIS(t *testing.T) {
	backendErr := errors.New("store unavailable")
	tests := []struct {
		name, desc string
		geoIP      bool
		block      bool
		wantCalls  int32
	}{
		{name: "whois", desc: "A quota error prevents the WHOIS TCP connection"},
		{name: "geoip_acquire", desc: "GeoIP uses the external store before downloading", geoIP: true},
		{name: "geoip_block", desc: "A Retry-After write error is returned by the GeoIP update", geoIP: true, block: true, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var calls atomic.Int32
			store := &rateLimitTestStore{
				acquire: func(_ context.Context, key string, _ []RateLimit) (time.Duration, error) {
					want := "whois://whois.example"
					if tt.geoIP {
						want = "https://download.maxmind.com"
					}
					if key != want {
						t.Errorf("key = %s; want %s", key, want)
					}
					if tt.block {
						return 0, nil
					}
					return 0, backendErr
				},
				block: func(context.Context, string, time.Time) (time.Time, error) {
					return time.Time{}, backendErr
				},
			}
			limiter := rateLimitStoreLimiter(t, store)
			var err error
			if tt.geoIP {
				geo := geoIPDownloadTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.WriteHeader(http.StatusTooManyRequests)
				})
				geo.config.RateLimiter = limiter
				err = geo.UpdateDatabases(t.Context())
			} else {
				client := NewClient(WithRateLimiter(limiter))
				client.dialContext = func(context.Context, string, string) (net.Conn, error) {
					calls.Add(1)
					return nil, errors.New("unexpected dial")
				}
				_, err = client.whois(t.Context(), "WHOIS.example.", "example.com")
			}
			if !errors.Is(err, ErrRateLimitStore) || !errors.Is(err, backendErr) || (tt.block && !errors.Is(err, ErrRateLimited)) {
				t.Fatalf("request error = %v", err)
			}
			if calls.Load() != tt.wantCalls {
				t.Fatalf("network calls = %d; want %d", calls.Load(), tt.wantCalls)
			}
		})
	}
}

type rateLimitTestStore struct {
	RateLimitStore
	acquire func(context.Context, string, []RateLimit) (time.Duration, error)
	block   func(context.Context, string, time.Time) (time.Time, error)
}

func (s *rateLimitTestStore) Acquire(ctx context.Context, key string, limits []RateLimit) (time.Duration, error) {
	if s.acquire != nil {
		return s.acquire(ctx, key, limits)
	}
	return s.RateLimitStore.Acquire(ctx, key, limits)
}

func (s *rateLimitTestStore) Block(ctx context.Context, key string, until time.Time) (time.Time, error) {
	if s.block != nil {
		return s.block(ctx, key, until)
	}
	return s.RateLimitStore.Block(ctx, key, until)
}

func rateLimitStoreLimiter(t *testing.T, store RateLimitStore) *RateLimiter {
	t.Helper()
	limiter, err := NewRateLimiter(RateLimitConfig{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}
