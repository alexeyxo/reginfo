package reginfo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"resty.dev/v3"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLookupIPRDAP(t *testing.T) {
	tests := []struct {
		name     string
		desc     string
		inputs   []string
		ip       string
		version  string
		start    string
		end      string
		cidr     string
		redirect bool
	}{
		{name: "ipv4", desc: "IPv4 returns network information and subsequent lookups use the cache", inputs: []string{"192.0.2.42", " 192.0.2.42 "}, ip: "192.0.2.42", version: "v4", start: "192.0.2.0", end: "192.0.2.255", cidr: "192.0.2.0/24"},
		{name: "mapped_ipv4", desc: "IPv4-mapped IPv6 uses the IPv4 bootstrap and the same cache key", inputs: []string{"::ffff:192.0.2.42", "192.0.2.42"}, ip: "192.0.2.42", version: "v4", start: "192.0.2.0", end: "192.0.2.255", cidr: "192.0.2.0/24"},
		{name: "ipv6", desc: "Different IPv6 spellings normalize to the same address", inputs: []string{"2001:0DB8:0000:0000:0000:0000:0000:0042", "2001:db8::42"}, ip: "2001:db8::42", version: "v6", start: "2001:db8::", end: "2001:db8::ffff", cidr: "2001:db8::/112"},
		{name: "redirect", desc: "Resty follows a registry redirect", inputs: []string{"192.0.2.42"}, ip: "192.0.2.42", version: "v4", start: "192.0.2.0", end: "192.0.2.255", cidr: "192.0.2.0/24", redirect: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			body := ipRDAPFixture(tt.version, tt.start, tt.end)
			client, calls := ipFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/bootstrap/") {
					writeTestResponse(t, w, ipBootstrapFixture("https://"+r.Host))
					return
				}
				if tt.redirect && strings.HasPrefix(r.URL.Path, "/registry/") {
					http.Redirect(w, r, "/final/ip/"+tt.ip, http.StatusFound)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/ip/"+tt.ip) || r.Header.Get("Accept") != "application/rdap+json, application/json" {
					t.Errorf("unexpected request: %s, headers=%v", r.URL.Path, r.Header)
				}
				writeTestResponse(t, w, body)
			})
			for _, input := range tt.inputs {
				info, err := client.LookupIP(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
				if info.IP != tt.ip || info.IPVersion != tt.version || info.StartAddress != tt.start || info.EndAddress != tt.end || !reflect.DeepEqual(info.CIDRs, []string{tt.cidr}) {
					t.Fatalf("unexpected network: %+v", info)
				}
				if info.Source != sourceRDAP || info.Raw != body || info.CollectedAt.IsZero() || info.RegistryID != "NET-TEST" || info.ParentHandle != "PARENT" || info.Name != "TEST-NET" || info.Type != "ALLOCATED" || info.Country != "ZZ" || !reflect.DeepEqual(info.Status, []string{"active"}) {
					t.Fatalf("unexpected metadata: %+v", info)
				}
				if info.Organization != "Example Network" || len(info.Contacts) != 2 || info.Contacts[1].Emails[0] != "abuse@example.test" || info.CreatedAt == nil || info.CreatedAt.Year() != 2001 || info.UpdatedAt == nil || info.UpdatedAt.Year() != 2025 {
					t.Fatalf("unexpected contacts or dates: %+v", info)
				}
				if info.Registry.WHOISServer != "whois.registry.example" || !strings.HasSuffix(info.Registry.RDAPServer, "/registry/") {
					t.Fatalf("registry = %+v", info.Registry)
				}
				info.Contacts[1].Emails[0] = "changed"
				info.CIDRs[0] = "changed"
			}
			wantRequests := int32(1)
			if tt.redirect {
				wantRequests++
			}
			if calls.rdap.Load() != wantRequests || calls.v4.Load()+calls.v6.Load() != 1 || (calls.v4.Load() == 1) != (tt.version == "v4") {
				t.Fatalf("requests: rdap=%d ipv4=%d ipv6=%d", calls.rdap.Load(), calls.v4.Load(), calls.v6.Load())
			}
		})
	}
}

func TestLookupIPInvalidInput(t *testing.T) {
	tests := []struct{ name, desc, input string }{
		{"empty", "An empty address is rejected", ""},
		{"domain", "Domain names are handled by Lookup", "example.com"},
		{"url", "A URL is not an IP address", "https://192.0.2.1"},
		{"port", "An address with a port is rejected", "192.0.2.1:43"},
		{"cidr", "The public method accepts a single address", "192.0.2.0/24"},
		{"zone", "An IPv6 zone identifier is not sent to the registry", "fe80::1%en0"},
		{"brackets", "Brackets are URL syntax rather than part of an IP address", "[2001:db8::1]"},
		{"leading_zero", "Ambiguous IPv4 notation is rejected", "192.000.2.1"},
		{"overflow", "An IPv4 octet cannot exceed 255", "192.0.2.256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var client Client
			for _, lookup := range []func(context.Context, string) (*IPInfo, error){client.LookupIP, LookupIP} {
				if info, err := lookup(t.Context(), tt.input); info != nil || !errors.Is(err, ErrInvalidIP) {
					t.Fatalf("LookupIP = %+v, %v", info, err)
				}
			}
			if client.http != nil {
				t.Fatal("invalid input initialized network client")
			}
		})
	}
}

func TestLookupIPFallback(t *testing.T) {
	tests := []struct {
		name, desc      string
		bootstrapStatus int
		bootstrapBody   string
		status          int
		body            string
		wantErr         error
		whoisBody       string
	}{
		{name: "bootstrap_error", desc: "An IANA error allows WHOIS fallback", bootstrapStatus: 503},
		{name: "bootstrap_json", desc: "Invalid IANA JSON allows WHOIS fallback", bootstrapBody: "{"},
		{name: "bootstrap_missing_services", desc: "Missing services do not constitute a successful bootstrap", bootstrapBody: "{}"},
		{name: "no_provider", desc: "A missing prefix allows WHOIS fallback", bootstrapBody: `{"services":[]}`},
		{name: "rdap_404", desc: "A missing RDAP record allows WHOIS fallback", status: 404},
		{name: "rdap_json", desc: "A malformed RDAP response is not cached", body: "{"},
		{name: "wrong_range", desc: "An unrelated network in the RDAP response is rejected", body: ipRDAPFixture("v4", "198.51.100.0", "198.51.100.255")},
		{name: "wrong_family", desc: "An IPv6 response to an IPv4 query is rejected", body: ipRDAPFixture("v6", "2001:db8::", "2001:db8::ffff")},
		{name: "wrong_class", desc: "A domain object is not treated as a network", body: domainJSON},
		{name: "rdap_429", desc: "A registry rate limit stops the lookup without WHOIS fallback", status: 429, wantErr: ErrRateLimited},
		{name: "bootstrap_429", desc: "An IANA rate limit stops the lookup without WHOIS fallback", bootstrapStatus: 429, wantErr: ErrRateLimited},
		{name: "no_registration", desc: "A WHOIS error preserves the RDAP failure cause", status: 503, whoisBody: "% no entries found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			client, _ := ipFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/bootstrap/") {
					if tt.bootstrapStatus != 0 {
						w.WriteHeader(tt.bootstrapStatus)
						return
					}
					if tt.bootstrapBody != "" {
						writeTestResponse(t, w, tt.bootstrapBody)
						return
					}
					writeTestResponse(t, w, ipBootstrapFixture("https://"+r.Host))
					return
				}
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				writeTestResponse(t, w, tt.body)
			})
			body := "inetnum: 192.0.2.0 - 192.0.2.255\nnetname: TEST-NET\ncountry: ZZ\n"
			if tt.whoisBody != "" {
				body = tt.whoisBody
			}
			calls := installIPWHOIS(t, client, map[string]ipWHOISFixture{
				"whois.iana.org":         {query: "192.0.2.42", body: "refer: whois.registry.example\n"},
				"whois.registry.example": {query: "192.0.2.42", body: body},
			})
			info, err := client.LookupIP(t.Context(), "192.0.2.42")
			switch {
			case tt.wantErr != nil:
				if info != nil || !errors.Is(err, tt.wantErr) || calls.Load() != 0 {
					t.Fatalf("LookupIP = %+v, %v; WHOIS calls=%d", info, err, calls.Load())
				}
			case tt.whoisBody != "":
				if info != nil || err == nil || !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "WHOIS") {
					t.Fatalf("LookupIP = %+v, %v", info, err)
				}
			default:
				if err != nil || info.Source != sourceWHOIS || info.Name != "TEST-NET" || calls.Load() != 2 {
					t.Fatalf("LookupIP = %+v, %v; WHOIS calls=%d", info, err, calls.Load())
				}
			}
		})
	}
}

func TestIPBootstrapSelection(t *testing.T) {
	tests := []struct {
		name, desc, services string
		want                 []string
		wantErr              bool
	}{
		{"longest_prefix", "The most specific network takes precedence over service order", `[[["192.0.0.0/16"],["broad"]],[["192.0.2.0/24"],["specific"]],[["0.0.0.0/0"],["default"]]]`, []string{"specific"}, false},
		{"equal_prefix", "Equal prefixes merge endpoints without duplicates", `[[["192.0.2.0/24"],["one"]],[["192.0.2.0/24"],["one","two"]]]`, []string{"one", "two"}, false},
		{"invalid_entries", "Invalid entries are skipped", `[[["invalid"],["bad"]],[["192.0.2.0/24"]],[["192.0.2.0/24"],["valid"]]]`, []string{"valid"}, false},
		{"no_match", "No matching prefix is found", `[[["198.51.100.0/24"],["other"]]]`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			client, calls := ipFixtureClient(t, func(w http.ResponseWriter, _ *http.Request) { writeTestResponse(t, w, `{"services":`+tt.services+`}`) })
			for range 2 {
				servers, err := client.ipServers(t.Context(), netip.MustParseAddr("192.0.2.42"))
				if (err != nil) != tt.wantErr || !reflect.DeepEqual(servers, tt.want) {
					t.Fatalf("servers=%v, err=%v; want %v", servers, err, tt.want)
				}
			}
			if calls.v4.Load() != 1 {
				t.Fatalf("bootstrap calls=%d", calls.v4.Load())
			}
		})
	}
}

func TestIPBootstrapConcurrencyAndRetry(t *testing.T) {
	tests := []struct {
		name, desc string
		failFirst  bool
	}{
		{"concurrent", "Concurrent requests share one bootstrap download", false},
		{"retry", "A download error is not cached permanently", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var attempts atomic.Int32
			client, calls := ipFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) == 1 && tt.failFirst {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				writeTestResponse(t, w, ipBootstrapFixture("https://"+r.Host))
			})
			if tt.failFirst {
				if _, err := client.ipServers(t.Context(), netip.MustParseAddr("192.0.2.42")); err == nil {
					t.Fatal("expected bootstrap error")
				}
			}
			var wg sync.WaitGroup
			for range 12 {
				wg.Go(func() {
					servers, err := client.ipServers(t.Context(), netip.MustParseAddr("192.0.2.42"))
					if err != nil || len(servers) != 1 {
						t.Errorf("servers=%v err=%v", servers, err)
					}
				})
			}
			wg.Wait()
			want := int32(1)
			if tt.failFirst {
				want++
			}
			if calls.v4.Load() != want {
				t.Fatalf("calls=%d; want %d", calls.v4.Load(), want)
			}
		})
	}
}

func TestIPRangeCIDRs(t *testing.T) {
	tests := []struct {
		name, desc, start, end string
		want                   []string
	}{
		{"single_v4", "A single IPv4 address", "192.0.2.42", "192.0.2.42", []string{"192.0.2.42/32"}},
		{"uneven", "Multiple CIDRs cover the range exactly", "192.0.2.1", "192.0.2.6", []string{"192.0.2.1/32", "192.0.2.2/31", "192.0.2.4/31", "192.0.2.6/32"}},
		{"all_v4", "The full IPv4 range does not overflow", "0.0.0.0", "255.255.255.255", []string{"0.0.0.0/0"}},
		{"all_v6", "The full IPv6 range does not overflow", "::", "ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", []string{"::/0"}},
		{"single_v6", "A single IPv6 address", "2001:db8::1", "2001:db8::1", []string{"2001:db8::1/128"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			got := rangeCIDRs(netip.MustParseAddr(tt.start), netip.MustParseAddr(tt.end))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("CIDRs=%v; want %v", got, tt.want)
			}
		})
	}
}

func TestIPWHOISFormats(t *testing.T) {
	tests := []struct {
		name, desc, ip, body, network, organization, cidr string
		wantErr                                           bool
	}{
		{"arin", "An ARIN range includes organization details", "192.0.2.42", "NetRange: 192.0.2.0 - 192.0.2.255\nNetName: TEST\nOrganization: Example\nRegDate: 2001-01-01\nUpdated: 2025-01-01\n\nOrgAbuseEmail: abuse@example.test\n", "TEST", "Example", "192.0.2.0/24", false},
		{"ripe", "RIPE includes a separate organisation object", "192.0.2.42", "inetnum: 192.0.2.0 - 192.0.2.255\nnetname: TEST\norg: ORG-TEST\n\norganisation: ORG-TEST\norg-name: Example\n\nrole: Abuse\nabuse-mailbox: abuse@example.test\n", "TEST", "Example", "192.0.2.0/24", false},
		{"apnic", "APNIC includes an IPv6 prefix", "2001:db8::42", "inet6num: 2001:db8::/32\nnetname: TEST\ncountry: ZZ\n", "TEST", "", "2001:db8::/32", false},
		{"lacnic", "LACNIC includes an owner and compact dates", "192.0.2.42", "inetnum: 192.0.2.0/24\nowner: Example\ncreated: 20010101\nchanged: 20250101\n", "", "Example", "192.0.2.0/24", false},
		{"afrinic", "AFRINIC includes an inetnum range", "192.0.2.42", "inetnum: 192.0.2.0 - 192.0.2.255\nnetname: TEST\nstatus: ASSIGNED PA\n", "TEST", "", "192.0.2.0/24", false},
		{"specific", "The most specific network in the response is selected", "192.0.2.42", "NetRange: 192.0.0.0 - 192.0.255.255\nNetName: PARENT\n\nNetRange: 192.0.2.0 - 192.0.2.255\nNetName: TEST\n", "TEST", "", "192.0.2.0/24", false},
		{"no_data", "A rate-limit message is not treated as IPInfo", "192.0.2.42", "Query limit reached", "", "", "", true},
		{"other_network", "The range must contain the address", "192.0.2.42", "inetnum: 198.51.100.0 - 198.51.100.255", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			info, err := parseIPWHOIS(tt.body, netip.MustParseAddr(tt.ip), "whois.example")
			if (err != nil) != tt.wantErr {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			if tt.wantErr {
				return
			}
			if info.Name != tt.network || info.Organization != tt.organization || !reflect.DeepEqual(info.CIDRs, []string{tt.cidr}) || info.Raw != tt.body {
				t.Fatalf("info=%+v", info)
			}
			if tt.name == "arin" || tt.name == "lacnic" {
				if info.CreatedAt == nil || info.CreatedAt.Year() != 2001 || info.UpdatedAt == nil || info.UpdatedAt.Year() != 2025 {
					t.Fatalf("dates: %+v", info)
				}
			}
			if tt.name == "arin" || tt.name == "ripe" {
				if len(info.Contacts) != 1 || info.Contacts[0].Emails[0] != "abuse@example.test" {
					t.Fatalf("contacts=%+v", info.Contacts)
				}
			}
		})
	}
}

func TestIPWHOISReferrals(t *testing.T) {
	tests := []struct {
		name, desc, referral string
		arin                 bool
		wantErr              string
	}{
		{"hostname", "A standard server from IANA", "whois.registry.example", false, ""},
		{"arin", "ARIN receives a full network object query", "whois.arin.net", true, ""},
		{"url", "A WHOIS URI specifies port 43 explicitly", "whois://whois.registry.example:43/", false, ""},
		{"loop", "A referral loop is stopped", "whois.iana.org", false, "loop"},
		{"bad_scheme", "An HTTP URL is not sent over TCP WHOIS", "https://whois.registry.example", false, "invalid WHOIS referral"},
		{"bad_port", "A different port is rejected", "whois://whois.registry.example:80", false, "invalid WHOIS referral"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			client := NewClient(WithRateLimiter(testRateLimiter(t)))
			t.Cleanup(func() { _ = client.http.Close() })
			query, host := "192.0.2.42", "whois.registry.example"
			if tt.arin {
				query, host = "n + "+query, "whois.arin.net"
			}
			installIPWHOIS(t, client, map[string]ipWHOISFixture{
				"whois.iana.org": {query: "192.0.2.42", body: "refer: " + tt.referral + "\n"},
				host:             {query: query, body: "inetnum: 192.0.2.0/24\n"},
			})
			info, err := client.lookupIPWHOIS(t.Context(), netip.MustParseAddr("192.0.2.42"))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("info=%+v err=%v", info, err)
				}
			} else if err != nil || info.Registry.WHOISServer != host {
				t.Fatalf("info=%+v err=%v", info, err)
			}
		})
	}
}

type ipFixtureCalls struct{ v4, v6, rdap atomic.Int32 }
type ipWHOISFixture struct{ query, body string }

func ipFixtureClient(t *testing.T, handler http.HandlerFunc, options ...Option) (*Client, *ipFixtureCalls) {
	t.Helper()
	calls := &ipFixtureCalls{}
	var base string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bootstrap/ipv4":
			calls.v4.Add(1)
		case "/bootstrap/ipv6":
			calls.v6.Add(1)
		default:
			calls.rdap.Add(1)
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	base = server.URL
	h := resty.New().SetTransport(server.Client().Transport)
	transport := h.Transport()
	h.SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		path := ""
		switch r.URL.String() {
		case ipv4BootstrapURL:
			path = "/bootstrap/ipv4"
		case ipv6BootstrapURL:
			path = "/bootstrap/ipv6"
		}
		if path != "" {
			r = r.Clone(r.Context())
			var err error
			r.URL, err = url.Parse(base + path)
			if err != nil {
				return nil, err
			}
		}
		if "https://"+r.URL.Host != base {
			return nil, fmt.Errorf("unexpected external request: %s", r.URL)
		}
		r.Host = r.URL.Host
		return transport.RoundTrip(r)
	}))
	options = append([]Option{WithHTTPClient(h), WithRateLimiter(testRateLimiter(t))}, options...)
	client := NewClient(options...)
	t.Cleanup(func() { _ = client.http.Close() })
	client.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Error("unexpected WHOIS call")
		return nil, errors.New("unexpected WHOIS")
	}
	return client, calls
}

func ipBootstrapFixture(base string) string {
	return `{"services":[[["192.0.2.0/24","2001:db8::/32"],["` + base + `/registry/"]]]}`
}

func ipRDAPFixture(version, start, end string) string {
	return fmt.Sprintf(`{"objectClassName":"ip network","ipVersion":%q,"startAddress":%q,"endAddress":%q,
	"handle":"NET-TEST","name":"TEST-NET","type":"ALLOCATED","country":"ZZ","parentHandle":"PARENT","port43":"whois.registry.example","status":["active","active"],
	"events":[{"eventAction":"registration","eventDate":"2001-01-02T03:04:05Z"},{"eventAction":"last changed","eventDate":"2025-01-02T03:04:05Z"}],
	"entities":[{"handle":"ORG-TEST","roles":["registrant"],"vcardArray":["vcard",[["fn",{},"text","Example Network"]]],
	"entities":[{"handle":"ABUSE","roles":["abuse"],"vcardArray":["vcard",[["email",{},"text","abuse@example.test"],["tel",{},"uri","tel:+123456"]]]}]}]}`, version, start, end)
}

func installIPWHOIS(t *testing.T, client *Client, fixtures map[string]ipWHOISFixture) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	client.dialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		calls.Add(1)
		host, port, err := net.SplitHostPort(address)
		fixture, ok := fixtures[host]
		if err != nil || network != "tcp" || port != "43" || !ok {
			t.Errorf("unexpected WHOIS address=%s network=%s", address, network)
			return nil, errors.New("unexpected WHOIS address")
		}
		local, remote := net.Pipe()
		go func() {
			defer remote.Close()
			if deadlineErr := remote.SetDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
				t.Error(deadlineErr)
				return
			}
			query, readErr := bufio.NewReader(remote).ReadString('\n')
			if readErr != nil {
				t.Error(readErr)
				return
			}
			if query != fixture.query+"\r\n" {
				t.Errorf("query=%q; want %q", query, fixture.query)
			}
			writeTestResponse(t, remote, fixture.body)
		}()
		return local, nil
	}
	return &calls
}
