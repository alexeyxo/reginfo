package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/alexeyxo/reginfo"

	"resty.dev/v3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*60)
	defer cancel()
	limiter, err := reginfo.NewRateLimiter(reginfo.RateLimitConfig{
		Store: reginfo.NewMemoryRateLimitStore(),
	})
	if err != nil {
		return err
	}
	cache := reginfo.NewDefaultMemoryCache()
	ipCache := reginfo.NewDefaultIPMemoryCache()
	httpClient := resty.New().SetTimeout(12 * time.Second).SetCookieJar(nil)
	defer httpClient.Close()
	client := reginfo.NewClient(
		reginfo.WithHTTPClient(httpClient),
		reginfo.WithRateLimiter(limiter),
		reginfo.WithCache(cache),
		reginfo.WithIPCache(ipCache),
	)

	domain := "github.io"
	domainInfo, err := client.Lookup(ctx, domain)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", domain, err)
	}

	ipv4 := "108.61.78.59"
	ipv4Info, err := client.LookupIP(ctx, ipv4)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", ipv4, err)
	}

	ipv6 := "2001:4860:4860::8888"
	ipv6Info, err := client.LookupIP(ctx, ipv6)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", ipv6, err)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(map[string]any{
		"domain": domainInfo,
		"ipv4":   ipv4Info,
		"ipv6":   ipv6Info,
	})
}
