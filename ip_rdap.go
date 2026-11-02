package reginfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

type ipRDAPResponse struct {
	ObjectClassName string   `json:"objectClassName"`
	Handle          string   `json:"handle"`
	StartAddress    string   `json:"startAddress"`
	EndAddress      string   `json:"endAddress"`
	IPVersion       string   `json:"ipVersion"`
	Name            string   `json:"name"`
	Type            string   `json:"type"`
	Country         string   `json:"country"`
	ParentHandle    string   `json:"parentHandle"`
	Port43          string   `json:"port43"`
	Status          []string `json:"status"`
	Events          []event  `json:"events"`
	Entities        []entity `json:"entities"`
}

func (c *Client) lookupIPRDAP(ctx context.Context, servers []string, addr netip.Addr) (*IPInfo, error) {
	var failures []error
	for _, base := range servers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		u, err := url.Parse(base)
		if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			failures = append(failures, fmt.Errorf("invalid RDAP endpoint %q", base))
			continue
		}
		endpoint := strings.TrimRight(base, "/") + "/ip/" + url.PathEscape(addr.String())
		body, err := c.get(ctx, endpoint, 4<<20)
		if err != nil {
			if isRateLimitError(err) {
				return nil, err
			}
			failures = append(failures, err)
			continue
		}
		info, err := parseIPRDAP(body, addr, base)
		if err == nil {
			return info, nil
		}
		failures = append(failures, err)
	}
	return nil, fmt.Errorf("no usable IP RDAP response: %w", errors.Join(failures...))
}

func parseIPRDAP(body []byte, addr netip.Addr, base string) (*IPInfo, error) {
	var response ipRDAPResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	start, startErr := netip.ParseAddr(response.StartAddress)
	end, endErr := netip.ParseAddr(response.EndAddress)
	if response.ObjectClassName != "ip network" || response.IPVersion != ipVersion(addr) ||
		startErr != nil || endErr != nil || !validIPRange(addr, start, end) {
		return nil, errors.New("RDAP response does not contain a matching IP network")
	}
	info := &IPInfo{
		IP: addr.String(), IPVersion: ipVersion(addr), RegistryID: response.Handle,
		Name: response.Name, Type: response.Type, Country: response.Country, ParentHandle: response.ParentHandle,
		StartAddress: start.String(), EndAddress: end.String(), CIDRs: rangeCIDRs(start, end),
		Status: uniq(response.Status), Registry: RegistryInfo{RDAPServer: base, WHOISServer: response.Port43},
		Source: sourceRDAP, CollectedAt: time.Now().UTC(), Raw: string(body),
	}
	for _, e := range response.Events {
		switch strings.ToLower(e.Action) {
		case "registration":
			info.CreatedAt = parseTime(e.Date)
		case "last changed":
			info.UpdatedAt = parseTime(e.Date)
		}
	}
	for _, e := range response.Entities {
		parseIPRDAPEntity(info, e)
	}
	return info, nil
}

func parseIPRDAPEntity(info *IPInfo, e entity) {
	contact := IPContact{
		Handle: e.Handle, Name: entityName(e), Organization: entityOrg(e),
		Roles: slices.Clone(e.Roles), Emails: vcard(e, "email"), Phones: vcard(e, "tel"),
	}
	info.Contacts = append(info.Contacts, contact)
	if roles(e, "registrant") && info.Organization == "" {
		info.Organization = contact.Organization
		if info.Organization == "" {
			info.Organization = contact.Name
		}
	}
	for _, child := range e.Entities {
		parseIPRDAPEntity(info, child)
	}
}
