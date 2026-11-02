package reginfo

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang/v2"
	"resty.dev/v3"
)

var (
	ErrGeoIPNotLoaded = errors.New("GeoIP databases are not loaded")
	ErrGeoIPNotFound  = errors.New("IP address is absent from GeoIP databases")
	ErrGeoIPClosed    = errors.New("GeoIP is closed")
)

type GeoIPConfig struct {
	Directory   string
	AccountID   string
	LicenseKey  string
	HTTPClient  *resty.Client
	RateLimiter *RateLimiter
}

type GeoIP struct {
	config   GeoIPConfig
	http     *resty.Client
	updateMu sync.Mutex
	mu       sync.RWMutex
	readers  geoIPReaders
	closed   bool
}

type GeoIPInfo struct {
	IP                string             `json:"ip"`
	Country           string             `json:"country,omitempty"`
	CountryName       string             `json:"country_name,omitempty"`
	RegisteredCountry string             `json:"registered_country,omitempty"`
	City              string             `json:"city,omitempty"`
	Continent         string             `json:"continent,omitempty"`
	ContinentCode     string             `json:"continent_code,omitempty"`
	Subdivisions      []GeoIPSubdivision `json:"subdivisions,omitempty"`
	PostalCode        string             `json:"postal_code,omitempty"`
	Latitude          *float64           `json:"latitude,omitempty"`
	Longitude         *float64           `json:"longitude,omitempty"`
	AccuracyRadius    uint16             `json:"accuracy_radius,omitempty"`
	TimeZone          string             `json:"time_zone,omitempty"`
	ASN               uint               `json:"asn,omitempty"`
	ASNOrganization   string             `json:"asn_organization,omitempty"`
	Network           string             `json:"network,omitempty"`
	ASNNetwork        string             `json:"asn_network,omitempty"`
	IsAnycast         bool               `json:"is_anycast,omitempty"`
}

type GeoIPSubdivision struct {
	Name    string `json:"name,omitempty"`
	ISOCode string `json:"iso_code,omitempty"`
}

type GeoIPDatabaseInfo struct {
	Edition   string    `json:"edition"`
	BuildTime time.Time `json:"build_time"`
	IPVersion uint      `json:"ip_version"`
}

func NewGeoIP(config GeoIPConfig) (*GeoIP, error) {
	if strings.TrimSpace(config.Directory) == "" {
		return nil, errors.New("GeoIP directory is required")
	}
	directory, err := filepath.Abs(config.Directory)
	if err != nil {
		return nil, fmt.Errorf("GeoIP directory: %w", err)
	}
	config.Directory = directory
	if config.RateLimiter == nil {
		config.RateLimiter = &sharedRateLimiter
	}
	return &GeoIP{config: config, http: prepareHTTPClient(config.HTTPClient, 5*time.Minute)}, nil
}

func WithGeoIP(geo *GeoIP) Option {
	return func(c *Client) { c.geoIP = geo }
}

func (c *Client) LookupGeoIP(ctx context.Context, ip string) (*GeoIPInfo, error) {
	if c.geoIP == nil {
		return nil, ErrGeoIPNotLoaded
	}
	return c.geoIP.Lookup(ctx, ip)
}

func (c *Client) enrichIP(ctx context.Context, info *IPInfo) (*IPInfo, error) {
	info.GeoIP = nil
	if c.geoIP == nil {
		return info, nil
	}
	geo, err := c.geoIP.Lookup(ctx, info.IP)
	if err != nil && !errors.Is(err, ErrGeoIPNotFound) {
		return nil, fmt.Errorf("GeoIP lookup: %w", err)
	}
	info.GeoIP = geo
	return info, nil
}

func (g *GeoIP) Lookup(ctx context.Context, ip string) (*GeoIPInfo, error) {
	addr, err := normalizeIP(ip)
	if err != nil {
		return nil, err
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err = g.ready(ctx); err != nil {
		return nil, err
	}
	city, err := g.readers[0].City(addr)
	if err != nil {
		return nil, fmt.Errorf("GeoIP City: %w", err)
	}
	country, err := g.readers[1].Country(addr)
	if err != nil {
		return nil, fmt.Errorf("GeoIP Country: %w", err)
	}
	asn, err := g.readers[2].ASN(addr)
	if err != nil {
		return nil, fmt.Errorf("GeoIP ASN: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if !city.HasData() && !country.HasData() && !asn.HasData() {
		return nil, ErrGeoIPNotFound
	}
	return geoIPInfo(addr.String(), city, country, asn), nil
}

func (g *GeoIP) Metadata() ([]GeoIPDatabaseInfo, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := g.ready(context.Background()); err != nil {
		return nil, err
	}
	metadata := make([]GeoIPDatabaseInfo, 0, len(g.readers))
	for _, reader := range g.readers {
		m := reader.Metadata()
		metadata = append(metadata, GeoIPDatabaseInfo{
			Edition: m.DatabaseType, BuildTime: m.BuildTime().UTC(), IPVersion: m.IPVersion,
		})
	}
	return metadata, nil
}

func (g *GeoIP) Close() error {
	g.updateMu.Lock()
	defer g.updateMu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	err := g.readers.close()
	g.readers = geoIPReaders{}
	return err
}

func (g *GeoIP) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.closed {
		return ErrGeoIPClosed
	}
	if g.readers[0] == nil {
		return ErrGeoIPNotLoaded
	}
	return nil
}

func geoIPInfo(ip string, city *geoip2.City, country *geoip2.Country, asn *geoip2.ASN) *GeoIPInfo {
	info := &GeoIPInfo{
		IP: ip, City: city.City.Names.English, PostalCode: city.Postal.Code,
		Latitude: city.Location.Latitude, Longitude: city.Location.Longitude,
		AccuracyRadius: city.Location.AccuracyRadius, TimeZone: city.Location.TimeZone,
		ASN: asn.AutonomousSystemNumber, ASNOrganization: asn.AutonomousSystemOrganization,
		IsAnycast: city.Traits.IsAnycast || country.Traits.IsAnycast,
	}
	locationCountry := city.Country
	if !locationCountry.HasData() {
		locationCountry = country.Country
	}
	info.Country, info.CountryName = locationCountry.ISOCode, locationCountry.Names.English
	info.RegisteredCountry = city.RegisteredCountry.ISOCode
	if info.RegisteredCountry == "" {
		info.RegisteredCountry = country.RegisteredCountry.ISOCode
	}
	continent := city.Continent
	if !continent.HasData() {
		continent = country.Continent
	}
	info.Continent, info.ContinentCode = continent.Names.English, continent.Code
	if city.HasData() {
		info.Network = city.Traits.Network.String()
	} else if country.HasData() {
		info.Network = country.Traits.Network.String()
	}
	if asn.HasData() {
		info.ASNNetwork = asn.Network.String()
	}
	for _, subdivision := range city.Subdivisions {
		info.Subdivisions = append(info.Subdivisions, GeoIPSubdivision{Name: subdivision.Names.English, ISOCode: subdivision.ISOCode})
	}
	return info
}
