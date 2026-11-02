package reginfo

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

func (c *Client) lookupIPWHOIS(ctx context.Context, addr netip.Addr) (*IPInfo, error) {
	server := "whois.iana.org"
	visited := make(map[string]bool)
	for range 5 {
		if visited[server] {
			return nil, fmt.Errorf("WHOIS referral loop at %s", server)
		}
		visited[server] = true
		query := addr.String()
		if server == "whois.arin.net" {
			query = "n + " + query
		}
		body, err := c.whois(ctx, server, query)
		if err != nil {
			return nil, err
		}
		referral := field(body, "refer", "referralserver")
		if server == "whois.iana.org" && referral == "" {
			referral = field(body, "whois")
		}
		if referral != "" && !strings.HasPrefix(strings.ToLower(referral), "rwhois://") {
			next, referralErr := whoisReferral(referral)
			if referralErr != nil {
				return nil, referralErr
			}
			server = next
			continue
		}
		return parseIPWHOIS(body, addr, server)
	}
	return nil, errors.New("WHOIS referral limit exceeded")
}

func whoisReferral(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "whois://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != schemeWHOIS || u.Host == "" || u.User != nil ||
		(u.Port() != "" && u.Port() != "43") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid WHOIS referral %q", raw)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if !strings.Contains(host, ".") {
		return "", fmt.Errorf("invalid WHOIS host %q", host)
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' {
			return "", fmt.Errorf("invalid WHOIS host %q", host)
		}
	}
	if _, parseErr := netip.ParseAddr(host); parseErr == nil {
		return "", fmt.Errorf("invalid WHOIS host %q", host)
	}
	return host, nil
}

func parseIPWHOIS(body string, addr netip.Addr, server string) (*IPInfo, error) {
	blocks := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	var selected string
	var start, end netip.Addr
	for _, block := range blocks {
		candidateStart, candidateEnd := whoisIPRange(block, addr)
		if validIPRange(addr, candidateStart, candidateEnd) &&
			(!start.IsValid() || (candidateStart.Compare(start) >= 0 && candidateEnd.Compare(end) <= 0)) {
			selected, start, end = block, candidateStart, candidateEnd
		}
	}
	if selected == "" {
		return nil, errors.New("WHOIS response does not contain a matching IP network")
	}
	info := &IPInfo{
		IP: addr.String(), IPVersion: ipVersion(addr), StartAddress: start.String(), EndAddress: end.String(),
		CIDRs: rangeCIDRs(start, end), RegistryID: field(selected, "nethandle", "net-hdl"),
		Name: field(selected, "netname"), Type: field(selected, "nettype", "status"),
		Country: field(selected, "country"), ParentHandle: field(selected, "parent"),
		Organization: field(selected, "orgname", "org-name", "owner", "organization"),
		CreatedAt:    parseIPWHOISTime(field(selected, "regdate", "created")),
		UpdatedAt:    parseIPWHOISTime(field(selected, "updated", "last-modified", "changed")),
		Status:       uniq(fields(selected, "status")), Registry: RegistryInfo{WHOISServer: server},
		Source: sourceWHOIS, CollectedAt: time.Now().UTC(), Raw: body,
	}
	orgHandle := field(selected, "org", "organisation")
	for _, block := range blocks {
		if orgHandle != "" && strings.EqualFold(field(block, "organisation", "orgid"), orgHandle) {
			info.Organization = field(block, "org-name", "orgname", "organization")
		}
		info.Contacts = append(info.Contacts, whoisIPContacts(block)...)
	}
	return info, nil
}

func whoisIPRange(block string, addr netip.Addr) (netip.Addr, netip.Addr) {
	raw := field(block, "netrange", "inetnum", "inet6num")
	if left, right, ok := strings.Cut(raw, "-"); ok {
		start, startErr := netip.ParseAddr(strings.TrimSpace(left))
		end, endErr := netip.ParseAddr(strings.TrimSpace(right))
		if startErr != nil || endErr != nil {
			return netip.Addr{}, netip.Addr{}
		}
		return start, end
	}
	if raw == "" {
		raw = field(block, "cidr")
	}
	for _, part := range strings.Split(raw, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err == nil && prefix.Contains(addr) {
			return prefix.Masked().Addr(), prefixLast(prefix)
		}
	}
	return netip.Addr{}, netip.Addr{}
}

func whoisIPContacts(block string) []IPContact {
	var contacts []IPContact
	for _, role := range []string{"abuse", "tech", "noc"} {
		prefix := "org" + role
		emails := fields(block, prefix+"email")
		if role == "abuse" {
			emails = append(emails, fields(block, "abuse-mailbox")...)
		}
		phones := fields(block, prefix+"phone")
		if len(emails) != 0 || len(phones) != 0 {
			contacts = append(contacts, IPContact{
				Handle: field(block, prefix+"handle"), Name: field(block, prefix+"name"),
				Roles: []string{role}, Emails: uniq(emails), Phones: uniq(phones),
			})
		}
	}
	emails, phones := fields(block, "e-mail", "email"), fields(block, "phone")
	if len(emails) != 0 || len(phones) != 0 {
		contacts = append(contacts, IPContact{
			Handle: field(block, "nic-hdl", "handle", "nic-hdl-br"), Name: field(block, "person", "role"),
			Emails: uniq(emails), Phones: uniq(phones),
		})
	}
	return contacts
}

func parseIPWHOISTime(raw string) *time.Time {
	if parsed := parseTime(raw); parsed != nil {
		return parsed
	}
	for _, token := range strings.Fields(raw) {
		if parsed, err := time.Parse("20060102", token); err == nil {
			return &parsed
		}
	}
	return nil
}
