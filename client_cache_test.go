package reginfo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLookupMemoryCache(t *testing.T) {
	type args struct {
		domains  []string
		key      string
		unicode  string
		disabled bool
		ttl      time.Duration
		whois    bool
	}
	type want struct {
		rdapCalls  int32
		whoisCalls int32
	}
	tests := []struct {
		name string
		desc string
		args args
		want want
	}{
		{
			name: "ascii", desc: "Subdomains, case variants, and a trailing dot share a key",
			args: args{domains: []string{"WWW.Example.COM.", "example.com", "mail.example.com"}, key: "example.com", unicode: "example.com"},
			want: want{rdapCalls: 1},
		},
		{
			name: "idn", desc: "Unicode and punycode share one entry containing both names",
			args: args{domains: []string{"www.ДОМ.РФ.", "xn--d1aqf.xn--p1ai", "дом.рф"}, key: "xn--d1aqf.xn--p1ai", unicode: "дом.рф"},
			want: want{rdapCalls: 1},
		},
		{
			name: "private_suffix", desc: "Subdomains of github.io use the registrable domain's entry",
			args: args{domains: []string{"user.github.io", "github.io", "дом.github.io"}, key: "github.io", unicode: "github.io"},
			want: want{rdapCalls: 1},
		},
		{
			name: "whois", desc: "A successful WHOIS response after an RDAP error is also cached",
			args: args{domains: []string{"example.com", "www.example.com", "EXAMPLE.COM"}, key: "example.com", unicode: "example.com", whois: true},
			want: want{rdapCalls: 1, whoisCalls: 2},
		},
		{
			name: "disabled", desc: "WithCache(nil) keeps repeated registry requests enabled",
			args: args{domains: []string{"example.com", "example.com"}, key: "example.com", unicode: "example.com", disabled: true},
			want: want{rdapCalls: 2},
		},
		{
			name: "expired", desc: "The next lookup fetches fresh data after the TTL expires",
			args: args{domains: []string{"example.com", "example.com"}, key: "example.com", unicode: "example.com", ttl: time.Nanosecond},
			want: want{rdapCalls: 2},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var options []Option
			if tt.args.disabled {
				options = append(options, WithCache(nil))
			} else if tt.args.ttl != 0 {
				cache, err := NewMemoryCache(MemoryCacheConfig{TTL: tt.args.ttl})
				if err != nil {
					t.Fatal(err)
				}
				options = append(options, WithCache(cache))
			}
			var rdapCalls atomic.Int32
			client, bootstrapCalls := fixtureClient(t, http.StatusOK, func(w http.ResponseWriter, r *http.Request) {
				rdapCalls.Add(1)
				if r.URL.Path != "/rdap/domain/"+tt.args.key {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				if tt.args.whois {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				writeTestResponse(t, w, strings.Replace(domainJSON, "EXAMPLE.COM", tt.args.key, 1))
			}, options...)
			var whoisCalls *atomic.Int32
			if tt.args.whois {
				whoisCalls = installWHOIS(t, client, tt.args.key, "com", "Registrar: Example\nCreation Date: 2000-01-02T03:04:05Z\nName Server: NS1.EXAMPLE.COM\nStatus: active\n")
			}
			var first []byte
			for _, domain := range tt.args.domains {
				info, err := client.Lookup(t.Context(), domain)
				if err != nil || info == nil {
					t.Fatalf("Lookup = %+v, %v", info, err)
				}
				if info.Domain != tt.args.key || info.UnicodeName != tt.args.unicode {
					t.Fatalf("incorrect cached names: %+v", info)
				}
				encoded, err := json.Marshal(info)
				if err != nil {
					t.Fatal(err)
				}
				if first == nil {
					first = encoded
				} else if tt.want.rdapCalls == 1 && string(encoded) != string(first) {
					t.Fatalf("cached result changed: %s; want %s", encoded, first)
				}
				mutateCachedInfo(info)
			}
			if rdapCalls.Load() != tt.want.rdapCalls || bootstrapCalls.Load() != 1 {
				t.Fatalf("RDAP calls = %d, bootstrap calls = %d; want %d, 1", rdapCalls.Load(), bootstrapCalls.Load(), tt.want.rdapCalls)
			}
			if whoisCalls != nil && whoisCalls.Load() != tt.want.whoisCalls {
				t.Fatalf("WHOIS calls = %d; want %d", whoisCalls.Load(), tt.want.whoisCalls)
			}
		})
	}
}

func TestLookupCustomCache(t *testing.T) {
	type args struct {
		cached   bool
		getError bool
		setError bool
		cancelAt string
		invalid  bool
	}
	type want struct {
		gets, sets, requests int32
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    want
		wantErr error
	}{
		{name: "hit", desc: "A custom cache returns data without IANA, RDAP, or quota consumption", args: args{cached: true}, want: want{gets: 1}},
		{name: "miss", desc: "A cache miss stores the result under the normalized key", want: want{gets: 1, sets: 1, requests: 1}},
		{name: "read_error", desc: "A cache read error does not prevent a lookup", args: args{getError: true, cached: true}, want: want{gets: 1, sets: 1, requests: 1}},
		{name: "write_error", desc: "A cache write error does not hide a successful registry response", args: args{setError: true}, want: want{gets: 1, sets: 1, requests: 1}},
		{name: "cancel_before", desc: "A canceled lookup does not access even a populated cache", args: args{cached: true, cancelAt: "before"}, wantErr: context.Canceled},
		{name: "cancel_get", desc: "Cancellation during a cache read returns no result and makes no network request", args: args{cached: true, cancelAt: "get"}, want: want{gets: 1}, wantErr: context.Canceled},
		{name: "cancel_set", desc: "Cancellation during a cache write ends the lookup with a context error", args: args{cancelAt: "set"}, want: want{gets: 1, sets: 1, requests: 1}, wantErr: context.Canceled},
		{name: "invalid_domain", desc: "An invalid domain is rejected before accessing the cache", args: args{invalid: true}, wantErr: ErrInvalidDomain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			const key = "xn--d1aqf.xn--p1ai"
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			var stored *DomainInfo
			if tt.args.cached {
				stored = cachedTestInfo()
				stored.Domain, stored.UnicodeName = key, "дом.рф"
			}
			var gets, sets int32
			cache := &cacheFuncs{
				get: func(gotCtx context.Context, domain string) (*DomainInfo, error) {
					gets++
					if gotCtx != ctx || domain != key {
						t.Error("Get did not receive the caller context and normalized key")
					}
					if tt.args.cancelAt == "get" {
						cancel()
					}
					if tt.args.getError {
						return stored, errors.New("cache read unavailable")
					}
					return stored, nil
				},
				set: func(gotCtx context.Context, domain string, info *DomainInfo) error {
					sets++
					if gotCtx != ctx || domain != key || info.Domain != key || info.UnicodeName != "дом.рф" {
						t.Error("Set did not receive the caller context and normalized names")
					}
					if tt.args.cancelAt == "set" {
						cancel()
					}
					if tt.args.setError {
						return errors.New("cache write unavailable")
					}
					stored = info
					return nil
				},
			}
			var requests atomic.Int32
			client, bootstrapCalls := fixtureClient(t, http.StatusOK, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writeTestResponse(t, w, strings.Replace(domainJSON, "EXAMPLE.COM", key, 1))
			}, WithCache(cache))
			if tt.args.cached && !tt.args.getError {
				if _, err := client.rateLimiter.block(t.Context(), serviceKey("https", "data.iana.org"), time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				deadlineCtx, deadlineCancel := context.WithTimeout(ctx, time.Second)
				t.Cleanup(deadlineCancel)
				ctx = deadlineCtx
			}
			if tt.args.cancelAt == "before" {
				cancel()
			}
			domain := "www.ДОМ.РФ."
			if tt.args.invalid {
				domain = "co.uk"
			}
			info, err := client.Lookup(ctx, domain)
			if !errors.Is(err, tt.wantErr) || (info == nil) != (tt.wantErr != nil) {
				t.Fatalf("Lookup = %+v, %v; want error %v", info, err, tt.wantErr)
			}
			if gets != tt.want.gets || sets != tt.want.sets || requests.Load() != tt.want.requests || bootstrapCalls.Load() != tt.want.requests {
				t.Fatalf("gets = %d, sets = %d, RDAP = %d, bootstrap = %d; want %+v", gets, sets, requests.Load(), bootstrapCalls.Load(), tt.want)
			}
			if stored != nil && info != nil {
				before, marshalErr := json.Marshal(stored)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				mutateCachedInfo(info)
				after, marshalErr := json.Marshal(stored)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if string(before) != string(after) {
					t.Fatal("caller mutated a value retained by the custom cache")
				}
			}
		})
	}
}

func TestLookupCacheSharing(t *testing.T) {
	tests := []struct {
		name string
		desc string
		args bool
		want int32
	}{
		{name: "shared", desc: "An injected cache shares results across clients", args: true, want: 1},
		{name: "independent", desc: "Each client has an independent default cache", want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var options []Option
			if tt.args {
				options = append(options, WithCache(NewDefaultMemoryCache()))
			}
			var requests atomic.Int32
			var bootstrapTotal int32
			for range 2 {
				client, bootstrapCalls := fixtureClient(t, http.StatusOK, func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					writeTestResponse(t, w, domainJSON)
				}, options...)
				if _, err := client.Lookup(t.Context(), "example.com"); err != nil {
					t.Fatal(err)
				}
				bootstrapTotal += bootstrapCalls.Load()
			}
			if requests.Load() != tt.want || bootstrapTotal != tt.want {
				t.Fatalf("RDAP = %d, bootstrap = %d; want %d each", requests.Load(), bootstrapTotal, tt.want)
			}
		})
	}
}

func TestLookupDoesNotCacheErrors(t *testing.T) {
	tests := []struct {
		name    string
		desc    string
		args    int
		wantErr string
	}{
		{name: "not_found", desc: "Missing domains are not cached", args: http.StatusNotFound, wantErr: "HTTP 404"},
		{name: "unavailable", desc: "Registry unavailability is not cached", args: http.StatusServiceUnavailable, wantErr: "HTTP 503"},
		{name: "rate_limited", desc: "A 429 response is not stored as domain information", args: http.StatusTooManyRequests, wantErr: "HTTP 429"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var calls atomic.Int32
			client, _ := fixtureClient(t, http.StatusOK, func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(tt.args)
					return
				}
				writeTestResponse(t, w, domainJSON)
			})
			if tt.args != http.StatusTooManyRequests {
				installWHOIS(t, client, "example.com", "com", "No match found\n")
			}
			if info, err := client.Lookup(t.Context(), "example.com"); info != nil || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("first lookup = %+v, %v; want %s", info, err, tt.wantErr)
			}
			info, err := client.Lookup(t.Context(), "example.com")
			if err != nil || info == nil || info.Source != "rdap" || calls.Load() != 2 {
				t.Fatalf("retry = %+v, %v; calls = %d", info, err, calls.Load())
			}
		})
	}
}

type cacheFuncs struct {
	get func(context.Context, string) (*DomainInfo, error)
	set func(context.Context, string, *DomainInfo) error
}

func (c *cacheFuncs) Get(ctx context.Context, domain string) (*DomainInfo, error) {
	return c.get(ctx, domain)
}

func (c *cacheFuncs) Set(ctx context.Context, domain string, info *DomainInfo) error {
	return c.set(ctx, domain, info)
}
