package reginfo

import (
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"resty.dev/v3"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPGetResponses(t *testing.T) {
	type args struct {
		body         string
		contentType  string
		status       int
		max          int64
		gzip         bool
		chunked      bool
		redirect     bool
		customClient bool
	}
	type want struct {
		body  string
		calls int32
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    want
		wantErr string
	}{
		{
			name: "json", desc: "Resty returns the original JSON without modification",
			args: args{body: " {\n\"ldhName\":\"example.com\"}\n", contentType: "application/rdap+json", status: 200, max: 100},
			want: want{body: " {\n\"ldhName\":\"example.com\"}\n", calls: 1},
		},
		{
			name: "custom_client", desc: "The supplied HTTP client uses Resty",
			args: args{body: "COM\nRU\n", contentType: "text/plain", status: 200, max: 100, customClient: true},
			want: want{body: "COM\nRU\n", calls: 1},
		},
		{
			name: "malformed_json", desc: "JSON validation remains in the RDAP parser",
			args: args{body: "{broken", contentType: "application/json", status: 200, max: 100},
			want: want{body: "{broken", calls: 1},
		},
		{
			name: "empty", desc: "An empty body is read without a transport error",
			args: args{status: 200, max: 100}, want: want{calls: 1},
		},
		{
			name: "at_limit", desc: "A response exactly at the size limit is returned in full",
			args: args{body: "1234", status: 200, max: 4}, want: want{body: "1234", calls: 1},
		},
		{
			name: "over_limit", desc: "An oversized response does not return a truncated body",
			args: args{body: "12345", status: 200, max: 4}, want: want{calls: 1}, wantErr: "response exceeds 4 bytes",
		},
		{
			name: "chunked_over_limit", desc: "The limit applies without Content-Length",
			args: args{body: "12345", status: 200, max: 4, chunked: true}, want: want{calls: 1}, wantErr: "response exceeds 4 bytes",
		},
		{
			name: "gzip", desc: "A compressed response is decompressed before reading",
			args: args{body: "1234", status: 200, max: 4, gzip: true}, want: want{body: "1234", calls: 1},
		},
		{
			name: "gzip_over_limit", desc: "The size limit applies to the decompressed response",
			args: args{body: strings.Repeat("a", 1024), status: 200, max: 100, gzip: true},
			want: want{calls: 1}, wantErr: "response exceeds 100 bytes",
		},
		{
			name: "redirect", desc: "Redirects preserve headers",
			args: args{body: "ok", status: 200, max: 100, redirect: true}, want: want{body: "ok", calls: 2},
		},
		{
			name: "server_error", desc: "A server error is returned without an automatic retry",
			args: args{body: "unavailable", status: 503, max: 100}, want: want{calls: 1}, wantErr: "HTTP 503",
		},
		{
			name: "rate_limited", desc: "A 429 response is handled by the existing limiter without a retry",
			args: args{body: "limited", status: 429, max: 100}, want: want{calls: 1}, wantErr: ErrRateLimited.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.Header.Get("User-Agent") != "reginfo-go/1.0" || r.Header.Get("Accept") != "application/rdap+json, application/json" {
					t.Errorf("unexpected request: %s, headers: %v", r.Method, r.Header)
				}
				if tt.args.redirect && r.URL.Path != "/final" {
					http.Redirect(w, r, "/final", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", tt.args.contentType)
				if tt.args.gzip {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(tt.args.status)
				if tt.args.chunked {
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Error(err)
						return
					}
				}
				if tt.args.gzip {
					writer := gzip.NewWriter(w)
					writeTestResponse(t, writer, tt.args.body)
					_ = writer.Close()
					return
				}
				writeTestResponse(t, w, tt.args.body)
			}))
			t.Cleanup(server.Close)
			options := []Option{WithRateLimiter(testRateLimiter(t))}
			if tt.args.customClient {
				options = append(options, WithHTTPClient(resty.New().SetTransport(server.Client().Transport)))
			}
			client := NewClient(options...)
			t.Cleanup(func() { _ = client.http.Close() })
			body, err := client.get(t.Context(), server.URL, tt.args.max)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("GET error = %v; want %q", err, tt.wantErr)
			}
			if tt.args.status == http.StatusTooManyRequests && !errors.Is(err, ErrRateLimited) {
				t.Fatalf("GET error = %v; want ErrRateLimited", err)
			}
			if string(body) != tt.want.body || calls.Load() != tt.want.calls {
				t.Fatalf("body = %q, calls = %d; want %+v", body, calls.Load(), tt.want)
			}
		})
	}
}

func TestHTTPGetBodyCancellation(t *testing.T) {
	type args struct {
		restyTimeout   time.Duration
		customTimeout  time.Duration
		contextTimeout time.Duration
		cancel         bool
	}
	tests := []struct {
		name    string
		desc    string
		args    args
		want    []byte
		wantErr error
	}{
		{
			name: "resty_timeout", desc: "The Resty timeout applies while reading the body",
			args: args{restyTimeout: 100 * time.Millisecond}, wantErr: context.DeadlineExceeded,
		},
		{
			name: "custom_resty_timeout", desc: "The supplied Resty client's timeout is preserved",
			args: args{customTimeout: 100 * time.Millisecond}, wantErr: context.DeadlineExceeded,
		},
		{
			name: "context_deadline", desc: "The context deadline interrupts body reading",
			args: args{contextTimeout: 100 * time.Millisecond}, wantErr: context.DeadlineExceeded,
		},
		{
			name: "cancel", desc: "Context cancellation interrupts body reading",
			args: args{cancel: true}, wantErr: context.Canceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if tt.args.contextTimeout > 0 {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, tt.args.contextTimeout)
				t.Cleanup(stop)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				writeTestResponse(t, w, "partial")
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
					return
				}
				if tt.args.cancel {
					cancel()
				}
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			options := []Option{WithRateLimiter(testRateLimiter(t))}
			if tt.args.customTimeout > 0 {
				options = append(options, WithHTTPClient(resty.New().SetTimeout(tt.args.customTimeout)))
			}
			client := NewClient(options...)
			t.Cleanup(func() { _ = client.http.Close() })
			if tt.args.restyTimeout > 0 {
				client.http.SetTimeout(tt.args.restyTimeout)
			}
			body, err := client.get(ctx, server.URL, 100)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("GET error = %v; want %v", err, tt.wantErr)
			}
			if body != nil {
				t.Fatalf("body = %q; want %v", body, tt.want)
			}
		})
	}
}
