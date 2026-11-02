package reginfo

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"resty.dev/v3"
)

func TestRestyClientSettings(t *testing.T) {
	tests := []struct {
		name, desc string
		geoIP      bool
		status     int
	}{
		{name: "registry", desc: "Registry requests preserve Resty settings and middleware", status: http.StatusOK},
		{name: "geoip", desc: "GeoIP downloads preserve Resty settings and middleware", geoIP: true, status: http.StatusOK},
		{name: "registry_no_retry", desc: "Registry requests are not retried despite the Resty setting", status: http.StatusServiceUnavailable},
		{name: "geoip_no_retry", desc: "GeoIP requests are not retried despite the Resty setting", geoIP: true, status: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var calls, middlewareCalls atomic.Int32
			archive := geoIPTestArchive(t, "GeoLite2-City", []byte("database"))
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("X-Custom") != "preserved" || r.Header.Get("X-Middleware") != "active" || r.URL.Query().Get("custom") != "value" {
					t.Errorf("Resty settings missing: %s, %v", r.URL, r.Header)
				}
				w.WriteHeader(tt.status)
				if tt.geoIP {
					if _, err := w.Write(archive); err != nil {
						t.Error(err)
					}
				} else {
					writeTestResponse(t, w, "ok")
				}
			}))
			t.Cleanup(server.Close)
			h := restyFixtureClient(t, server).
				SetHeader("X-Custom", "preserved").SetHeader("User-Agent", "caller").
				SetHeader("Accept", "text/plain").SetQueryParam("custom", "value").
				SetTimeout(time.Second).SetRetryCount(2)
			h.AddRequestMiddleware(func(_ *resty.Client, req *resty.Request) error {
				middlewareCalls.Add(1)
				if !req.IsResponseDoNotParse || req.RetryCount != 0 {
					t.Errorf("streaming=%v retries=%d", req.IsResponseDoNotParse, req.RetryCount)
				}
				req.SetHeader("X-Middleware", "active")
				return nil
			})
			var err error
			if tt.geoIP {
				geo := newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir(), HTTPClient: h, AccountID: "account", LicenseKey: "key", RateLimiter: testRateLimiter(t)})
				err = geo.downloadDatabase(t.Context(), "GeoLite2-City", t.TempDir())
			} else {
				client := NewClient(WithHTTPClient(h), WithRateLimiter(testRateLimiter(t)))
				_, err = client.get(t.Context(), server.URL, 100)
			}
			if (err != nil) != (tt.status != http.StatusOK) {
				t.Fatalf("request error=%v", err)
			}
			if calls.Load() != 1 || middlewareCalls.Load() != 1 {
				t.Fatalf("requests=%d middleware=%d; want one each", calls.Load(), middlewareCalls.Load())
			}
			if h.RetryCount() != 2 || h.Timeout() != time.Second || h.Header().Get("User-Agent") != "caller" || h.Header().Get("Accept") != "text/plain" {
				t.Fatal("Resty defaults were overwritten")
			}
		})
	}
}

func TestSharedRestyRedirects(t *testing.T) {
	tests := []struct {
		name, desc      string
		geoFirst        bool
		sharedTransport bool
	}{
		{name: "registry_first", desc: "GeoIP rules do not change registry redirects"},
		{name: "geoip_first", desc: "Client construction order does not change redirect rules", geoFirst: true},
		{name: "shared_transport", desc: "Separate Resty clients sharing a transport retain their own redirect rules", sharedTransport: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var finalCalls, redirects atomic.Int32
			final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				finalCalls.Add(1)
				writeTestResponse(t, w, "ok")
			}))
			t.Cleanup(final.Close)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, final.URL, http.StatusFound)
			}))
			t.Cleanup(server.Close)
			policy := resty.RedirectPolicyFunc(func(_ *http.Request, _ []*http.Request) error {
				redirects.Add(1)
				return nil
			})
			h := restyFixtureClient(t, server).SetRedirectPolicy(policy)
			config := GeoIPConfig{Directory: t.TempDir(), HTTPClient: h, AccountID: "account", LicenseKey: "key", RateLimiter: testRateLimiter(t)}
			var geo *GeoIP
			if tt.geoFirst {
				geo = newGeoIPTest(t, config)
			}
			client := NewClient(WithHTTPClient(h), WithRateLimiter(testRateLimiter(t)))
			if tt.sharedTransport {
				config.HTTPClient = resty.New().SetTransport(h.Transport()).SetRedirectPolicy(policy)
				t.Cleanup(func() { _ = config.HTTPClient.Close() })
			}
			if !tt.geoFirst {
				geo = newGeoIPTest(t, config)
			}
			if err := geo.downloadDatabase(t.Context(), "GeoLite2-City", t.TempDir()); err == nil || !strings.Contains(err.Error(), "requires HTTPS") {
				t.Fatalf("GeoIP redirect error=%v", err)
			}
			body, err := client.get(t.Context(), server.URL, 100)
			if err != nil || string(body) != "ok" || finalCalls.Load() != 1 || redirects.Load() != 2 {
				t.Fatalf("GET=%q, %v; final=%d redirects=%d", body, err, finalCalls.Load(), redirects.Load())
			}
		})
	}
}

func TestRestyDirectRequests(t *testing.T) {
	tests := []struct {
		name, desc string
		status     int
	}{
		{name: "success", desc: "A direct Resty request preserves headers and automatic response parsing", status: http.StatusOK},
		{name: "rate_limited", desc: "A direct Resty request bypasses the reginfo limiter", status: http.StatusTooManyRequests},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") != "caller" {
					t.Error("direct request headers changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				writeTestResponse(t, w, `{"value":"preserved"}`)
			}))
			t.Cleanup(server.Close)
			h := resty.New().SetHeader("User-Agent", "caller")
			t.Cleanup(func() { _ = h.Close() })
			NewClient(WithHTTPClient(h))
			newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir(), HTTPClient: h})
			var result map[string]string
			response, err := h.R().SetContext(t.Context()).SetResult(&result).Get(server.URL)
			if err != nil || response.StatusCode() != tt.status || errors.Is(err, ErrRateLimited) {
				t.Fatalf("direct request=%v, %v", response, err)
			}
			if tt.status == http.StatusOK && result["value"] != "preserved" {
				t.Fatalf("autoparsing lost: %v", result)
			}
		})
	}
}

func restyFixtureClient(t *testing.T, server *httptest.Server) *resty.Client {
	t.Helper()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := server.Client().Transport
	client := resty.New().SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "download.maxmind.com" {
			req = req.Clone(req.Context())
			req.URL.Host = endpoint.Host
		}
		return base.RoundTrip(req)
	}))
	t.Cleanup(func() { _ = client.Close() })
	return client
}
