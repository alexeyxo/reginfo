package reginfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
)

const (
	ipv4BootstrapURL = "https://data.iana.org/rdap/ipv4.json"
	ipv6BootstrapURL = "https://data.iana.org/rdap/ipv6.json"
)

type ipService struct {
	prefix  netip.Prefix
	servers []string
}

type ipBootstrap struct {
	mu       sync.Mutex
	loaded   bool
	loading  chan struct{}
	services []ipService
}

func (c *Client) ipServers(ctx context.Context, addr netip.Addr) ([]string, error) {
	state, endpoint := &c.ipv6Bootstrap, ipv6BootstrapURL
	if addr.Is4() {
		state, endpoint = &c.ipv4Bootstrap, ipv4BootstrapURL
	}
	services, err := state.load(ctx, c, endpoint)
	if err != nil {
		return nil, err
	}
	longest := -1
	var servers []string
	for _, service := range services {
		if !service.prefix.Contains(addr) || service.prefix.Bits() < longest {
			continue
		}
		if service.prefix.Bits() > longest {
			servers = nil
			longest = service.prefix.Bits()
		}
		for _, server := range service.servers {
			if !slices.Contains(servers, server) {
				servers = append(servers, server)
			}
		}
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no RDAP provider for IP %s", addr)
	}
	return servers, nil
}

func (b *ipBootstrap) load(ctx context.Context, c *Client, endpoint string) ([]ipService, error) {
	for {
		b.mu.Lock()
		if b.loaded {
			services := b.services
			b.mu.Unlock()
			return services, nil
		}
		if pending := b.loading; pending != nil {
			b.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		b.loading = make(chan struct{})
		b.mu.Unlock()
		break
	}
	body, err := c.get(ctx, endpoint, 2<<20)
	var response bootstrapResponse
	if err == nil {
		err = json.Unmarshal(body, &response)
	}
	if err == nil && response.Services == nil {
		err = errors.New("IP bootstrap response missing services")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.services = parseIPServices(response.Services)
		b.loaded = true
	}
	close(b.loading)
	b.loading = nil
	return b.services, err
}

func parseIPServices(services [][][]string) []ipService {
	var parsed []ipService
	for _, service := range services {
		if len(service) != 2 {
			continue
		}
		for _, raw := range service[0] {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				continue
			}
			parsed = append(parsed, ipService{prefix: prefix.Masked(), servers: service[1]})
		}
	}
	return parsed
}
