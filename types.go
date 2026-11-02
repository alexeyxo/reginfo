package reginfo

import (
	"time"
)

type RegistrarInfo struct {
	Name       string `json:"name,omitempty"`
	IANAID     string `json:"iana_id,omitempty"`
	URL        string `json:"url,omitempty"`
	AbuseEmail string `json:"abuse_email,omitempty"`
	AbusePhone string `json:"abuse_phone,omitempty"`
}

type RegistrantInfo struct {
	Organization string `json:"organization,omitempty"`
	Country      string `json:"country,omitempty"`
	State        string `json:"state,omitempty"`
}

type RegistryInfo struct {
	WHOISServer string `json:"whois_server,omitempty"`
	RDAPServer  string `json:"rdap_server,omitempty"`
}

type DomainInfo struct {
	Domain      string         `json:"domain"`
	UnicodeName string         `json:"unicode_name,omitempty"`
	RegistryID  string         `json:"registry_id,omitempty"`
	CreatedAt   *time.Time     `json:"created_at,omitempty"`
	UpdatedAt   *time.Time     `json:"updated_at,omitempty"`
	ExpiresAt   *time.Time     `json:"expires_at,omitempty"`
	AgeDays     *int           `json:"age_days,omitempty"`
	Registrar   RegistrarInfo  `json:"registrar"`
	Registrant  RegistrantInfo `json:"registrant"`
	NameServers []string       `json:"nameservers,omitempty"`
	DNSSEC      string         `json:"dnssec,omitempty"`
	Status      []string       `json:"status,omitempty"`
	Registry    RegistryInfo   `json:"registry"`
	Source      string         `json:"source"`
	CollectedAt time.Time      `json:"collected_at"`
	Raw         string         `json:"raw,omitempty"`
}

type Provider struct {
	TLD    string   `json:"tld"`
	RDAP   []string `json:"rdap,omitempty"`
	WHOIS  string   `json:"whois,omitempty"`
	Status string   `json:"status"`
	Error  string   `json:"error,omitempty"`
}

type IPContact struct {
	Handle       string   `json:"handle,omitempty"`
	Name         string   `json:"name,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Roles        []string `json:"roles,omitempty"`
	Emails       []string `json:"emails,omitempty"`
	Phones       []string `json:"phones,omitempty"`
}

type IPInfo struct {
	GeoIP        *GeoIPInfo   `json:"geo_ip,omitempty"`
	IP           string       `json:"ip"`
	IPVersion    string       `json:"ip_version"`
	RegistryID   string       `json:"registry_id,omitempty"`
	Name         string       `json:"name,omitempty"`
	Type         string       `json:"type,omitempty"`
	StartAddress string       `json:"start_address"`
	EndAddress   string       `json:"end_address"`
	CIDRs        []string     `json:"cidrs"`
	ParentHandle string       `json:"parent_handle,omitempty"`
	Country      string       `json:"country,omitempty"`
	Organization string       `json:"organization,omitempty"`
	Contacts     []IPContact  `json:"contacts,omitempty"`
	CreatedAt    *time.Time   `json:"created_at,omitempty"`
	UpdatedAt    *time.Time   `json:"updated_at,omitempty"`
	Status       []string     `json:"status,omitempty"`
	Registry     RegistryInfo `json:"registry"`
	Source       string       `json:"source"`
	CollectedAt  time.Time    `json:"collected_at"`
	Raw          string       `json:"raw,omitempty"`
}
