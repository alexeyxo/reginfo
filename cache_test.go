package reginfo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestMemoryCacheEntries(t *testing.T) {
	type step struct {
		set  string
		get  string
		wait time.Duration
	}
	type args struct {
		config MemoryCacheConfig
		steps  []step
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    map[string]string
		wantErr error
	}{
		{
			name: "empty", desc: "A missing key returns nil without an error",
			want: map[string]string{"a": ""},
		},
		{
			name: "independent_keys", desc: "Different domains are stored independently",
			args: args{steps: []step{{set: "a"}, {set: "b"}}},
			want: map[string]string{"a": "0", "b": "1", "missing": ""},
		},
		{
			name: "default_ttl", desc: "A default entry remains available for five minutes",
			args: args{steps: []step{{set: "a"}, {wait: 5*time.Minute - time.Nanosecond}}},
			want: map[string]string{"a": "0"},
		},
		{
			name: "default_ttl_boundary", desc: "A default entry expires at exactly five minutes",
			args: args{steps: []step{{set: "a"}, {wait: 5 * time.Minute}}},
			want: map[string]string{"a": ""},
		},
		{
			name: "fixed_ttl", desc: "Reads do not extend the configured TTL",
			args: args{config: MemoryCacheConfig{TTL: time.Minute}, steps: []step{{set: "a"}, {wait: 30 * time.Second}, {get: "a"}, {wait: 30 * time.Second}}},
			want: map[string]string{"a": ""},
		},
		{
			name: "replace_renews_ttl", desc: "Replacing an entry updates its value and expiration",
			args: args{config: MemoryCacheConfig{TTL: time.Minute}, steps: []step{{set: "a"}, {wait: 30 * time.Second}, {set: "a"}, {wait: 30 * time.Second}}},
			want: map[string]string{"a": "2"},
		},
		{
			name: "least_recently_used", desc: "The least recently used entry is evicted at capacity",
			args: args{config: MemoryCacheConfig{MaxEntries: 2}, steps: []step{{set: "a"}, {set: "b"}, {get: "a"}, {set: "c"}}},
			want: map[string]string{"a": "0", "b": "", "c": "3"},
		},
		{
			name: "replace_at_capacity", desc: "Replacing an existing key does not evict other domains",
			args: args{config: MemoryCacheConfig{MaxEntries: 2}, steps: []step{{set: "a"}, {set: "b"}, {set: "a"}}},
			want: map[string]string{"a": "2", "b": "1"},
		},
		{
			name: "expired_before_live", desc: "Expired entries are evicted before live entries even if recently read",
			args: args{config: MemoryCacheConfig{TTL: time.Minute, MaxEntries: 2}, steps: []step{{set: "a"}, {wait: 30 * time.Second}, {set: "b"}, {get: "a"}, {wait: 30 * time.Second}, {set: "c"}}},
			want: map[string]string{"a": "", "b": "2", "c": "5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				cache, configErr := NewMemoryCache(tt.args.config)
				if configErr != nil {
					t.Fatal(configErr)
				}
				for i, step := range tt.args.steps {
					if step.wait != 0 {
						time.Sleep(step.wait)
					}
					if step.set != "" {
						if err := cache.Set(t.Context(), step.set, &DomainInfo{Domain: step.set, RegistryID: fmt.Sprint(i)}); err != nil {
							t.Fatal(err)
						}
					}
					if step.get != "" {
						info, err := cache.Get(t.Context(), step.get)
						if err != nil || info == nil {
							t.Fatalf("Get(%q) = %+v, %v", step.get, info, err)
						}
					}
				}
				for key, want := range tt.want {
					info, err := cache.Get(t.Context(), key)
					if !errors.Is(err, tt.wantErr) {
						t.Fatalf("Get error = %v; want %v", err, tt.wantErr)
					}
					if want == "" {
						if info != nil {
							t.Fatalf("Get(%q) = %+v; want nil", key, info)
						}
					} else if info == nil || info.Domain != key || info.RegistryID != want {
						t.Fatalf("Get(%q) = %+v; want RegistryID %q", key, info, want)
					}
				}
			})
		})
	}
}

func TestMemoryCacheDefaultCapacity(t *testing.T) {
	tests := []struct {
		name string
		desc string
		args func() *MemoryCache
		want int
	}{
		{name: "default", desc: "The default cache stores at most 256 domains", args: NewDefaultMemoryCache, want: 256},
		{name: "zero_value", desc: "The zero-value cache uses the same limits", args: func() *MemoryCache { return &MemoryCache{} }, want: 256},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			cache := tt.args()
			for i := range tt.want + 1 {
				key := fmt.Sprintf("example%d.com", i)
				if err := cache.Set(t.Context(), key, &DomainInfo{Domain: key}); err != nil {
					t.Fatal(err)
				}
			}
			for i := range tt.want + 1 {
				key := fmt.Sprintf("example%d.com", i)
				info, err := cache.Get(t.Context(), key)
				if err != nil || (info == nil) != (i == 0) {
					t.Fatalf("Get(%q) = %+v, %v; only the oldest entry must be absent", key, info, err)
				}
			}
		})
	}
}

func TestMemoryCacheCopies(t *testing.T) {
	tests := []struct {
		name string
		desc string
		args func() *DomainInfo
	}{
		{name: "all_fields", desc: "Mutating the supplied or returned DomainInfo does not change the cache", args: cachedTestInfo},
		{name: "optional_fields", desc: "Nil pointers and empty slices remain empty", args: func() *DomainInfo {
			return &DomainInfo{Domain: "example.com"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			info, want := tt.args(), tt.args()
			cache := NewDefaultMemoryCache()
			if err := cache.Set(t.Context(), info.Domain, info); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				mutateCachedInfo(info)
				var err error
				info, err = cache.Get(t.Context(), want.Domain)
				if err != nil || !reflect.DeepEqual(info, want) {
					t.Fatalf("Get = %+v, %v; want %+v", info, err, want)
				}
			}
		})
	}
}

func TestMemoryCacheCancellation(t *testing.T) {
	tests := []struct {
		name    string
		desc    string
		args    time.Duration
		wantErr error
	}{
		{name: "cancel", desc: "Cancellation prevents reading or modifying stored data", wantErr: context.Canceled},
		{name: "deadline", desc: "An expired deadline prevents reading or modifying stored data", args: -time.Second, wantErr: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			cache := NewDefaultMemoryCache()
			want := cachedTestInfo()
			if err := cache.Set(t.Context(), want.Domain, want); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			if tt.args != 0 {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), tt.args)
			}
			cancel()
			if info, err := cache.Get(ctx, want.Domain); info != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("Get = %+v, %v; want nil, %v", info, err, tt.wantErr)
			}
			if err := cache.Set(ctx, want.Domain, &DomainInfo{}); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Set error = %v; want %v", err, tt.wantErr)
			}
			if info, err := cache.Get(t.Context(), want.Domain); err != nil || !reflect.DeepEqual(info, want) {
				t.Fatalf("stored entry changed: %+v, %v", info, err)
			}
		})
	}
}

func TestMemoryCacheConcurrentAccess(t *testing.T) {
	tests := []struct {
		name string
		desc string
		args bool
	}{
		{name: "same_domain", desc: "Concurrent reads and writes for the same key are safe", args: true},
		{name: "different_domains", desc: "Concurrent operations on different domains do not lose entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			cache := NewDefaultMemoryCache()
			var wg sync.WaitGroup
			for i := range 12 {
				wg.Go(func() {
					key := fmt.Sprintf("example%d.com", i)
					if tt.args {
						key = "example.com"
					}
					for range 20 {
						info := cachedTestInfo()
						info.Domain = key
						if err := cache.Set(t.Context(), key, info); err != nil {
							t.Error(err)
							return
						}
						mutateCachedInfo(info)
						info, err := cache.Get(t.Context(), key)
						if err != nil || info == nil || info.Domain != key || info.NameServers[0] != "ns1.example.com" {
							t.Errorf("Get(%q) = %+v, %v", key, info, err)
							return
						}
						mutateCachedInfo(info)
					}
				})
			}
			wg.Wait()
		})
	}
}

func cachedTestInfo() *DomainInfo {
	created := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	updated, expires, age := created.AddDate(25, 0, 0), created.AddDate(30, 0, 0), 100
	return &DomainInfo{
		Domain: "example.com", UnicodeName: "example.com", RegistryID: "id-123",
		CreatedAt: &created, UpdatedAt: &updated, ExpiresAt: &expires, AgeDays: &age,
		Registrar: RegistrarInfo{Name: "Registrar"}, Registrant: RegistrantInfo{Organization: "Organization"},
		NameServers: []string{"ns1.example.com"}, DNSSEC: "signed", Status: []string{"active"},
		Registry: RegistryInfo{RDAPServer: "https://rdap.example"}, Source: "rdap",
		CollectedAt: updated, Raw: "raw response",
	}
}

func mutateCachedInfo(info *DomainInfo) {
	if info.CreatedAt != nil {
		*info.CreatedAt = time.Time{}
	}
	if info.UpdatedAt != nil {
		*info.UpdatedAt = time.Time{}
	}
	if info.ExpiresAt != nil {
		*info.ExpiresAt = time.Time{}
	}
	if info.AgeDays != nil {
		*info.AgeDays = -1
	}
	if len(info.NameServers) > 0 {
		info.NameServers[0] = "changed.example"
	}
	if len(info.Status) > 0 {
		info.Status[0] = "changed"
	}
	info.Domain, info.UnicodeName, info.Raw = "changed", "changed", "changed"
	info.Registrar.Name, info.Registrant.Organization, info.Registry.RDAPServer = "changed", "changed", "changed"
	info.CollectedAt = time.Time{}
}
