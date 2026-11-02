package reginfo

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestIPMemoryCache(t *testing.T) {
	tests := []struct {
		name, desc string
		config     MemoryCacheConfig
		wait       time.Duration
		evict      bool
		wantMiss   bool
	}{
		{name: "copies", desc: "The cache copies dates and nested contact lists"},
		{name: "ttl", desc: "An entry expires exactly at the configured TTL", config: MemoryCacheConfig{TTL: time.Minute}, wait: time.Minute, wantMiss: true},
		{name: "default_ttl", desc: "A zero TTL selects five minutes", wait: 5 * time.Minute, wantMiss: true},
		{name: "capacity", desc: "An older IP entry is evicted at capacity", config: MemoryCacheConfig{MaxEntries: 1}, evict: true, wantMiss: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				cache, err := NewIPMemoryCache(tt.config)
				if err != nil {
					t.Fatal(err)
				}
				want, info := cachedIPTestInfo(), cachedIPTestInfo()
				if setErr := cache.Set(t.Context(), want.IP, info); setErr != nil {
					t.Fatal(setErr)
				}
				mutateCachedIPInfo(info)
				if tt.wait != 0 {
					time.Sleep(tt.wait)
				}
				if tt.evict {
					if setErr := cache.Set(t.Context(), "192.0.2.43", cachedIPTestInfo()); setErr != nil {
						t.Fatal(setErr)
					}
				}
				for range 2 {
					info, err = cache.Get(t.Context(), want.IP)
					if err != nil || (info == nil) != tt.wantMiss {
						t.Fatalf("Get=%+v, %v", info, err)
					}
					if !tt.wantMiss {
						if !reflect.DeepEqual(info, want) {
							t.Fatalf("Get=%+v; want %+v", info, want)
						}
						mutateCachedIPInfo(info)
					}
				}
			})
		})
	}
}

func TestIPMemoryCacheConcurrentAccess(t *testing.T) {
	tests := []struct {
		name, desc string
		canceled   bool
	}{
		{"parallel", "Concurrent reads and writes retain independent copies", false},
		{"canceled", "Cancellation does not change previously stored data", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var cache IPMemoryCache
			want := cachedIPTestInfo()
			if err := cache.Set(t.Context(), want.IP, want); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if tt.canceled {
				cancel()
			}
			var wg sync.WaitGroup
			for range 12 {
				wg.Go(func() {
					for range 10 {
						setErr := cache.Set(ctx, want.IP, cachedIPTestInfo())
						info, err := cache.Get(ctx, want.IP)
						if tt.canceled {
							if !errors.Is(setErr, context.Canceled) || !errors.Is(err, context.Canceled) || info != nil {
								t.Errorf("Set=%v Get=%+v, %v", setErr, info, err)
							}
							return
						}
						if setErr != nil || err != nil || !reflect.DeepEqual(info, want) {
							t.Errorf("Set=%v Get=%+v, %v", setErr, info, err)
							return
						}
						mutateCachedIPInfo(info)
					}
				})
			}
			wg.Wait()
			info, err := cache.Get(t.Context(), want.IP)
			if err != nil || !reflect.DeepEqual(info, want) {
				t.Fatalf("stored value changed: %+v, %v", info, err)
			}
		})
	}
}

func TestLookupIPCustomCache(t *testing.T) {
	tests := []struct {
		name, desc                                              string
		disabled, hit, getError, setError, cancelGet, cancelSet bool
		wantCalls                                               int32
	}{
		{name: "hit", desc: "A custom cache returns a copy without network access", hit: true},
		{name: "miss", desc: "A cache miss stores the registry result", wantCalls: 1},
		{name: "disabled", desc: "WithIPCache(nil) disables only the IP cache", disabled: true, wantCalls: 1},
		{name: "get_error", desc: "A cache read error does not prevent a registry request", getError: true, wantCalls: 1},
		{name: "set_error", desc: "A cache write error does not discard the response", setError: true, wantCalls: 1},
		{name: "cancel_get", desc: "Cancellation during a cache read stops the lookup", hit: true, cancelGet: true},
		{name: "cancel_set", desc: "Cancellation during a cache write is returned to the caller", cancelSet: true, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			stored := cachedIPTestInfo()
			var saved *IPInfo
			var gets, sets int
			cache := &testIPCache{
				get: func(_ context.Context, key string) (*IPInfo, error) {
					gets++
					if key != "192.0.2.42" {
						t.Errorf("key=%q", key)
					}
					if tt.cancelGet {
						cancel()
					}
					if tt.getError {
						return nil, errors.New("cache unavailable")
					}
					if tt.hit {
						return stored, nil
					}
					return nil, nil //nolint:nilnil // The IPCache contract returns nil without an error on a miss.
				},
				set: func(_ context.Context, key string, info *IPInfo) error {
					sets++
					if key != "192.0.2.42" {
						t.Errorf("key=%q", key)
					}
					saved = info
					if tt.cancelSet {
						cancel()
					}
					if tt.setError {
						return errors.New("cache unavailable")
					}
					return nil
				},
			}
			option := WithIPCache(cache)
			if tt.disabled {
				option = WithIPCache(nil)
			}
			client, calls := ipFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/bootstrap/") {
					writeTestResponse(t, w, ipBootstrapFixture("https://"+r.Host))
					return
				}
				writeTestResponse(t, w, ipRDAPFixture("v4", "192.0.2.0", "192.0.2.255"))
			}, option)
			info, err := client.LookupIP(ctx, "::ffff:192.0.2.42")
			if tt.cancelGet || tt.cancelSet {
				if !errors.Is(err, context.Canceled) || info != nil {
					t.Fatalf("LookupIP=%+v, %v", info, err)
				}
			} else {
				if err != nil || info == nil {
					t.Fatalf("LookupIP=%+v, %v", info, err)
				}
				mutateCachedIPInfo(info)
				if stored.CIDRs[0] != "192.0.2.0/24" || (saved != nil && saved.CIDRs[0] != "192.0.2.0/24") {
					t.Fatal("client shared mutable cache value")
				}
			}
			if calls.rdap.Load() != tt.wantCalls {
				t.Fatalf("requests=%d; want %d", calls.rdap.Load(), tt.wantCalls)
			}
			if tt.disabled && (gets != 0 || sets != 0) {
				t.Fatalf("disabled cache calls: %d, %d", gets, sets)
			}
			if client.cache == nil {
				t.Fatal("IP cache option disabled domain cache")
			}
		})
	}
}

func TestLookupIPCancellation(t *testing.T) {
	tests := []struct{ name, desc, stage string }{
		{"before_request", "A canceled context sends no requests", "before"},
		{"bootstrap_body", "Cancellation interrupts bootstrap response reading", "bootstrap"},
		{"rdap_body", "Cancellation interrupts RDAP response reading without WHOIS fallback", "rdap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			client, _ := ipFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				bootstrap := strings.HasPrefix(r.URL.Path, "/bootstrap/")
				if (bootstrap && tt.stage == "bootstrap") || (!bootstrap && tt.stage == "rdap") {
					writeTestResponse(t, w, "{")
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Error(err)
						return
					}
					cancel()
					<-r.Context().Done()
					return
				}
				writeTestResponse(t, w, ipBootstrapFixture("https://"+r.Host))
			})
			if tt.stage == "before" {
				cancel()
			}
			info, err := client.LookupIP(ctx, "192.0.2.42")
			if info != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("LookupIP=%+v, %v", info, err)
			}
		})
	}
}

func TestIPLookupRateLimitAndCache(t *testing.T) {
	tests := []struct {
		name, desc   string
		disableCache bool
		wantErr      error
	}{
		{"cache_hit", "An IP cache hit consumes no additional quota", false, nil},
		{"quota_wait", "Without a cache, context cancellation interrupts the quota wait", true, context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			limiter, err := NewRateLimiter(RateLimitConfig{HTTP: []RateLimit{{Requests: 1, Interval: time.Hour}}})
			if err != nil {
				t.Fatal(err)
			}
			options := []Option{WithRateLimiter(limiter)}
			if tt.disableCache {
				options = append(options, WithIPCache(nil))
			}
			client, calls := ipFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/bootstrap/") {
					writeTestResponse(t, w, ipBootstrapFixture("https://"+r.Host))
					return
				}
				writeTestResponse(t, w, ipRDAPFixture("v4", "192.0.2.0", "192.0.2.255"))
			}, options...)
			if _, lookupErr := client.LookupIP(t.Context(), "192.0.2.42"); lookupErr != nil {
				t.Fatal(lookupErr)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			t.Cleanup(cancel)
			_, err = client.LookupIP(ctx, "192.0.2.42")
			if !errors.Is(err, tt.wantErr) || calls.rdap.Load() != 1 {
				t.Fatalf("error=%v calls=%d", err, calls.rdap.Load())
			}
		})
	}
}

type testIPCache struct {
	get func(context.Context, string) (*IPInfo, error)
	set func(context.Context, string, *IPInfo) error
}

func (c *testIPCache) Get(ctx context.Context, ip string) (*IPInfo, error) { return c.get(ctx, ip) }
func (c *testIPCache) Set(ctx context.Context, ip string, info *IPInfo) error {
	return c.set(ctx, ip, info)
}

func cachedIPTestInfo() *IPInfo {
	created := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	updated := created.AddDate(24, 0, 0)
	return &IPInfo{IP: "192.0.2.42", IPVersion: "v4", StartAddress: "192.0.2.0", EndAddress: "192.0.2.255", CIDRs: []string{"192.0.2.0/24"}, Status: []string{"active"}, CreatedAt: &created, UpdatedAt: &updated,
		Contacts: []IPContact{{Name: "Test", Roles: []string{"abuse"}, Emails: []string{"abuse@example.test"}, Phones: []string{"+123456"}}}}
}

func mutateCachedIPInfo(info *IPInfo) {
	info.IP = "changed"
	if info.CreatedAt != nil {
		*info.CreatedAt = time.Time{}
	}
	if info.UpdatedAt != nil {
		*info.UpdatedAt = time.Time{}
	}
	if len(info.CIDRs) > 0 {
		info.CIDRs[0] = "changed"
	}
	if len(info.Status) > 0 {
		info.Status[0] = "changed"
	}
	for i := range info.Contacts {
		contact := &info.Contacts[i]
		contact.Name = "changed"
		if len(contact.Roles) > 0 {
			contact.Roles[0] = "changed"
		}
		if len(contact.Emails) > 0 {
			contact.Emails[0] = "changed"
		}
		if len(contact.Phones) > 0 {
			contact.Phones[0] = "changed"
		}
	}
}
