package reginfo

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/v2"
	"github.com/maxmind/mmdbwriter/v2/mmdbtype"
)

func TestGeoIPLookup(t *testing.T) {
	tests := []struct {
		name, desc, ip, normalized string
		load, close, cancel        bool
		wantErr                    error
	}{
		{name: "ipv4", desc: "Returns geolocation and ASN data from local databases", ip: "192.0.2.42", normalized: "192.0.2.42", load: true},
		{name: "ipv6", desc: "Normalizes IPv6 before a lookup", ip: " 2001:0DB8::1 ", normalized: "2001:db8::1", load: true},
		{name: "mapped", desc: "IPv4-mapped IPv6 uses the IPv4 record", ip: "::ffff:192.0.2.42", normalized: "192.0.2.42", load: true},
		{name: "missing", desc: "A missing record is distinguished from a database error", ip: "10.0.0.1", load: true, wantErr: ErrGeoIPNotFound},
		{name: "invalid", desc: "A domain name is not an IP address", ip: "example.com", load: true, wantErr: ErrInvalidIP},
		{name: "zone", desc: "A scope ID is rejected", ip: "fe80::1%eth0", load: true, wantErr: ErrInvalidIP},
		{name: "not_loaded", desc: "The constructor does not implicitly load databases", ip: "192.0.2.42", wantErr: ErrGeoIPNotLoaded},
		{name: "closed", desc: "Reads are rejected after Close", ip: "192.0.2.42", load: true, close: true, wantErr: ErrGeoIPClosed},
		{name: "canceled", desc: "A canceled context stops a local lookup", ip: "192.0.2.42", load: true, cancel: true, wantErr: context.Canceled},
	}
	databases := geoIPTestDatabases(t, "Old City")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			geo := newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir()})
			if tt.load {
				writeGeoIPTestDatabases(t, geo.config.Directory, databases)
				if err := geo.LoadDatabases(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if tt.close {
				if err := geo.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if tt.cancel {
				cancel()
			}
			info, err := geo.Lookup(ctx, tt.ip)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Lookup=%+v, %v; want %v", info, err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if info != nil {
					t.Fatalf("error returned with data: %+v", info)
				}
				return
			}
			if info.IP != tt.normalized || info.City != "Old City" || info.Country != "US" || info.CountryName != "United States" || info.RegisteredCountry != "CA" || info.ASN != 64500 || info.ASNOrganization != "Example ASN" {
				t.Fatalf("unexpected location: %+v", info)
			}
			if info.Latitude == nil || *info.Latitude != 0 || info.Longitude == nil || *info.Longitude != 12.5 || info.AccuracyRadius != 25 || info.TimeZone != "Etc/UTC" || info.PostalCode != "12345" || !info.IsAnycast {
				t.Fatalf("unexpected coordinates: %+v", info)
			}
			if len(info.Subdivisions) != 1 || info.Subdivisions[0].ISOCode != "CA" || info.ContinentCode != "NA" || info.Network == "" || info.ASNNetwork == "" {
				t.Fatalf("missing geography: %+v", info)
			}
			metadata, metaErr := geo.Metadata()
			if metaErr != nil || len(metadata) != 3 || metadata[0].Edition != "GeoLite2-City" || !metadata[0].BuildTime.Equal(time.Unix(1700000000, 0)) {
				t.Fatalf("Metadata=%+v, %v", metadata, metaErr)
			}
		})
	}
}

func TestGeoIPReload(t *testing.T) {
	tests := []struct {
		name, desc, damage string
		wantErr            bool
	}{
		{name: "replace", desc: "A new set replaces the previous set"},
		{name: "missing", desc: "An incomplete set does not replace the active databases", damage: "missing", wantErr: true},
		{name: "corrupt", desc: "A corrupt database leaves the active set intact", damage: "corrupt", wantErr: true},
		{name: "wrong_type", desc: "The database type is checked in addition to the filename", damage: "wrong_type", wantErr: true},
		{name: "manifest", desc: "CURRENT cannot point outside the database directory", damage: "manifest", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			geo := newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir()})
			writeGeoIPTestDatabases(t, geo.config.Directory, geoIPTestDatabases(t, "Old City"))
			if err := geo.LoadDatabases(t.Context()); err != nil {
				t.Fatal(err)
			}
			next := geoIPTestDatabases(t, "New City")
			switch tt.damage {
			case "corrupt":
				next["GeoLite2-ASN"] = []byte("invalid mmdb")
			case "wrong_type":
				next["GeoLite2-ASN"] = next["GeoLite2-City"]
			}
			writeGeoIPTestDatabases(t, geo.config.Directory, next)
			if tt.damage == "missing" {
				if err := os.Remove(filepath.Join(geo.config.Directory, "GeoLite2-ASN.mmdb")); err != nil {
					t.Fatal(err)
				}
			}
			if tt.damage == "manifest" {
				if err := os.WriteFile(filepath.Join(geo.config.Directory, "CURRENT"), []byte(".reginfo-geoip-../../outside"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := geo.LoadDatabases(t.Context()); (err != nil) != tt.wantErr {
				t.Fatalf("LoadDatabases=%v", err)
			}
			info, err := geo.Lookup(t.Context(), "192.0.2.42")
			want := "New City"
			if tt.wantErr {
				want = "Old City"
			}
			if err != nil || info.City != want {
				t.Fatalf("Lookup=%+v, %v; want %s", info, err, want)
			}
		})
	}
}

func TestGeoIPPartialRecords(t *testing.T) {
	tests := []struct {
		name, desc         string
		city, country, asn bool
	}{
		{name: "country_only", desc: "Country is used when City has no matching record", country: true},
		{name: "asn_only", desc: "ASN data is returned without geographic data", asn: true},
		{name: "city_only", desc: "Geographic data is returned without ASN data", city: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			databases := geoIPTestDatabases(t, "Old City")
			for i, present := range []bool{tt.city, tt.country, tt.asn} {
				if !present {
					databases[geoIPEditions[i]] = geoIPTestDatabase(t, geoIPEditions[i], nil)
				}
			}
			geo := newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir()})
			writeGeoIPTestDatabases(t, geo.config.Directory, databases)
			if err := geo.LoadDatabases(t.Context()); err != nil {
				t.Fatal(err)
			}
			info, err := geo.Lookup(t.Context(), "192.0.2.42")
			if err != nil {
				t.Fatal(err)
			}
			if (info.City != "") != tt.city || (info.Country != "") != (tt.city || tt.country) || (info.ASN != 0) != tt.asn {
				t.Fatalf("unexpected partial record: %+v", info)
			}
		})
	}
}

func TestClientGeoIPCache(t *testing.T) {
	tests := []struct {
		name, desc string
		hit        bool
	}{
		{name: "cache_hit", desc: "Geolocation is refreshed even on a cache hit", hit: true},
		{name: "registry_then_cache", desc: "The cache stores registration data without geolocation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			geo := newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir()})
			writeGeoIPTestDatabases(t, geo.config.Directory, geoIPTestDatabases(t, "Old City"))
			if err := geo.LoadDatabases(t.Context()); err != nil {
				t.Fatal(err)
			}
			cache := NewDefaultIPMemoryCache()
			if tt.hit {
				info := cachedIPTestInfo()
				info.Country = "ZZ"
				if err := cache.Set(t.Context(), info.IP, info); err != nil {
					t.Fatal(err)
				}
			}
			client, calls := ipFixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/bootstrap/") {
					writeTestResponse(t, w, ipBootstrapFixture("https://"+r.Host))
					return
				}
				writeTestResponse(t, w, ipRDAPFixture("v4", "192.0.2.0", "192.0.2.255"))
			}, WithIPCache(cache), WithGeoIP(geo))
			for _, city := range []string{"Old City", "New City"} {
				writeGeoIPTestDatabases(t, geo.config.Directory, geoIPTestDatabases(t, city))
				if err := geo.LoadDatabases(t.Context()); err != nil {
					t.Fatal(err)
				}
				info, err := client.LookupIP(t.Context(), "192.0.2.42")
				if err != nil || info.GeoIP == nil || info.GeoIP.City != city {
					t.Fatalf("LookupIP=%+v, %v", info, err)
				}
				if tt.hit && info.Country != "ZZ" {
					t.Fatalf("registration country changed: %s", info.Country)
				}
				local, localErr := client.LookupGeoIP(t.Context(), "192.0.2.42")
				if localErr != nil || !reflect.DeepEqual(local, info.GeoIP) {
					t.Fatalf("local lookup differs: %+v, %v", local, localErr)
				}
				stored, cacheErr := cache.Get(t.Context(), info.IP)
				if cacheErr != nil || stored.GeoIP != nil {
					t.Fatalf("cache contains GeoIP: %+v, %v", stored, cacheErr)
				}
				info.GeoIP.City = "mutated"
				*info.GeoIP.Latitude = 99
				info.GeoIP.Subdivisions[0].Name = "mutated"
			}
			wantCalls := int32(1)
			if tt.hit {
				wantCalls = 0
			}
			if calls.rdap.Load() != wantCalls {
				t.Fatalf("RDAP requests=%d; want %d", calls.rdap.Load(), wantCalls)
			}
		})
	}
}

func TestGeoIPConcurrentLifecycle(t *testing.T) {
	tests := []struct {
		name, desc string
		close      bool
	}{
		{name: "reload", desc: "Concurrent requests see a complete database set"},
		{name: "close", desc: "Close is safe during active requests", close: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			geo := newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir()})
			writeGeoIPTestDatabases(t, geo.config.Directory, geoIPTestDatabases(t, "Old City"))
			if err := geo.LoadDatabases(t.Context()); err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			for range 8 {
				workers.Go(func() {
					for range 50 {
						info, err := geo.Lookup(t.Context(), "192.0.2.42")
						if tt.close && errors.Is(err, ErrGeoIPClosed) {
							return
						}
						if err != nil || info.Country != "US" || info.ASN != 64500 {
							t.Errorf("Lookup=%+v, %v", info, err)
							return
						}
					}
				})
			}
			var err error
			if tt.close {
				err = geo.Close()
			} else {
				err = geo.LoadDatabases(t.Context())
			}
			workers.Wait()
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClientGeoIPErrors(t *testing.T) {
	tests := []struct {
		name, desc            string
		disabled, load, close bool
		wantErr, localErr     error
	}{
		{name: "disabled", desc: "Disabled GeoIP removes a stale field from a custom cache result", disabled: true, localErr: ErrGeoIPNotLoaded},
		{name: "missing", desc: "Missing geolocation returns registration data without GeoIP", load: true, localErr: ErrGeoIPNotFound},
		{name: "not_loaded", desc: "An attached component with no loaded databases returns an error", wantErr: ErrGeoIPNotLoaded, localErr: ErrGeoIPNotLoaded},
		{name: "closed", desc: "A closed component's error is not hidden", load: true, close: true, wantErr: ErrGeoIPClosed, localErr: ErrGeoIPClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			geo := newGeoIPTest(t, GeoIPConfig{Directory: t.TempDir()})
			if tt.load {
				writeGeoIPTestDatabases(t, geo.config.Directory, geoIPTestDatabases(t, "Old City"))
				if err := geo.LoadDatabases(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if tt.close {
				if err := geo.Close(); err != nil {
					t.Fatal(err)
				}
			}
			cache := NewDefaultIPMemoryCache()
			stored := &IPInfo{IP: "10.0.0.1", Country: "ZZ", GeoIP: &GeoIPInfo{City: "Stale"}}
			if err := cache.Set(t.Context(), stored.IP, stored); err != nil {
				t.Fatal(err)
			}
			options := []Option{WithIPCache(cache)}
			if !tt.disabled {
				options = append(options, WithGeoIP(geo))
			}
			client := NewClient(options...)
			info, err := client.LookupIP(t.Context(), stored.IP)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("LookupIP=%+v, %v; want %v", info, err, tt.wantErr)
			}
			if tt.wantErr == nil && (info.GeoIP != nil || info.Country != stored.Country) {
				t.Fatalf("unexpected registration result: %+v", info)
			}
			if _, err = client.LookupGeoIP(t.Context(), stored.IP); !errors.Is(err, tt.localErr) {
				t.Fatalf("LookupGeoIP=%v; want %v", err, tt.localErr)
			}
		})
	}
}

func TestIPMemoryCacheGeoIPCopies(t *testing.T) {
	tests := []struct {
		name, desc  string
		coordinates bool
	}{
		{name: "complete", desc: "The cache copies geolocation including coordinates and subdivisions", coordinates: true},
		{name: "no_coordinates", desc: "Missing coordinates remain nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.desc)
			cache := NewDefaultIPMemoryCache()
			geo := &GeoIPInfo{City: "Original", Subdivisions: []GeoIPSubdivision{{Name: "Region"}}}
			if tt.coordinates {
				latitude, longitude := 0.0, 12.5
				geo.Latitude, geo.Longitude = &latitude, &longitude
			}
			info := &IPInfo{IP: "192.0.2.42", GeoIP: geo}
			if err := cache.Set(t.Context(), info.IP, info); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				geo.City, geo.Subdivisions[0].Name = "Changed", "Changed"
				if tt.coordinates {
					*geo.Latitude, *geo.Longitude = 90, 180
				}
				got, err := cache.Get(t.Context(), info.IP)
				if err != nil || got.GeoIP.City != "Original" || got.GeoIP.Subdivisions[0].Name != "Region" {
					t.Fatalf("Get=%+v, %v", got, err)
				}
				geo = got.GeoIP
				if tt.coordinates && (*geo.Latitude != 0 || *geo.Longitude != 12.5) {
					t.Fatalf("cached coordinates changed: %+v", geo)
				}
				if !tt.coordinates && (geo.Latitude != nil || geo.Longitude != nil) {
					t.Fatalf("missing coordinates acquired values: %+v", geo)
				}
			}
		})
	}
}

func newGeoIPTest(t *testing.T, config GeoIPConfig) *GeoIP {
	t.Helper()
	geo, err := NewGeoIP(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := geo.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return geo
}

func geoIPTestDatabases(t *testing.T, city string) map[string][]byte {
	t.Helper()
	country := mmdbtype.Map{
		"country":            mmdbtype.Map{"iso_code": mmdbtype.String("US"), "names": mmdbtype.Map{"en": mmdbtype.String("United States")}},
		"registered_country": mmdbtype.Map{"iso_code": mmdbtype.String("CA")},
		"continent":          mmdbtype.Map{"code": mmdbtype.String("NA"), "names": mmdbtype.Map{"en": mmdbtype.String("North America")}},
	}
	cityRecord := country.Copy().(mmdbtype.Map) //nolint:errcheck // Copy preserves the mmdbtype.Map type.
	cityRecord["city"] = mmdbtype.Map{"names": mmdbtype.Map{"en": mmdbtype.String(city)}}
	cityRecord["location"] = mmdbtype.Map{"latitude": mmdbtype.Float64(0), "longitude": mmdbtype.Float64(12.5), "accuracy_radius": mmdbtype.Uint16(25), "time_zone": mmdbtype.String("Etc/UTC")}
	cityRecord["subdivisions"] = mmdbtype.Slice{mmdbtype.Map{"iso_code": mmdbtype.String("CA"), "names": mmdbtype.Map{"en": mmdbtype.String("California")}}}
	cityRecord["postal"] = mmdbtype.Map{"code": mmdbtype.String("12345")}
	cityRecord["traits"] = mmdbtype.Map{"is_anycast": mmdbtype.Bool(true)}
	asn := mmdbtype.Map{"autonomous_system_number": mmdbtype.Uint32(64500), "autonomous_system_organization": mmdbtype.String("Example ASN")}
	return map[string][]byte{
		"GeoLite2-City":    geoIPTestDatabase(t, "GeoLite2-City", cityRecord),
		"GeoLite2-Country": geoIPTestDatabase(t, "GeoLite2-Country", country),
		"GeoLite2-ASN":     geoIPTestDatabase(t, "GeoLite2-ASN", asn),
	}
}

func geoIPTestDatabase(t *testing.T, edition string, record mmdbtype.Map) []byte {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: edition, IPVersion: 6, BuildEpoch: 1700000000, IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	if record != nil {
		for _, prefix := range []string{"192.0.2.0/24", "2001:db8::/32"} {
			if err = tree.Insert(netip.MustParsePrefix(prefix), record); err != nil {
				t.Fatal(err)
			}
		}
	}
	var data bytes.Buffer
	if _, err = tree.WriteTo(&data); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func writeGeoIPTestDatabases(t *testing.T, directory string, databases map[string][]byte) {
	t.Helper()
	for edition, data := range databases {
		staged := filepath.Join(directory, edition+".tmp")
		if err := os.WriteFile(staged, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(staged, filepath.Join(directory, edition+".mmdb")); err != nil {
			t.Fatal(err)
		}
	}
}
