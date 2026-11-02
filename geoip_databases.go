package reginfo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oschwald/geoip2-golang/v2"
)

const geoIPGenerationPrefix = ".reginfo-geoip-"

var geoIPEditions = [...]string{"GeoLite2-City", "GeoLite2-Country", "GeoLite2-ASN"}

type geoIPReaders [3]*geoip2.Reader

func (g *GeoIP) LoadDatabases(ctx context.Context) error {
	g.updateMu.Lock()
	defer g.updateMu.Unlock()
	if err := g.canUpdate(ctx); err != nil {
		return err
	}
	directory, err := geoIPDirectory(g.config.Directory)
	if err != nil {
		return err
	}
	readers, err := openGeoIPDatabases(ctx, directory)
	if err != nil {
		return err
	}
	return g.replaceReaders(readers)
}

func (g *GeoIP) UpdateDatabases(ctx context.Context) error {
	g.updateMu.Lock()
	defer g.updateMu.Unlock()
	if err := g.canUpdate(ctx); err != nil {
		return err
	}
	if g.config.AccountID == "" || g.config.LicenseKey == "" {
		return errors.New("MaxMind account ID and license key are required to download GeoIP databases")
	}
	previous, err := geoIPDirectory(g.config.Directory)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(g.config.Directory, 0o750); err != nil {
		return fmt.Errorf("create GeoIP directory: %w", err)
	}
	staged, err := os.MkdirTemp(g.config.Directory, geoIPGenerationPrefix)
	if err != nil {
		return fmt.Errorf("create GeoIP staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staged)
		}
	}()
	for _, edition := range geoIPEditions {
		if err = g.downloadDatabase(ctx, edition, staged); err != nil {
			return fmt.Errorf("download %s: %w", edition, err)
		}
	}
	readers, err := openGeoIPDatabases(ctx, staged)
	if err != nil {
		return err
	}
	if err = publishGeoIPDirectory(ctx, g.config.Directory, staged); err != nil {
		return errors.Join(err, readers.close())
	}
	published = true
	if err = g.replaceReaders(readers); err != nil {
		return fmt.Errorf("GeoIP databases activated; close previous databases: %w", err)
	}
	if previous != g.config.Directory {
		if err = os.RemoveAll(previous); err != nil {
			return fmt.Errorf("GeoIP databases activated; remove previous generation: %w", err)
		}
	}
	return nil
}

func (g *GeoIP) canUpdate(ctx context.Context) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.closed {
		return ErrGeoIPClosed
	}
	return nil
}

func (g *GeoIP) replaceReaders(readers geoIPReaders) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	previous := g.readers
	g.readers = readers
	return previous.close()
}

func openGeoIPDatabases(ctx context.Context, directory string) (geoIPReaders, error) {
	var readers geoIPReaders
	for i, edition := range geoIPEditions {
		if err := ctx.Err(); err != nil {
			return geoIPReaders{}, errors.Join(err, readers.close())
		}
		reader, err := geoip2.Open(filepath.Join(directory, edition+".mmdb"))
		if err != nil {
			return geoIPReaders{}, errors.Join(fmt.Errorf("open %s: %w", edition, err), readers.close())
		}
		readers[i] = reader
		if actual := reader.Metadata().DatabaseType; actual != edition {
			return geoIPReaders{}, errors.Join(fmt.Errorf("%s contains database type %q", edition, actual), readers.close())
		}
	}
	if err := ctx.Err(); err != nil {
		return geoIPReaders{}, errors.Join(err, readers.close())
	}
	return readers, nil
}

func (r geoIPReaders) close() error {
	var failures []error
	for _, reader := range r {
		if reader != nil {
			failures = append(failures, reader.Close())
		}
	}
	return errors.Join(failures...)
}

func geoIPDirectory(directory string) (string, error) {
	file, err := os.Open(filepath.Join(directory, "CURRENT"))
	if errors.Is(err, os.ErrNotExist) {
		return directory, nil
	}
	if err != nil {
		return "", fmt.Errorf("open GeoIP CURRENT: %w", err)
	}
	defer file.Close()
	data, err := readLimited(file, 256)
	if err != nil {
		return "", fmt.Errorf("read GeoIP CURRENT: %w", err)
	}
	name := strings.TrimSpace(string(data))
	if !strings.HasPrefix(name, geoIPGenerationPrefix) || strings.ContainsAny(name, "/\\") || name == geoIPGenerationPrefix {
		return "", errors.New("invalid GeoIP CURRENT generation")
	}
	return filepath.Join(directory, name), nil
}

func publishGeoIPDirectory(ctx context.Context, directory, staged string) error {
	file, err := os.CreateTemp(directory, ".CURRENT-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.WriteString(filepath.Base(staged) + "\n")
	err = errors.Join(writeErr, file.Close())
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(directory, "CURRENT"))
}
