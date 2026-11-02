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
	const (
		directory       = "./build/geoip"
		ip              = "108.61.78.59"
		updateDatabases = false
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	httpClient := resty.New().SetTimeout(5 * time.Minute).SetCookieJar(nil)
	defer httpClient.Close()
	geo, err := reginfo.NewGeoIP(reginfo.GeoIPConfig{
		HTTPClient:  httpClient,
		Directory:   directory,
		AccountID:   os.Getenv("MAXMIND_ACCOUNT_ID"),
		LicenseKey:  os.Getenv("MAXMIND_LICENSE_KEY"),
		RateLimiter: reginfo.NewDefaultRateLimiter(),
	})
	if err != nil {
		return err
	}
	defer geo.Close()
	if updateDatabases {
		err = geo.UpdateDatabases(ctx)
	} else {
		err = geo.LoadDatabases(ctx)
	}
	if err != nil {
		return err
	}
	client := reginfo.NewClient(reginfo.WithGeoIP(geo))
	info, err := client.LookupGeoIP(ctx, ip)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(info)
}
