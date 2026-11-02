package reginfo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"resty.dev/v3"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGeoIPUpdateDatabases(t *testing.T) {
	tests := []struct {
		name, desc, failure string
		calls               int32
		wantErr             error
	}{
		{name: "success", desc: "Three databases are published as one set and can be reopened after a restart", calls: 3},
		{name: "unauthorized", desc: "An authentication error does not change the active set", failure: "unauthorized", calls: 1},
		{name: "partial_failure", desc: "Failure of the third download does not publish the first two databases", failure: "partial", calls: 3},
		{name: "invalid_mmdb", desc: "MMDB validation precedes publication of the set", failure: "mmdb", calls: 3},
		{name: "rate_limit", desc: "A 429 is returned without a retry while preserving the active databases", failure: "rate_limit", calls: 1, wantErr: ErrRateLimited},
		{name: "cancel_body", desc: "Cancellation interrupts stream reading", failure: "cancel_body", calls: 1, wantErr: context.Canceled},
		{name: "cancel_before", desc: "A canceled context makes no network requests", failure: "cancel_before", wantErr: context.Canceled},
		{name: "credentials", desc: "Local reads work without credentials but downloads are rejected", failure: "credentials"},
		{name: "closed", desc: "A closed component cannot be updated", failure: "closed", wantErr: ErrGeoIPClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			databases := geoIPTestDatabases(t, "New City")
			if tt.failure == "mmdb" {
				databases["GeoLite2-ASN"] = []byte("broken")
			}
			archives := make(map[string][]byte, len(databases))
			for edition, data := range databases {
				archives[edition] = geoIPTestArchive(t, edition, data)
			}
			var calls atomic.Int32
			geo := geoIPDownloadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				account, key, ok := r.BasicAuth()
				if !ok || account != "test-account" || key != "test-key" || r.Method != http.MethodGet || r.URL.Query().Get("suffix") != "tar.gz" {
					t.Error("download request has incorrect authentication, method, or suffix")
				}
				edition := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/geoip/databases/"), "/download")
				if tt.failure == "unauthorized" || (tt.failure == "partial" && edition == "GeoLite2-ASN") {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if tt.failure == "rate_limit" {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if tt.failure == "cancel_body" {
					w.WriteHeader(http.StatusOK)
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Error(err)
					}
					cancel()
					<-r.Context().Done()
					return
				}
				data, exists := archives[edition]
				if !exists {
					t.Errorf("unknown edition %s", edition)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if _, err := w.Write(data); err != nil {
					t.Error(err)
				}
			})
			previous := filepath.Join(geo.config.Directory, geoIPGenerationPrefix+"baseline")
			if err := os.Mkdir(previous, 0o700); err != nil {
				t.Fatal(err)
			}
			writeGeoIPTestDatabases(t, previous, geoIPTestDatabases(t, "Old City"))
			if err := publishGeoIPDirectory(t.Context(), geo.config.Directory, previous); err != nil {
				t.Fatal(err)
			}
			if err := geo.LoadDatabases(t.Context()); err != nil {
				t.Fatal(err)
			}
			switch tt.failure {
			case "credentials":
				geo.config.LicenseKey = ""
			case "cancel_before":
				cancel()
			case "closed":
				if err := geo.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err := geo.UpdateDatabases(ctx)
			if (err != nil) != (tt.failure != "") || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("UpdateDatabases=%v; expected failure=%s, sentinel=%v", err, tt.failure, tt.wantErr)
			}
			if calls.Load() != tt.calls {
				t.Fatalf("requests=%d; want %d", calls.Load(), tt.calls)
			}
			directory, dirErr := geoIPDirectory(geo.config.Directory)
			if dirErr != nil || (directory == previous) != (tt.failure != "") {
				t.Fatalf("CURRENT=%s, %v", directory, dirErr)
			}
			entries, readErr := os.ReadDir(geo.config.Directory)
			if readErr != nil || len(entries) != 2 {
				t.Fatalf("staging files remain: %v, %v", entries, readErr)
			}
			want := "New City"
			if tt.failure != "" {
				want = "Old City"
			}
			if tt.failure != "closed" {
				info, lookupErr := geo.Lookup(t.Context(), "192.0.2.42")
				if lookupErr != nil || info.City != want {
					t.Fatalf("active lookup=%+v, %v", info, lookupErr)
				}
			}
			reopened := newGeoIPTest(t, GeoIPConfig{Directory: geo.config.Directory})
			if loadErr := reopened.LoadDatabases(t.Context()); loadErr != nil {
				t.Fatal(loadErr)
			}
			info, lookupErr := reopened.Lookup(t.Context(), "192.0.2.42")
			if lookupErr != nil || info.City != want {
				t.Fatalf("reopened lookup=%+v, %v", info, lookupErr)
			}
		})
	}
}

func TestGeoIPDownloadRedirects(t *testing.T) {
	tests := []struct {
		name, desc, target string
		wantAuth, wantErr  bool
	}{
		{name: "same_host", desc: "Authorization is preserved on the original host", target: "https://download.maxmind.com/archive", wantAuth: true},
		{name: "storage", desc: "The MaxMind key is not forwarded to the storage host", target: "https://storage.example/archive"},
		{name: "subdomain", desc: "The key is not forwarded even to another subdomain", target: "https://cdn.download.maxmind.com/archive"},
		{name: "http", desc: "A redirect from HTTPS to HTTP is rejected", target: "http://storage.example/archive", wantErr: true},
	}
	data := geoIPTestArchive(t, "GeoLite2-City", geoIPTestDatabases(t, "Old City")["GeoLite2-City"])
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var redirected atomic.Bool
			geo := geoIPDownloadTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/archive" {
					http.Redirect(w, r, tt.target, http.StatusFound)
					return
				}
				redirected.Store(true)
				if (r.Header.Get("Authorization") != "") != tt.wantAuth {
					t.Error("redirect forwarded incorrect authorization")
				}
				if _, err := w.Write(data); err != nil {
					t.Error(err)
				}
			})
			err := geo.downloadDatabase(t.Context(), "GeoLite2-City", t.TempDir())
			if (err != nil) != tt.wantErr || redirected.Load() == tt.wantErr {
				t.Fatalf("download=%v redirected=%v", err, redirected.Load())
			}
		})
	}
}

func TestGeoIPArchiveValidation(t *testing.T) {
	tests := []struct {
		name, desc, filename                   string
		kind                                   byte
		size                                   int64
		duplicate, truncate, checksum, wantErr bool
	}{
		{name: "nested", desc: "Only the requested database is extracted", filename: "GeoLite2-City_20260101/GeoLite2-City.mmdb", kind: tar.TypeReg},
		{name: "traversal", desc: "An archive entry name is not used as the output path", filename: "../../GeoLite2-City.mmdb", kind: tar.TypeReg},
		{name: "wrong_file", desc: "A missing required file is an error", filename: "LICENSE.txt", kind: tar.TypeReg, wantErr: true},
		{name: "symlink", desc: "A symbolic link is not accepted as a database", filename: "GeoLite2-City.mmdb", kind: tar.TypeSymlink, wantErr: true},
		{name: "duplicate", desc: "A duplicate database is rejected", filename: "GeoLite2-City.mmdb", kind: tar.TypeReg, duplicate: true, wantErr: true},
		{name: "oversized", desc: "The file size is checked before allocating space", filename: "GeoLite2-City.mmdb", kind: tar.TypeReg, size: geoIPMaxDatabase + 1, wantErr: true},
		{name: "truncated", desc: "A truncated stream is not treated as a successful download", filename: "GeoLite2-City.mmdb", kind: tar.TypeReg, truncate: true, wantErr: true},
		{name: "checksum", desc: "The gzip checksum is verified after the end of the tar archive", filename: "GeoLite2-City.mmdb", kind: tar.TypeReg, checksum: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			var buffer bytes.Buffer
			compressed := gzip.NewWriter(&buffer)
			archive := tar.NewWriter(compressed)
			count := 1
			if tt.duplicate {
				count++
			}
			for range count {
				size := tt.size
				if size == 0 && tt.kind == tar.TypeReg {
					size = 4
				}
				if err := archive.WriteHeader(&tar.Header{Name: tt.filename, Typeflag: tt.kind, Size: size, Mode: 0o600}); err != nil {
					t.Fatal(err)
				}
				if tt.kind == tar.TypeReg && tt.size == 0 {
					if _, err := archive.Write([]byte("test")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := archive.Close(); err != nil && tt.size == 0 {
				t.Fatal(err)
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			data := buffer.Bytes()
			if tt.truncate {
				data = data[:len(data)-5]
			}
			if tt.checksum {
				data[len(data)-8] ^= 0xff
			}
			directory := t.TempDir()
			err := extractGeoIPDatabase(t.Context(), bytes.NewReader(data), "GeoLite2-City", directory)
			if (err != nil) != tt.wantErr {
				t.Fatalf("extract=%v", err)
			}
			if !tt.wantErr {
				got, readErr := os.ReadFile(filepath.Join(directory, "GeoLite2-City.mmdb"))
				if readErr != nil || string(got) != "test" {
					t.Fatalf("extracted=%q, %v", got, readErr)
				}
			}
		})
	}
}

func geoIPDownloadTestClient(t *testing.T, handler http.HandlerFunc) *GeoIP {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := resty.New().SetTransport(server.Client().Transport)
	base := client.Transport()
	client.SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Scheme != schemeHTTPS || (req.URL.Host != "download.maxmind.com" && req.URL.Host != "storage.example" && req.URL.Host != "cdn.download.maxmind.com") {
			return nil, fmt.Errorf("unexpected download request: %s", req.URL)
		}
		copyReq := req.Clone(req.Context())
		copyReq.Host = req.URL.Host
		copyReq.URL.Host = endpoint.Host
		return base.RoundTrip(copyReq)
	}))
	limiter, err := NewRateLimiter(RateLimitConfig{
		HTTP:    []RateLimit{{Requests: 1000, Interval: time.Second}},
		Servers: map[string][]RateLimit{"https://download.maxmind.com": {{Requests: 1000, Interval: time.Second}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir(), AccountID: "test-account", LicenseKey: "test-key", HTTPClient: client, RateLimiter: limiter})
}

func geoIPTestArchive(t *testing.T, edition string, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: edition + "_20260101/" + edition + ".mmdb", Mode: 0o600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
