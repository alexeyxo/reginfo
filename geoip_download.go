package reginfo

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
)

const (
	geoIPDownloadURL = "https://download.maxmind.com/geoip/databases/"
	geoIPMaxDownload = 256 << 20
	geoIPMaxArchive  = 512 << 20
	geoIPMaxDatabase = 256 << 20
)

func (g *GeoIP) downloadDatabase(ctx context.Context, edition, directory string) error {
	response, err := newHTTPRequest(ctx, g.http, g.config.RateLimiter, true).
		SetBasicAuth(g.config.AccountID, g.config.LicenseKey).
		Get(geoIPDownloadURL + edition + "/download?suffix=tar.gz")
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return err
	}
	if response.StatusCode() != http.StatusOK {
		return fmt.Errorf("MaxMind download: HTTP %d", response.StatusCode())
	}
	compressed := &io.LimitedReader{R: response.Body, N: geoIPMaxDownload + 1}
	err = extractGeoIPDatabase(ctx, compressed, edition, directory)
	if compressed.N == 0 {
		return fmt.Errorf("GeoIP download exceeds %d bytes", geoIPMaxDownload)
	}
	return err
}

func extractGeoIPDatabase(ctx context.Context, source io.Reader, edition, directory string) error {
	compressed, err := gzip.NewReader(source)
	if err != nil {
		return fmt.Errorf("open GeoIP gzip: %w", err)
	}
	defer compressed.Close()
	expanded := &io.LimitedReader{R: geoIPContextReader{ctx: ctx, reader: compressed}, N: geoIPMaxArchive + 1}
	archive := tar.NewReader(expanded)
	found := false
	for {
		header, nextErr := archive.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("read GeoIP archive: %w", nextErr)
		}
		if header.Size < 0 || header.Size > geoIPMaxDatabase {
			return errors.New("GeoIP archive entry exceeds size limit")
		}
		if path.Base(header.Name) != edition+".mmdb" {
			continue
		}
		if found || header.Typeflag != tar.TypeReg || header.Size == 0 {
			return errors.New("GeoIP archive must contain exactly one regular, nonempty database file")
		}
		if err = writeGeoIPDatabase(archive, filepath.Join(directory, edition+".mmdb")); err != nil {
			return err
		}
		found = true
	}
	if _, err = io.Copy(io.Discard, expanded); err != nil {
		return fmt.Errorf("read GeoIP gzip trailer: %w", err)
	}
	if expanded.N == 0 {
		return fmt.Errorf("GeoIP archive exceeds %d bytes", geoIPMaxArchive)
	}
	if !found {
		return fmt.Errorf("GeoIP archive does not contain %s.mmdb", edition)
	}
	return ctx.Err()
}

func writeGeoIPDatabase(source io.Reader, filename string) error {
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, source)
	return errors.Join(copyErr, file.Close())
}

type geoIPContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r geoIPContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
