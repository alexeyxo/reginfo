package reginfo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"resty.dev/v3"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestRateLimiterWait(t *testing.T) {
	type args struct {
		server   string
		requests int
		limits   []RateLimit
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    time.Duration
		wantErr error
	}{
		{name: "http_default", desc: "Unknown HTTP services use one start per second", args: args{server: "https://rdap.example", requests: 3}, want: 2 * time.Second},
		{name: "iana_whois_default", desc: "IANA WHOIS uses the conservative default", args: args{server: "whois://whois.iana.org", requests: 3}, want: 2 * time.Second},
		{name: "tcinet", desc: "TCI requests are spread within its 120/minute quota", args: args{server: "whois://whois.tcinet.ru", requests: 121}, want: time.Minute},
		{name: "nominet_second", desc: "Nominet WHOIS permits five starts per second", args: args{server: "whois://whois.nic.uk", requests: 6}, want: time.Second},
		{name: "nominet_day", desc: "Nominet WHOIS also enforces its rolling daily quota", args: args{server: "whois://whois.nic.uk", requests: 1001}, want: 24 * time.Hour},
		{name: "maxmind_day", desc: "GeoLite downloads share a rolling quota of thirty per day", args: args{server: "https://download.maxmind.com", requests: 31}, want: 24 * time.Hour},
		{name: "multiple_windows", desc: "All configured rolling windows apply together", args: args{server: "https://rdap.example", requests: 5, limits: []RateLimit{{Requests: 2, Interval: time.Second}, {Requests: 3, Interval: 10 * time.Second}}}, want: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				limiter := NewDefaultRateLimiter()
				if tt.args.limits != nil {
					var err error
					limiter, err = NewRateLimiter(RateLimitConfig{
						Servers: map[string][]RateLimit{tt.args.server: tt.args.limits},
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				start := time.Now()
				for range tt.args.requests {
					if err := limiter.wait(t.Context(), tt.args.server); !errors.Is(err, tt.wantErr) {
						t.Fatalf("wait error = %v; want %v", err, tt.wantErr)
					}
				}
				if got := time.Since(start); got != tt.want {
					t.Fatalf("elapsed = %v; want %v", got, tt.want)
				}
			})
		})
	}
}

func TestRateLimiterWaitCancellation(t *testing.T) {
	type args struct {
		cancel  bool
		timeout time.Duration
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    time.Duration
		wantErr error
	}{
		{name: "canceled_waiter", desc: "Cancellation stops a queued request without reserving future capacity", args: args{cancel: true}, want: time.Second, wantErr: context.Canceled},
		{name: "deadline", desc: "An expiring context stops a queued request without consuming quota", args: args{timeout: 100 * time.Millisecond}, want: time.Second, wantErr: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				var limiter RateLimiter
				const server = "https://rdap.example"
				start := time.Now()
				if err := limiter.wait(t.Context(), server); err != nil {
					t.Fatal(err)
				}
				var ctx context.Context
				var cancel context.CancelFunc
				if tt.args.cancel {
					ctx, cancel = context.WithCancel(t.Context())
				} else {
					ctx, cancel = context.WithTimeout(t.Context(), tt.args.timeout)
				}
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() { done <- limiter.wait(ctx, server) }()
				synctest.Wait()
				if tt.args.cancel {
					cancel()
				}
				if err := <-done; !errors.Is(err, tt.wantErr) {
					t.Fatalf("wait error = %v; want %v", err, tt.wantErr)
				}
				if err := limiter.wait(t.Context(), server); err != nil {
					t.Fatal(err)
				}
				if got := time.Since(start); got != tt.want {
					t.Fatalf("next request at %v; want %v", got, tt.want)
				}
			})
		})
	}
}

func TestRateLimiterConcurrentWaits(t *testing.T) {
	type args struct {
		requests int
		servers  []string
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    []time.Duration
		wantErr error
	}{
		{name: "same_server", desc: "Concurrent callers cannot exceed the shared server quota", args: args{requests: 4, servers: []string{"https://one.example"}}, want: []time.Duration{0, time.Second, 2 * time.Second, 3 * time.Second}},
		{name: "independent_servers", desc: "A busy server does not delay other servers", args: args{requests: 4, servers: []string{"https://one.example", "https://two.example"}}, want: []time.Duration{0, 0, time.Second, time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				var limiter RateLimiter
				start := time.Now()
				done := make(chan time.Duration, tt.args.requests)
				var wg sync.WaitGroup
				for i := range tt.args.requests {
					wg.Go(func() {
						err := limiter.wait(t.Context(), tt.args.servers[i%len(tt.args.servers)])
						if !errors.Is(err, tt.wantErr) {
							t.Errorf("wait error = %v; want %v", err, tt.wantErr)
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
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("request times = %v; want %v", got, tt.want)
				}
			})
		})
	}
}

func TestRateLimiterCooldown(t *testing.T) {
	type args struct{ nextCooldown time.Duration }
	tests := []struct {
		name    string
		desc    string
		args    args
		want    time.Duration
		wantErr error
	}{
		{name: "extend", desc: "An existing waiter observes an extended server cooldown", args: args{nextCooldown: 5 * time.Second}, want: 6 * time.Second},
		{name: "never_shorten", desc: "A shorter Retry-After cannot clear an existing longer cooldown", args: args{nextCooldown: time.Second}, want: 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				var limiter RateLimiter
				const server = "https://rdap.example"
				start := time.Now()
				if _, err := limiter.block(t.Context(), server, start.Add(3*time.Second)); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- limiter.wait(t.Context(), server) }()
				time.Sleep(time.Second)
				if _, err := limiter.block(t.Context(), server, time.Now().Add(tt.args.nextCooldown)); err != nil {
					t.Fatal(err)
				}
				if err := <-done; !errors.Is(err, tt.wantErr) {
					t.Fatalf("wait error = %v; want %v", err, tt.wantErr)
				}
				if got := time.Since(start); got != tt.want {
					t.Fatalf("request at %v; want %v", got, tt.want)
				}
			})
		})
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		desc string
		args string
		want time.Duration
	}{
		{name: "seconds", desc: "Delta seconds are accepted", args: " 10 ", want: 10 * time.Second},
		{name: "date", desc: "HTTP dates are accepted", args: now.Add(time.Minute).Format(http.TimeFormat), want: time.Minute},
		{name: "past_date", desc: "Past HTTP dates cause no additional cooldown", args: now.Add(-time.Minute).Format(http.TimeFormat)},
		{name: "zero", desc: "Zero is a valid Retry-After", args: "0"},
		{name: "missing", desc: "Missing headers use local backoff", want: time.Minute},
		{name: "invalid", desc: "Malformed headers use local backoff", args: "invalid", want: time.Minute},
		{name: "negative", desc: "Negative seconds are not valid", args: "-1", want: time.Minute},
		{name: "overflow", desc: "Large values never overflow into an immediate retry", args: "99999999999999999999999", want: time.Duration(1<<63 - 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			if got := retryAfter(tt.args, now).Sub(now); got != tt.want {
				t.Fatalf("Retry-After = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestHTTPRateLimitAcrossClients(t *testing.T) {
	type args struct {
		useDefault   bool
		independent  bool
		secondScheme string
		secondHost   string
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    time.Duration
		wantErr error
	}{
		{name: "process_default", desc: "Default clients share one server budget across paths", args: args{useDefault: true, secondScheme: "https"}, want: time.Second},
		{name: "explicit_default_shared", desc: "Clients share an explicitly supplied default limiter", args: args{secondScheme: "https"}, want: time.Second},
		{name: "http_https", desc: "Changing scheme, case or trailing dot does not create a new quota", args: args{secondScheme: "http"}, want: time.Second},
		{name: "different_hosts", desc: "Independent servers have separate quotas", args: args{secondScheme: "https", secondHost: "other.example"}},
		{name: "independent_limiters", desc: "One Resty client preserves separate limiters for its components", args: args{independent: true, secondScheme: "https"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				host := fmt.Sprintf("shared-%d.example", rateTestID.Add(1))
				var limiter *RateLimiter
				if !tt.args.useDefault {
					limiter = NewDefaultRateLimiter()
				}
				h := resty.New().SetTransport(roundTripFunc(func(_ *http.Request) (*http.Response, error) { return rateTestResponse(200, "", "ok"), nil }))
				first := NewClient(WithHTTPClient(h), WithRateLimiter(limiter))
				secondLimiter := limiter
				if tt.args.independent {
					secondLimiter = NewDefaultRateLimiter()
				}
				second := NewClient(WithHTTPClient(h), WithRateLimiter(secondLimiter))
				start := time.Now()
				if _, err := first.get(t.Context(), "https://"+host+"/one", 10); err != nil {
					t.Fatal(err)
				}
				secondHost := strings.ToUpper(host) + "."
				if tt.args.secondHost != "" {
					secondHost = tt.args.secondHost
				}
				_, err := second.get(t.Context(), tt.args.secondScheme+"://"+secondHost+"/two", 10)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("GET error = %v; want %v", err, tt.wantErr)
				}
				if got := time.Since(start); got != tt.want {
					t.Fatalf("elapsed = %v; want %v", got, tt.want)
				}
				transport, ok := h.Transport().(*clientTransport)
				if !ok {
					t.Fatal("Resty transport is not configured")
				}
				if _, ok = transport.base.(roundTripFunc); !ok {
					t.Fatal("Resty transport was wrapped more than once")
				}
			})
		})
	}
}

func TestHTTPRetryAfter(t *testing.T) {
	type args struct {
		header   string
		redirect bool
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    time.Duration
		wantErr error
	}{
		{name: "seconds", desc: "429 installs a cooldown shared by subsequent clients", args: args{header: "3"}, want: 3 * time.Second, wantErr: ErrRateLimited},
		{name: "http_date", desc: "HTTP-date cooldowns are enforced", args: args{header: "Sat, 01 Jan 2000 00:00:03 GMT"}, want: 3 * time.Second, wantErr: ErrRateLimited},
		{name: "missing_header", desc: "Missing Retry-After installs the fallback cooldown", want: time.Minute, wantErr: ErrRateLimited},
		{name: "redirect", desc: "The final redirect target gets its own cooldown", args: args{header: "3", redirect: true}, want: 3 * time.Second, wantErr: ErrRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				const target = "https://registry.example/domain/example.com"
				limiter := &RateLimiter{}
				var requests int
				body := &rateTestBody{Reader: strings.NewReader("limited")}
				h := resty.New().SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Hostname() == "redirect.example" {
						response := rateTestResponse(302, "", "")
						response.Header.Set("Location", target)
						return response, nil
					}
					requests++
					if requests == 1 {
						response := rateTestResponse(429, tt.args.header, "")
						response.Body = body
						return response, nil
					}
					return rateTestResponse(200, "", "ok"), nil
				}))
				first := NewClient(WithHTTPClient(h), WithRateLimiter(limiter))
				second := NewClient(WithHTTPClient(h), WithRateLimiter(limiter))
				endpoint := target
				if tt.args.redirect {
					endpoint = "https://redirect.example/domain/example.com"
				}
				start := time.Now()
				_, err := first.get(t.Context(), endpoint, 20)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("GET error = %v; want %v", err, tt.wantErr)
				}
				var limited *RateLimitError
				if !errors.As(err, &limited) || limited.Server != "https://registry.example" || !limited.RetryAt.Equal(start.Add(tt.want)) {
					t.Fatalf("RateLimitError = %+v", limited)
				}
				if !body.closed || requests != 1 {
					t.Fatalf("closed = %v; requests = %d; want closed body and no automatic retry", body.closed, requests)
				}
				if _, nextErr := second.get(t.Context(), target, 20); nextErr != nil {
					t.Fatal(nextErr)
				}
				if elapsed := time.Since(start); elapsed != tt.want {
					t.Fatalf("next request at %v; want %v", elapsed, tt.want)
				}
			})
		})
	}
}

func TestLookupRateLimitStopsFallback(t *testing.T) {
	tests := []struct {
		name    string
		desc    string
		args    bool
		want    int
		wantErr error
	}{
		{name: "bootstrap", desc: "An IANA 429 does not trigger WHOIS discovery", args: true, want: 1, wantErr: ErrRateLimited},
		{name: "rdap", desc: "An RDAP 429 does not trigger another endpoint or WHOIS", want: 2, wantErr: ErrRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var calls int
			h := resty.New().SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() == bootstrapURL && !tt.args {
					return rateTestResponse(200, "", `{"services":[[["com"],["https://registry.example/","https://alternate.example/"]]]}`), nil
				}
				return rateTestResponse(429, "30", "limited"), nil
			}))
			c := NewClient(WithHTTPClient(h), WithRateLimiter(&RateLimiter{}))
			c.dialContext = func(context.Context, string, string) (net.Conn, error) {
				t.Error("429 caused a WHOIS request")
				return nil, errors.New("unexpected WHOIS request")
			}
			info, err := c.Lookup(t.Context(), "example.com")
			if !errors.Is(err, tt.wantErr) || info != nil {
				t.Fatalf("Lookup = %+v, %v; want %v", info, err, tt.wantErr)
			}
			if calls != tt.want {
				t.Fatalf("HTTP requests = %d; want %d", calls, tt.want)
			}
		})
	}
}

func TestWHOISRateLimiting(t *testing.T) {
	type args struct {
		server  string
		timeout time.Duration
	}
	tests := []struct {
		name      string
		desc      string
		args      args
		want      time.Duration
		wantDials int
		wantErr   error
	}{
		{name: "shared_server", desc: "Different WHOIS queries share their server budget", args: args{server: "whois.example"}, want: time.Second, wantDials: 2},
		{name: "independent_server", desc: "A different WHOIS server can start immediately", args: args{server: "other.example"}, wantDials: 2},
		{name: "deadline_before_dial", desc: "A queued WHOIS request can expire without opening a connection", args: args{server: "whois.example", timeout: 100 * time.Millisecond}, want: 100 * time.Millisecond, wantDials: 1, wantErr: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			synctest.Test(t, func(t *testing.T) {
				c := NewClient(WithRateLimiter(&RateLimiter{}))
				var dials int
				c.dialContext = func(context.Context, string, string) (net.Conn, error) {
					dials++
					client, server := net.Pipe()
					t.Cleanup(func() { _ = client.Close() })
					t.Cleanup(func() { _ = server.Close() })
					go func() {
						defer server.Close()
						data := make([]byte, 256)
						if _, err := server.Read(data); err != nil {
							t.Error(err)
							return
						}
						writeTestResponse(t, server, "Registrar: Example\n")
					}()
					return client, nil
				}
				start := time.Now()
				if _, err := c.whois(t.Context(), "whois.example", "example.com"); err != nil {
					t.Fatal(err)
				}
				ctx := t.Context()
				if tt.args.timeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tt.args.timeout)
					t.Cleanup(cancel)
				}
				_, err := c.whois(ctx, tt.args.server, "example.net")
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("WHOIS error = %v; want %v", err, tt.wantErr)
				}
				if elapsed := time.Since(start); elapsed != tt.want || dials != tt.wantDials {
					t.Fatalf("elapsed = %v, dials = %d; want %v, %d", elapsed, dials, tt.want, tt.wantDials)
				}
			})
		})
	}
}

var rateTestID atomic.Uint64

type rateTestBody struct {
	*strings.Reader
	closed bool
}

func (b *rateTestBody) Close() error { b.closed = true; return nil }

func rateTestResponse(status int, retry, body string) *http.Response {
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	if retry != "" {
		response.Header.Set("Retry-After", retry)
	}
	return response
}
