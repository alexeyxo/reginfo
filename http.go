package reginfo

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"resty.dev/v3"
)

var httpClientMu sync.Mutex

type httpRequestOptions struct {
	limiter *RateLimiter
	geoIP   bool
}

type httpRequestOptionsKey struct{}

type clientTransport struct {
	base  http.RoundTripper
	owner *resty.Client
}

func prepareHTTPClient(client *resty.Client, timeout time.Duration) *resty.Client {
	if client == nil {
		client = resty.New().SetTimeout(timeout).SetCookieJar(nil)
	}
	httpClientMu.Lock()
	defer httpClientMu.Unlock()
	base := client.Transport()
	if transport, ok := base.(*clientTransport); ok {
		if transport.owner == client {
			return client
		}
		base = transport.base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	redirect := client.Client().CheckRedirect
	client.SetTransport(&clientTransport{base: base, owner: client}).
		SetRedirectPolicy(resty.RedirectPolicyFunc(func(req *http.Request, via []*http.Request) error {
			options, ok := via[0].Context().Value(httpRequestOptionsKey{}).(httpRequestOptions)
			geoIP := ok && options.geoIP
			if len(via) >= 10 && (geoIP || redirect == nil) {
				return errors.New("stopped after 10 redirects")
			}
			if redirect != nil {
				if err := redirect(req, via); err != nil {
					return err
				}
			}
			if geoIP {
				if req.URL.Scheme != schemeHTTPS {
					return errors.New("GeoIP download redirect requires HTTPS")
				}
				if req.URL.Host != via[0].URL.Host {
					req.Header.Del("Authorization")
				}
			}
			return nil
		}))
	return client
}

func newHTTPRequest(ctx context.Context, client *resty.Client, limiter *RateLimiter, geoIP bool) *resty.Request {
	ctx = context.WithValue(ctx, httpRequestOptionsKey{}, httpRequestOptions{limiter: limiter, geoIP: geoIP})
	return client.R().SetContext(ctx).SetHeader("User-Agent", "reginfo-go/1.0").
		SetRetryCount(0).SetResponseDoNotParse(true)
}

func (t *clientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	options, ok := req.Context().Value(httpRequestOptionsKey{}).(httpRequestOptions)
	if !ok || options.limiter == nil {
		return t.base.RoundTrip(req)
	}
	limited := rateLimitedTransport{base: t.base, limiter: options.limiter}
	return limited.RoundTrip(req)
}

func (t *clientTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
