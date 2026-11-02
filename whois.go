package reginfo

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

func (a *Client) whois(ctx context.Context, server, query string) (body string, err error) {
	defer func() {
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			err = context.DeadlineExceeded
		}
	}()
	if err = a.rateLimiter.wait(ctx, serviceKey(schemeWHOIS, server)); err != nil {
		return "", err
	}
	c, e := a.dialContext(ctx, "tcp", net.JoinHostPort(server, "43"))
	if e != nil {
		return "", e
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	deadline := time.Now().Add(8 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if e = c.SetDeadline(deadline); e != nil {
		return "", e
	}
	if _, e = io.WriteString(c, query+"\r\n"); e != nil {
		return "", e
	}
	b, e := readLimited(c, 2<<20)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return string(b), e
}
func field(body string, keys ...string) string {
	wanted := map[string]bool{}
	for _, k := range keys {
		wanted[strings.ToLower(k)] = true
	}
	for _, l := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if ok && wanted[strings.ToLower(strings.TrimSpace(k))] {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func fields(body string, keys ...string) []string {
	wanted := map[string]bool{}
	for _, k := range keys {
		wanted[strings.ToLower(k)] = true
	}
	var out []string
	for _, l := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if ok && wanted[strings.ToLower(strings.TrimSpace(k))] {
			v = strings.TrimSpace(v)
			if v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}
func parseWHOISCommon(body, domain string, p Provider) *DomainInfo {
	now := time.Now().UTC()
	i := &DomainInfo{Domain: domain, Source: sourceWHOIS, CollectedAt: now, Raw: body, Registry: RegistryInfo{WHOISServer: p.WHOIS, RDAPServer: first(p.RDAP)}}
	i.RegistryID = field(body, "registry domain id", "domain id")
	i.CreatedAt = parseTime(field(body, "created", "creation date", "created on", "registered on", "registration time", "domain create date"))
	i.UpdatedAt = parseTime(field(body, "updated date", "last updated on", "changed", "modified", "last modified", "domain last updated date"))
	i.ExpiresAt = parseTime(field(body, "paid-till", "registry expiry date", "expiration date", "expiry date", "expires", "expire date", "domain expiration date"))
	i.AgeDays = ageDays(i.CreatedAt)
	i.Registrar.Name = field(body, "registrar", "registrar name", "sponsoring registrar")
	i.Registrar.IANAID = field(body, "registrar iana id")
	i.Registrar.URL = field(body, "registrar url")
	i.Registrar.AbuseEmail = field(body, "registrar abuse contact email", "abuse contact email")
	i.Registrar.AbusePhone = field(body, "registrar abuse contact phone", "abuse contact phone")
	i.Registrant.Organization = field(body, "registrant organization", "org")
	i.Registrant.Country = field(body, "registrant country", "country")
	i.Registrant.State = field(body, "registrant state/province", "registrant state", "state")
	for _, v := range fields(body, "nserver", "name server", "nameserver") {
		if x := strings.Fields(v); len(x) > 0 {
			i.NameServers = append(i.NameServers, strings.ToLower(x[0]))
		}
	}
	i.NameServers = uniq(i.NameServers)
	i.Status = uniq(fields(body, "state", "domain status", "status"))
	dns := strings.ToLower(field(body, "dnssec"))
	if strings.Contains(dns, "signed") && !strings.Contains(dns, "unsigned") {
		i.DNSSEC = "signed"
	} else if dns != "" {
		i.DNSSEC = "unsigned"
	}
	return i
}
func parseRUWHOIS(body, domain string, p Provider) *DomainInfo {
	i := parseWHOISCommon(body, domain, p)
	i.CreatedAt = parseTime(field(body, "created"))
	i.ExpiresAt = parseTime(field(body, "paid-till", "free-date"))
	i.AgeDays = ageDays(i.CreatedAt)
	i.Registrar.Name = field(body, "registrar")
	i.Registrant.Organization = field(body, "org")
	i.Status = uniq(fields(body, "state"))
	return i
}
func recognized(i *DomainInfo) bool {
	return i.CreatedAt != nil || i.ExpiresAt != nil || i.Registrar.Name != "" || len(i.NameServers) > 0 || len(i.Status) > 0
}
func (a *Client) lookupWHOIS(ctx context.Context, p Provider, domain, tld string) (*DomainInfo, error) {
	if p.WHOIS == "" {
		return nil, errors.New("no WHOIS provider")
	}
	body, e := a.whois(ctx, p.WHOIS, domain)
	if e != nil {
		return nil, e
	}
	var i *DomainInfo
	if tld == "ru" || tld == "xn--p1ai" {
		i = parseRUWHOIS(body, domain, p)
	} else {
		i = parseWHOISCommon(body, domain, p)
	}
	if !recognized(i) {
		return nil, errors.New("WHOIS response contained no recognized registration fields")
	}
	return i, nil
}
