package reginfo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"resty.dev/v3"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLookupRDAP(t *testing.T) {
	type args struct {
		domain  string
		lookups int
	}
	type want struct {
		domain         string
		registryID     string
		source         string
		createdYear    int
		updatedYear    int
		expiresYear    int
		registrar      RegistrarInfo
		registrant     RegistrantInfo
		nameServers    []string
		dnssec         string
		status         []string
		bootstrapCalls int32
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		want    want
		wantErr error
	}{
		{
			name: "registration_data_and_cached_bootstrap",
			desc: "RDAP fields are normalized and repeated lookups reuse the IANA bootstrap",
			args: args{domain: " WWW.Example.COM. ", lookups: 2},
			want: want{
				domain: "example.com", registryID: "domain-123", source: "rdap",
				createdYear: 2000, updatedYear: 2025, expiresYear: 2030,
				registrar: RegistrarInfo{
					Name: "Example Registrar", IANAID: "123", URL: "https://registrar.example",
					AbuseEmail: "abuse@registrar.example", AbusePhone: "tel:+123456",
				},
				registrant:     RegistrantInfo{Organization: "Example Organization", State: "CA", Country: "US"},
				nameServers:    []string{"ns1.example.com"},
				dnssec:         "signed",
				status:         []string{"active"},
				bootstrapCalls: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			c, calls := fixtureClient(t, http.StatusOK, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/rdap/domain/"+tt.want.domain {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if !strings.Contains(r.Header.Get("Accept"), "application/rdap+json") {
					t.Error("missing RDAP Accept header")
				}
				writeTestResponse(t, w, domainJSON)
			})
			for range tt.args.lookups {
				info, err := c.Lookup(context.Background(), tt.args.domain)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Lookup error = %v; want %v", err, tt.wantErr)
				}
				if info == nil {
					t.Fatal("Lookup returned nil DomainInfo")
				}
				if info.Domain != tt.want.domain || info.RegistryID != tt.want.registryID || info.Source != tt.want.source || info.Raw != domainJSON || info.CollectedAt.IsZero() {
					t.Fatalf("incorrect domain data: %+v", info)
				}
				if info.CreatedAt == nil || info.CreatedAt.Year() != tt.want.createdYear || info.UpdatedAt == nil || info.UpdatedAt.Year() != tt.want.updatedYear || info.ExpiresAt == nil || info.ExpiresAt.Year() != tt.want.expiresYear || info.AgeDays == nil {
					t.Fatalf("incorrect dates: %+v", info)
				}
				if info.Registrar != tt.want.registrar || info.Registrant != tt.want.registrant {
					t.Fatalf("incorrect entities: %+v", info)
				}
				if info.DNSSEC != tt.want.dnssec || !reflect.DeepEqual(info.NameServers, tt.want.nameServers) || !reflect.DeepEqual(info.Status, tt.want.status) {
					t.Fatalf("incorrect DNS/status: %+v", info)
				}
				if info.Registry.RDAPServer == "" || info.Registry.WHOISServer != "" {
					t.Fatalf("incorrect registry: %+v", info.Registry)
				}
			}
			if calls.Load() != tt.want.bootstrapCalls {
				t.Fatalf("bootstrap requests = %d; want %d", calls.Load(), tt.want.bootstrapCalls)
			}
		})
	}
}

func TestLookupNormalizedNames(t *testing.T) {
	type args struct {
		domain          string
		rdapUnicodeName string
	}
	type want struct {
		domain      string
		unicodeName string
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		want    want
		wantErr error
	}{
		{
			name: "multilevel_suffix", desc: "Subdomains, case and the root dot are normalized",
			args: args{domain: "WWW.Example.CO.UK."}, want: want{domain: "example.co.uk", unicodeName: "example.co.uk"},
		},
		{
			name: "private_domain", desc: "A domain in the PRIVATE section reaches the RDAP request",
			args: args{domain: "github.io"}, want: want{domain: "github.io", unicodeName: "github.io"},
		},
		{
			name: "private_subdomain", desc: "A platform subdomain is queried as its registrable domain",
			args: args{domain: "WWW.Project.GitHub.IO."}, want: want{domain: "github.io", unicodeName: "github.io"},
		},
		{
			name: "private_unicode_subdomain", desc: "A Unicode platform subdomain normalizes to the owner's domain",
			args: args{domain: "дом.github.io"}, want: want{domain: "github.io", unicodeName: "github.io"},
		},
		{
			name: "cyrillic", desc: "Cyrillic labels are converted to punycode",
			args: args{domain: "дом.рф"}, want: want{domain: "xn--d1aqf.xn--p1ai", unicodeName: "дом.рф"},
		},
		{
			name: "cyrillic_subdomain", desc: "An uppercase Cyrillic subdomain resolves to the registrable domain",
			args: args{domain: "www.ДОМ.РФ."}, want: want{domain: "xn--d1aqf.xn--p1ai", unicodeName: "дом.рф"},
		},
		{
			name: "cyrillic_punycode", desc: "The punycode form produces the same RDAP query",
			args: args{domain: "xn--d1aqf.xn--p1ai"}, want: want{domain: "xn--d1aqf.xn--p1ai", unicodeName: "дом.рф"},
		},
		{
			name: "rdap_unicode_name", desc: "The Unicode name is normalized regardless of case in the RDAP response",
			args: args{domain: "xn--d1aqf.xn--p1ai", rdapUnicodeName: "ДОМ.РФ"}, want: want{domain: "xn--d1aqf.xn--p1ai", unicodeName: "дом.рф"},
		},
		{
			name: "cyrillic_example", desc: "Longer Cyrillic labels are supported",
			args: args{domain: "пример.рф"}, want: want{domain: "xn--e1afmkfd.xn--p1ai", unicodeName: "пример.рф"},
		},
		{
			name: "latin_unicode", desc: "An accented Latin label is converted to punycode",
			args: args{domain: "bücher.de"}, want: want{domain: "xn--bcher-kva.de", unicodeName: "bücher.de"},
		},
		{
			name: "decomposed_unicode", desc: "A combining mark is normalized to the same IDN label",
			args: args{domain: "bu\u0308cher.de"}, want: want{domain: "xn--bcher-kva.de", unicodeName: "bücher.de"},
		},
		{
			name: "latin_punycode", desc: "The ASCII form of an accented label is accepted",
			args: args{domain: "xn--bcher-kva.de"}, want: want{domain: "xn--bcher-kva.de", unicodeName: "bücher.de"},
		},
		{
			name: "chinese", desc: "Both the domain label and the Chinese suffix are converted",
			args: args{domain: "例子.中国"}, want: want{domain: "xn--fsqu00a.xn--fiqs8s", unicodeName: "例子.中国"},
		},
		{
			name: "unicode_separators", desc: "Unicode dots and a subdomain are normalized",
			args: args{domain: "www。例子。中国。"}, want: want{domain: "xn--fsqu00a.xn--fiqs8s", unicodeName: "例子.中国"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			c, _ := fixtureClient(t, http.StatusOK, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/rdap/domain/"+tt.want.domain {
					t.Errorf("path = %s; want domain %s", r.URL.Path, tt.want.domain)
				}
				response := map[string]string{"ldhName": tt.want.domain}
				if tt.args.rdapUnicodeName != "" {
					response["unicodeName"] = tt.args.rdapUnicodeName
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			})
			info, err := c.Lookup(context.Background(), tt.args.domain)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Lookup error = %v; want %v", err, tt.wantErr)
			}
			if info == nil || info.Domain != tt.want.domain || info.UnicodeName != tt.want.unicodeName {
				t.Fatalf("Lookup(%q) = %+v; want names %+v", tt.args.domain, info, tt.want)
			}
		})
	}
}

func TestLookupRejectsInvalidInputBeforeNetwork(t *testing.T) {
	type args struct {
		domain string
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		want    *DomainInfo
		wantErr error
	}{
		{name: "empty", desc: "Empty input is rejected", args: args{domain: ""}, wantErr: ErrInvalidDomain},
		{name: "tld_only", desc: "A TLD alone is not registrable", args: args{domain: "com"}, wantErr: ErrInvalidDomain},
		{name: "public_suffix", desc: "A multilevel suffix alone is not registrable", args: args{domain: "co.uk"}, wantErr: ErrInvalidDomain},
		{name: "unicode_tld", desc: "A Unicode TLD alone is not registrable", args: args{domain: "рф"}, wantErr: ErrInvalidDomain},
		{name: "unknown_suffix", desc: "An unknown public suffix is rejected", args: args{domain: "example.invalid"}, wantErr: ErrInvalidDomain},
		{name: "private_name_unknown_suffix", desc: "A platform name does not make an unknown suffix valid", args: args{domain: "user.github.invalid"}, wantErr: ErrInvalidDomain},
		{name: "url", desc: "URLs are not accepted as domain names", args: args{domain: "https://example.com"}, wantErr: ErrInvalidDomain},
		{name: "ipv4", desc: "IPv4 addresses are rejected", args: args{domain: "127.0.0.1"}, wantErr: ErrInvalidDomain},
		{name: "ipv6", desc: "IPv6 addresses are rejected", args: args{domain: "[::1]"}, wantErr: ErrInvalidDomain},
		{name: "empty_label", desc: "Empty labels are rejected", args: args{domain: "foo..com"}, wantErr: ErrInvalidDomain},
		{name: "leading_hyphen", desc: "A label cannot begin with a hyphen", args: args{domain: "-foo.com"}, wantErr: ErrInvalidDomain},
		{name: "trailing_hyphen", desc: "A label cannot end with a hyphen", args: args{domain: "foo-.com"}, wantErr: ErrInvalidDomain},
		{name: "underscore", desc: "An underscore is not valid in a domain label", args: args{domain: "foo_bar.com"}, wantErr: ErrInvalidDomain},
		{name: "control_characters", desc: "Embedded control characters are rejected", args: args{domain: "foo\r\nbar.com"}, wantErr: ErrInvalidDomain},
		{name: "multiple_root_dots", desc: "Only one trailing root dot is allowed", args: args{domain: "foo.com.."}, wantErr: ErrInvalidDomain},
		{name: "invalid_punycode", desc: "Malformed punycode is rejected", args: args{domain: "xn--.com"}, wantErr: ErrInvalidDomain},
		{name: "long_label", desc: "A label cannot exceed 63 bytes", args: args{domain: strings.Repeat("a", 64) + ".com"}, wantErr: ErrInvalidDomain},
		{name: "long_domain", desc: "A domain cannot exceed 253 bytes", args: args{domain: strings.Repeat("a.", 126) + "com"}, wantErr: ErrInvalidDomain},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			c := NewClient(WithRateLimiter(testRateLimiter(t)), WithHTTPClient(resty.New().SetTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("invalid input caused network request")
				return nil, errors.New("unexpected network")
			}))))
			info, err := c.Lookup(context.Background(), tt.args.domain)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Lookup error = %v; want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(info, tt.want) {
				t.Fatalf("Lookup(%q) = %+v; want %+v", tt.args.domain, info, tt.want)
			}
		})
	}
}

func TestNormalizePublicSuffixRules(t *testing.T) {
	type args struct {
		domain string
	}
	type want struct {
		domain string
		tld    string
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		want    want
		wantErr error
	}{
		{
			name: "multilevel_suffix", desc: "A multilevel suffix preserves the registrable label",
			args: args{domain: "a.b.example.com.au"}, want: want{domain: "example.com.au", tld: "au"},
		},
		{
			name: "suffix_exception", desc: "An exception overrides the wildcard suffix",
			args: args{domain: "www.city.kawasaki.jp"}, want: want{domain: "city.kawasaki.jp", tld: "jp"},
		},
		{
			name: "wildcard_suffix", desc: "A wildcard suffix requires an additional registrable label",
			args: args{domain: "a.b.ck"}, want: want{domain: "a.b.ck", tld: "ck"},
		},
		{
			name: "wildcard_exception", desc: "An exception can itself be a registrable domain",
			args: args{domain: "www.ck"}, want: want{domain: "www.ck", tld: "ck"},
		},
		{
			name: "private_domain", desc: "A PRIVATE suffix is itself a valid registrable domain",
			args: args{domain: "github.io"}, want: want{domain: "github.io", tld: "io"},
		},
		{
			name: "private_subdomain", desc: "A subdomain of a PRIVATE suffix resolves to the registry domain",
			args: args{domain: "project.github.io"}, want: want{domain: "github.io", tld: "io"},
		},
		{
			name: "private_rule", desc: "PRIVATE rules do not change the registration boundary",
			args: args{domain: "www.user.blogspot.com"}, want: want{domain: "blogspot.com", tld: "com"},
		},
		{
			name: "private_multilevel_parent", desc: "A platform under a multilevel suffix retains the ICANN boundary",
			args: args{domain: "www.user.blogspot.co.uk"}, want: want{domain: "blogspot.co.uk", tld: "uk"},
		},
		{
			name: "private_nested_suffix", desc: "A nested PRIVATE suffix resolves to the registrable domain",
			args: args{domain: "bucket.s3.amazonaws.com"}, want: want{domain: "amazonaws.com", tld: "com"},
		},
		{
			name: "private_wildcard", desc: "A PRIVATE wildcard rule does not prevent looking up the owner's domain",
			args: args{domain: "tenant.customer.compute.amazonaws.com"}, want: want{domain: "amazonaws.com", tld: "com"},
		},
		{
			name: "private_wildcard_only", desc: "The PRIVATE suffix matched by a wildcard is also allowed",
			args: args{domain: "customer.compute.amazonaws.com"}, want: want{domain: "amazonaws.com", tld: "com"},
		},
		{
			name: "wildcard_suffix_only", desc: "A wildcard suffix alone is not registrable",
			args: args{domain: "b.ck"}, wantErr: ErrInvalidDomain,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			domain, tld, err := normalize(tt.args.domain)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("normalize error = %v; want %v", err, tt.wantErr)
			}
			if domain != tt.want.domain || tld != tt.want.tld {
				t.Fatalf("normalize(%q) = %q, %q; want %q, %q", tt.args.domain, domain, tld, tt.want.domain, tt.want.tld)
			}
		})
	}
}

func TestLookupWHOISFallback(t *testing.T) {
	type args struct {
		input           string
		domain          string
		tld             string
		bootstrapStatus int
		rdapStatus      int
		rdapBody        string
		whoisBody       string
		lookups         int
	}
	type want struct {
		domain      string
		unicodeName string
		source      string
		registrar   string
		whoisServer string
		expiresYear int
		whoisCalls  int32
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		want    want
		wantErr error
	}{
		{
			name: "without_rdap", desc: "A TLD without RDAP uses WHOIS and caches its provider",
			args: args{
				input: "example.ru", domain: "example.ru", tld: "ru", lookups: 2,
				bootstrapStatus: http.StatusOK, rdapStatus: http.StatusOK,
				whoisBody: "created: 2000-01-02T03:04:05Z\npaid-till: 2030-01-02T03:04:05Z\nregistrar: EXAMPLE-RU\nnserver: ns1.example.ru. 192.0.2.1\nstate: REGISTERED, DELEGATED\n",
			},
			want: want{domain: "example.ru", unicodeName: "example.ru", source: "whois", registrar: "EXAMPLE-RU", whoisServer: "whois.registry.example", expiresYear: 2030, whoisCalls: 3},
		},
		{
			name: "rdap_failure", desc: "An RDAP HTTP error falls back to WHOIS",
			args: args{
				input: "example.com", domain: "example.com", tld: "com", lookups: 2,
				bootstrapStatus: http.StatusOK, rdapStatus: http.StatusServiceUnavailable,
				whoisBody: "Creation Date: 2000-01-02T03:04:05Z\nRegistry Expiry Date: 2030-01-02T03:04:05Z\nRegistrar: Example\nName Server: NS1.EXAMPLE.COM\n",
			},
			want: want{domain: "example.com", unicodeName: "example.com", source: "whois", registrar: "Example", whoisServer: "whois.registry.example", expiresYear: 2030, whoisCalls: 3},
		},
		{
			name: "bootstrap_failure", desc: "WHOIS remains usable when IANA HTTP bootstrap is unavailable",
			args: args{
				input: "example.ru", domain: "example.ru", tld: "ru", lookups: 2,
				bootstrapStatus: http.StatusServiceUnavailable, rdapStatus: http.StatusOK,
				whoisBody: "registrar: EXAMPLE-RU\n",
			},
			want: want{domain: "example.ru", unicodeName: "example.ru", source: "whois", registrar: "EXAMPLE-RU", whoisServer: "whois.registry.example", whoisCalls: 3},
		},
		{
			name: "malformed_rdap", desc: "Invalid RDAP JSON falls back to WHOIS",
			args: args{
				input: "example.com", domain: "example.com", tld: "com", lookups: 2,
				bootstrapStatus: http.StatusOK, rdapStatus: http.StatusOK,
				rdapBody: "{broken", whoisBody: "Registrar: Example\n",
			},
			want: want{domain: "example.com", unicodeName: "example.com", source: "whois", registrar: "Example", whoisServer: "whois.registry.example", whoisCalls: 3},
		},
		{
			name: "idn_whois", desc: "A Unicode subdomain is converted before the WHOIS fallback",
			args: args{
				input: "www.дом.рф", domain: "xn--d1aqf.xn--p1ai", tld: "xn--p1ai", lookups: 2,
				bootstrapStatus: http.StatusOK, rdapStatus: http.StatusServiceUnavailable,
				whoisBody: "registrar: EXAMPLE-RU\npaid-till: 2030-01-02T03:04:05Z\n",
			},
			want: want{domain: "xn--d1aqf.xn--p1ai", unicodeName: "дом.рф", source: "whois", registrar: "EXAMPLE-RU", whoisServer: "whois.registry.example", expiresYear: 2030, whoisCalls: 3},
		},
		{
			name: "punycode_whois", desc: "The Unicode name is restored from punycode when falling back to WHOIS",
			args: args{
				input: "xn--d1aqf.xn--p1ai", domain: "xn--d1aqf.xn--p1ai", tld: "xn--p1ai", lookups: 2,
				bootstrapStatus: http.StatusOK, rdapStatus: http.StatusServiceUnavailable,
				whoisBody: "registrar: EXAMPLE-RU\npaid-till: 2030-01-02T03:04:05Z\n",
			},
			want: want{domain: "xn--d1aqf.xn--p1ai", unicodeName: "дом.рф", source: "whois", registrar: "EXAMPLE-RU", whoisServer: "whois.registry.example", expiresYear: 2030, whoisCalls: 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			c, _ := fixtureClient(t, tt.args.bootstrapStatus, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.args.rdapStatus)
				writeTestResponse(t, w, tt.args.rdapBody)
			}, WithCache(nil))
			calls := installWHOIS(t, c, tt.args.domain, tt.args.tld, tt.args.whoisBody)
			for range tt.args.lookups {
				info, err := c.Lookup(context.Background(), tt.args.input)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Lookup error = %v; want %v", err, tt.wantErr)
				}
				if info == nil {
					t.Fatal("Lookup returned nil DomainInfo")
				}
				if info.Source != tt.want.source || info.Domain != tt.want.domain || info.UnicodeName != tt.want.unicodeName || info.Registrar.Name != tt.want.registrar || info.Registry.WHOISServer != tt.want.whoisServer || info.Raw != tt.args.whoisBody {
					t.Fatalf("incorrect WHOIS result: %+v", info)
				}
				if tt.want.expiresYear == 0 {
					if info.ExpiresAt != nil {
						t.Fatalf("ExpiresAt = %v; want nil", info.ExpiresAt)
					}
				} else if info.ExpiresAt == nil || info.ExpiresAt.Year() != tt.want.expiresYear {
					t.Fatalf("ExpiresAt = %v; want year %d", info.ExpiresAt, tt.want.expiresYear)
				}
			}
			if calls.Load() != tt.want.whoisCalls {
				t.Fatalf("WHOIS requests = %d; want %d", calls.Load(), tt.want.whoisCalls)
			}
		})
	}
}

func TestLookupRetainsBothFailures(t *testing.T) {
	type args struct {
		domain     string
		tld        string
		rdapStatus int
		whoisBody  string
	}

	tests := []struct {
		name           string
		desc           string
		args           args
		want           *DomainInfo
		wantErr        bool
		wantErrorParts []string
	}{
		{
			name: "rdap_and_whois_errors", desc: "The error retains both the RDAP status and the WHOIS parse failure",
			args:    args{domain: "example.com", tld: "com", rdapStatus: http.StatusServiceUnavailable, whoisBody: "No match found\n"},
			wantErr: true, wantErrorParts: []string{"HTTP 503", "no recognized registration fields"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			c, _ := fixtureClient(t, http.StatusOK, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.args.rdapStatus)
			})
			installWHOIS(t, c, tt.args.domain, tt.args.tld, tt.args.whoisBody)
			info, err := c.Lookup(context.Background(), tt.args.domain)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Lookup error = %v; wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(info, tt.want) {
				t.Fatalf("Lookup = %+v; want %+v", info, tt.want)
			}
			for _, part := range tt.wantErrorParts {
				if err == nil || !strings.Contains(err.Error(), part) {
					t.Errorf("Lookup error = %v; want substring %q", err, part)
				}
			}
		})
	}
}

func TestConcurrentLookups(t *testing.T) {
	type args struct {
		domain  string
		workers int
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		want    int32
		wantErr error
	}{
		{
			name: "shared_client", desc: "Concurrent lookups load IANA bootstrap only once",
			args: args{domain: "example.com", workers: 12}, want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			c, bootstrapCalls := fixtureClient(t, http.StatusOK, func(w http.ResponseWriter, _ *http.Request) {
				writeTestResponse(t, w, domainJSON)
			})
			var wg sync.WaitGroup
			for range tt.args.workers {
				wg.Go(func() {
					if _, err := c.Lookup(context.Background(), tt.args.domain); !errors.Is(err, tt.wantErr) {
						t.Errorf("Lookup error = %v; want %v", err, tt.wantErr)
					}
				})
			}
			wg.Wait()
			if bootstrapCalls.Load() != tt.want {
				t.Fatalf("bootstrap requests = %d; want %d", bootstrapCalls.Load(), tt.want)
			}
		})
	}
}

func TestBootstrapFailureIsRetryable(t *testing.T) {
	type args struct {
		domain string
	}
	type want struct {
		source    string
		httpCalls int
	}
	bootstrapErr := errors.New("temporary network failure")

	tests := []struct {
		name           string
		desc           string
		args           args
		want           want
		wantInitialErr error
		wantErr        error
	}{
		{
			name: "retry_after_network_failure", desc: "A failed bootstrap is retried on the next lookup",
			args: args{domain: "example.com"}, want: want{source: "rdap", httpCalls: 3},
			wantInitialErr: bootstrapErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var calls int
			c := NewClient(WithRateLimiter(testRateLimiter(t)), WithHTTPClient(resty.New().SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, bootstrapErr
				}
				body := `{"services":[[["com"],["https://rdap.example/"]]]}`
				if r.URL.String() != bootstrapURL {
					body = domainJSON
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			}))))
			c.dialContext = func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("WHOIS unavailable")
			}

			if _, err := c.Lookup(context.Background(), tt.args.domain); !errors.Is(err, tt.wantInitialErr) {
				t.Fatalf("initial Lookup error = %v; want %v", err, tt.wantInitialErr)
			}
			info, err := c.Lookup(context.Background(), tt.args.domain)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("retry error = %v; want %v", err, tt.wantErr)
			}
			if info == nil || info.Source != tt.want.source || calls != tt.want.httpCalls {
				t.Fatalf("retry = %+v; HTTP calls = %d; want %+v", info, calls, tt.want)
			}
		})
	}
}

func TestLookupCanceledContext(t *testing.T) {
	type args struct {
		domain string
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		setup   func(t *testing.T) context.Context
		want    *DomainInfo
		wantErr error
	}{
		{
			name: "canceled_before_lookup", desc: "A canceled context prevents lookup with a zero-value client",
			args: args{domain: "example.com"}, wantErr: context.Canceled,
			setup: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				cancel()
				return ctx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			ctx := tt.setup(t)
			var c Client
			info, err := c.Lookup(ctx, tt.args.domain)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Lookup error = %v; want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(info, tt.want) {
				t.Fatalf("Lookup = %+v; want %+v", info, tt.want)
			}
		})
	}
}

func TestWHOISContextErrors(t *testing.T) {
	type args struct {
		server           string
		query            string
		cancelAfterQuery bool
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		setup   func(t *testing.T) (context.Context, context.CancelFunc)
		want    string
		wantErr error
	}{
		{
			name: "cancellation_interrupts_read", desc: "Canceling a context interrupts a pending WHOIS read",
			args:    args{server: "whois.registry.example", query: "example.com", cancelAfterQuery: true},
			wantErr: context.Canceled,
			setup: func(t *testing.T) (context.Context, context.CancelFunc) {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				return ctx, cancel
			},
		},
		{
			name: "deadline_interrupts_read", desc: "A WHOIS socket timeout preserves the context deadline error",
			args:    args{server: "whois.registry.example", query: "example.com"},
			wantErr: context.DeadlineExceeded,
			setup: func(t *testing.T) (context.Context, context.CancelFunc) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				t.Cleanup(cancel)
				return ctx, cancel
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			c := NewClient(WithRateLimiter(testRateLimiter(t)))
			queryRead := make(chan struct{})
			c.dialContext = func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				t.Cleanup(func() { _ = client.Close() })
				t.Cleanup(func() { _ = server.Close() })
				go func() {
					defer server.Close()
					if _, err := bufio.NewReader(server).ReadString('\n'); err == nil {
						close(queryRead)
					}
					if _, copyErr := io.Copy(io.Discard, server); copyErr != nil && !errors.Is(copyErr, io.ErrClosedPipe) && !errors.Is(copyErr, net.ErrClosed) {
						t.Error(copyErr)
					}
				}()
				return client, nil
			}
			ctx, cancel := tt.setup(t)
			type result struct {
				body string
				err  error
			}
			done := make(chan result, 1)
			go func() {
				body, err := c.whois(ctx, tt.args.server, tt.args.query)
				done <- result{body: body, err: err}
			}()
			if tt.args.cancelAfterQuery {
				select {
				case <-queryRead:
				case <-time.After(2 * time.Second):
					t.Fatal("WHOIS query was not sent")
				}
				cancel()
			}
			select {
			case got := <-done:
				if !errors.Is(got.err, tt.wantErr) {
					t.Fatalf("WHOIS error = %v; want %v", got.err, tt.wantErr)
				}
				if got.body != tt.want {
					t.Fatalf("WHOIS body = %q; want %q", got.body, tt.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("context did not interrupt WHOIS exchange")
			}
		})
	}
}

func TestReadLimitedRejectsTruncatedResponses(t *testing.T) {
	type args struct {
		body string
		max  int64
	}

	tests := []struct {
		name    string
		desc    string
		args    args
		want    []byte
		wantErr bool
	}{
		{
			name: "at_limit", desc: "A response exactly at the limit is returned in full",
			args: args{body: "aaaa", max: 4}, want: []byte("aaaa"),
		},
		{
			name: "over_limit", desc: "An oversized response returns an error without truncated data",
			args: args{body: "aaaaa", max: 4}, wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			body, err := readLimited(strings.NewReader(tt.args.body), tt.args.max)
			if (err != nil) != tt.wantErr {
				t.Fatalf("readLimited error = %v; wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(body, tt.want) {
				t.Fatalf("readLimited = %q; want %q", body, tt.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClient(t *testing.T, bootstrapStatus int, rdapHandler http.HandlerFunc, options ...Option) (*Client, *atomic.Int32) {
	t.Helper()
	var bootstrapCalls atomic.Int32
	var base string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bootstrap" {
			bootstrapCalls.Add(1)
			w.WriteHeader(bootstrapStatus)
			if bootstrapStatus == http.StatusOK {
				if err := json.NewEncoder(w).Encode(bootstrapResponse{Services: [][][]string{{{"com", "uk", "de", "io", "xn--p1ai", "xn--fiqs8s"}, {base + "/rdap/"}}}}); err != nil {
					t.Error(err)
				}
			}
			return
		}
		rdapHandler(w, r)
	}))
	t.Cleanup(server.Close)
	base = server.URL
	h := resty.New().SetTransport(server.Client().Transport)
	transport := h.Transport()
	h.SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == bootstrapURL {
			r = r.Clone(r.Context())
			var err error
			r.URL, err = url.Parse(base + "/bootstrap")
			if err != nil {
				return nil, err
			}
		}
		return transport.RoundTrip(r)
	}))
	options = append([]Option{WithRateLimiter(testRateLimiter(t)), WithHTTPClient(h)}, options...)
	c := NewClient(options...)
	c.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Error("unexpected WHOIS request")
		return nil, errors.New("unexpected WHOIS request")
	}
	return c, &bootstrapCalls
}

const domainJSON = `{
  "ldhName": "EXAMPLE.COM", "handle": "domain-123",
  "events": [
    {"eventAction":"registration","eventDate":"2000-01-02T03:04:05Z"},
    {"eventAction":"last changed","eventDate":"2025-01-02T03:04:05Z"},
    {"eventAction":"expiration","eventDate":"2030-01-02T03:04:05Z"}
  ],
  "nameservers": [{"ldhName":"NS1.EXAMPLE.COM."},{"ldhName":"ns1.example.com"}],
  "status": ["active","active"], "secureDNS": {"delegationSigned":true},
  "entities": [
    {"roles":["registrar"], "publicIds":[{"type":"IANA Registrar ID","identifier":"123"}],
     "vcardArray":["vcard",[["fn",{},"text","Example Registrar"],["url",{},"uri","https://registrar.example"]]],
     "entities":[{"roles":["abuse"],"vcardArray":["vcard",[["email",{},"text","abuse@registrar.example"],["tel",{},"uri","tel:+123456"]]]}]},
    {"roles":["registrant"],"vcardArray":["vcard",[["org",{},"text","Example Organization"],["adr",{},"text",["","","","","CA","","US"]]]]}
  ]
}`

func installWHOIS(t *testing.T, c *Client, domain, tld, body string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	c.dialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		calls.Add(1)
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			if err := server.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Error(err)
				return
			}
			query, err := bufio.NewReader(server).ReadString('\n')
			if err != nil {
				t.Error(err)
				return
			}
			wantQuery, response := domain, body
			if address == "whois.iana.org:43" {
				wantQuery, response = tld, "whois: whois.registry.example\n"
			} else if address != "whois.registry.example:43" {
				t.Errorf("unexpected WHOIS address %q", address)
			}
			if network != "tcp" || query != wantQuery+"\r\n" {
				t.Errorf("WHOIS query = %q over %s; want %q", query, network, wantQuery)
			}
			writeTestResponse(t, server, response)
		}()
		return client, nil
	}
	return &calls
}

func testRateLimiter(t *testing.T) *RateLimiter {
	t.Helper()
	limiter, err := NewRateLimiter(RateLimitConfig{
		HTTP:  []RateLimit{{Requests: 1, Interval: time.Nanosecond}},
		WHOIS: []RateLimit{{Requests: 1, Interval: time.Nanosecond}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

func writeTestResponse(t *testing.T, writer io.Writer, body string) {
	t.Helper()
	if _, err := io.WriteString(writer, body); err != nil {
		t.Errorf("write response: %v", err)
	}
}
