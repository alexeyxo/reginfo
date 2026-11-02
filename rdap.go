package reginfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type bootstrapResponse struct {
	Services [][][]string `json:"services"`
}
type event struct {
	Action string `json:"eventAction"`
	Date   string `json:"eventDate"`
}
type vcardArray []any

type entity struct {
	Handle    string     `json:"handle"`
	Roles     []string   `json:"roles"`
	VCard     vcardArray `json:"vcardArray"`
	PublicIDs []struct {
		Type       string `json:"type"`
		Identifier string `json:"identifier"`
	} `json:"publicIds"`
	Entities []entity `json:"entities"`
}
type rdapResponse struct {
	Domain      string   `json:"ldhName"`
	UnicodeName string   `json:"unicodeName"`
	Handle      string   `json:"handle"`
	Events      []event  `json:"events"`
	Status      []string `json:"status"`
	SecureDNS   *struct {
		DelegationSigned bool `json:"delegationSigned"`
	} `json:"secureDNS"`
	Nameservers []struct {
		Domain      string `json:"ldhName"`
		UnicodeName string `json:"unicodeName"`
	} `json:"nameservers"`
	Entities []entity `json:"entities"`
}

func roles(e entity, role string) bool {
	for _, r := range e.Roles {
		if strings.EqualFold(r, role) {
			return true
		}
	}
	return false
}
func vcard(e entity, key string) []string {
	if len(e.VCard) < 2 {
		return nil
	}
	rows, ok := e.VCard[1].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, p := range rows {
		row, rowOK := p.([]any)
		if !rowOK || len(row) < 4 || !strings.EqualFold(fmt.Sprint(row[0]), key) {
			continue
		}
		out = append(out, fmt.Sprint(row[3]))
	}
	return out
}
func first(xs []string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return ""
}
func entityName(e entity) string { return first(vcard(e, "fn")) }
func entityOrg(e entity) string {
	v := vcard(e, "org")
	if len(v) > 0 {
		return strings.Trim(v[0], "[]")
	}
	return ""
}
func entityAddress(e entity) (state, country string) {
	if len(e.VCard) < 2 {
		return state, country
	}
	rows, ok := e.VCard[1].([]any)
	if !ok {
		return state, country
	}
	for _, p := range rows {
		row, rowOK := p.([]any)
		if !rowOK || len(row) < 4 || fmt.Sprint(row[0]) != "adr" {
			continue
		}
		vals, valuesOK := row[3].([]any)
		if !valuesOK {
			continue
		}
		if len(vals) > 4 {
			state = fmt.Sprint(vals[4])
		}
		if len(vals) > 6 {
			country = fmt.Sprint(vals[6])
		}
	}
	return state, country
}
func parseRDAPEntity(info *DomainInfo, e entity) {
	if roles(e, "registrar") {
		info.Registrar.Name = entityName(e)
		for _, id := range e.PublicIDs {
			if strings.Contains(strings.ToLower(id.Type), "iana") {
				info.Registrar.IANAID = id.Identifier
			}
		}
		for _, u := range vcard(e, "url") {
			if info.Registrar.URL == "" {
				info.Registrar.URL = u
			}
		}
		for _, mail := range vcard(e, "email") {
			if strings.Contains(strings.ToLower(mail), "abuse") {
				info.Registrar.AbuseEmail = mail
			}
		}
		for _, tel := range vcard(e, "tel") {
			if info.Registrar.AbusePhone == "" {
				info.Registrar.AbusePhone = tel
			}
		}
		for _, child := range e.Entities {
			if roles(child, "abuse") {
				if info.Registrar.AbuseEmail == "" {
					info.Registrar.AbuseEmail = first(vcard(child, "email"))
				}
				if info.Registrar.AbusePhone == "" {
					info.Registrar.AbusePhone = first(vcard(child, "tel"))
				}
			}
		}
	}
	if roles(e, "registrant") {
		info.Registrant.Organization = entityOrg(e)
		info.Registrant.State, info.Registrant.Country = entityAddress(e)
	}
}
func (a *Client) lookupRDAP(ctx context.Context, p Provider, domain string) (*DomainInfo, error) {
	var last error
	for _, base := range p.RDAP {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		u, e := url.Parse(base)
		if e != nil || u.Scheme != schemeHTTPS || u.Host == "" {
			last = fmt.Errorf("invalid RDAP endpoint %q", base)
			continue
		}
		endpoint := strings.TrimRight(base, "/") + "/domain/" + url.PathEscape(domain)
		b, e := a.get(ctx, endpoint, 4<<20)
		if e != nil {
			if isRateLimitError(e) {
				return nil, e
			}
			last = e
			continue
		}
		var r rdapResponse
		if e = json.Unmarshal(b, &r); e != nil {
			last = e
			continue
		}
		if r.Domain == "" {
			last = errors.New("RDAP response missing ldhName")
			continue
		}
		now := time.Now().UTC()
		info := &DomainInfo{Domain: strings.ToLower(r.Domain), UnicodeName: r.UnicodeName, RegistryID: r.Handle, Status: uniq(r.Status), Source: sourceRDAP, CollectedAt: now, Raw: string(b), Registry: RegistryInfo{RDAPServer: base, WHOISServer: p.WHOIS}}
		for _, ev := range r.Events {
			switch strings.ToLower(ev.Action) {
			case "registration":
				info.CreatedAt = parseTime(ev.Date)
			case "expiration":
				info.ExpiresAt = parseTime(ev.Date)
			case "last changed", "last update of rdap database":
				if info.UpdatedAt == nil {
					info.UpdatedAt = parseTime(ev.Date)
				}
			}
		}
		info.AgeDays = ageDays(info.CreatedAt)
		for _, ns := range r.Nameservers {
			info.NameServers = append(info.NameServers, strings.ToLower(ns.Domain))
		}
		info.NameServers = uniq(info.NameServers)
		if r.SecureDNS != nil {
			if r.SecureDNS.DelegationSigned {
				info.DNSSEC = "signed"
			} else {
				info.DNSSEC = "unsigned"
			}
		}
		for _, e := range r.Entities {
			parseRDAPEntity(info, e)
		}
		return info, nil
	}
	return nil, last
}
