package reginfo

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

var ErrInvalidIP = errors.New("invalid IP address")

func LookupIP(ctx context.Context, ip string) (*IPInfo, error) {
	return defaultClient.LookupIP(ctx, ip)
}

func (c *Client) LookupIP(ctx context.Context, ip string) (*IPInfo, error) {
	addr, err := normalizeIP(ip)
	if err != nil {
		return nil, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	c.init()
	ip = addr.String()
	if c.ipCache != nil {
		info, cacheErr := c.ipCache.Get(ctx, ip)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if cacheErr == nil && info != nil {
			return c.enrichIP(ctx, cloneIPInfo(info))
		}
	}
	info, err := c.lookupIP(ctx, addr)
	if err != nil {
		return nil, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if c.ipCache != nil {
		_ = c.ipCache.Set(ctx, ip, cloneIPInfo(info)) //nolint:errcheck // A cache error does not discard a successful registry response.
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	return c.enrichIP(ctx, info)
}

func (c *Client) lookupIP(ctx context.Context, addr netip.Addr) (*IPInfo, error) {
	var failures []error
	servers, err := c.ipServers(ctx, addr)
	if err != nil {
		if isRateLimitError(err) || ctx.Err() != nil {
			return nil, err
		}
		failures = append(failures, fmt.Errorf("IP bootstrap: %w", err))
	} else {
		info, rdapErr := c.lookupIPRDAP(ctx, servers, addr)
		if rdapErr == nil {
			return info, nil
		}
		if isRateLimitError(rdapErr) || ctx.Err() != nil {
			return nil, rdapErr
		}
		failures = append(failures, fmt.Errorf("RDAP: %w", rdapErr))
	}
	info, err := c.lookupIPWHOIS(ctx, addr)
	if err == nil {
		info.Registry.RDAPServer = first(servers)
		return info, nil
	}
	failures = append(failures, fmt.Errorf("WHOIS: %w", err))
	return nil, fmt.Errorf("lookup IP %s: %w", addr, errors.Join(failures...))
}

func normalizeIP(raw string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, ErrInvalidIP
	}
	return addr.Unmap(), nil
}

func ipVersion(addr netip.Addr) string {
	if addr.Is4() {
		return "v4"
	}
	return "v6"
}

func validIPRange(addr, start, end netip.Addr) bool {
	return start.IsValid() && end.IsValid() && start.Zone() == "" && end.Zone() == "" &&
		start.BitLen() == addr.BitLen() && end.BitLen() == addr.BitLen() &&
		start.Compare(addr) <= 0 && addr.Compare(end) <= 0
}

func rangeCIDRs(start, end netip.Addr) []string {
	var cidrs []string
	for current := start; current.IsValid() && current.Compare(end) <= 0; {
		prefix := netip.PrefixFrom(current, current.BitLen())
		for bits := prefix.Bits() - 1; bits >= 0; bits-- {
			candidate := netip.PrefixFrom(current, bits).Masked()
			if candidate.Addr() != current || prefixLast(candidate).Compare(end) > 0 {
				break
			}
			prefix = candidate
		}
		cidrs = append(cidrs, prefix.String())
		current = prefixLast(prefix).Next()
	}
	return cidrs
}

func prefixLast(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Masked().Addr().AsSlice()
	for bit := prefix.Bits(); bit < len(bytes)*8; bit++ {
		bytes[bit/8] |= 1 << (7 - bit%8)
	}
	addr, _ := netip.AddrFromSlice(bytes)
	return addr
}
