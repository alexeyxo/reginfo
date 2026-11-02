package reginfo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

func (a *Client) allTLDs(ctx context.Context) ([]string, error) {
	b, e := a.get(ctx, tldListURL, 200<<10)
	if e != nil {
		return nil, e
	}
	var o []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.ToLower(strings.TrimSpace(l))
		if l != "" && !strings.HasPrefix(l, "#") {
			o = append(o, l)
		}
	}
	return o, nil
}

func (a *Client) ScanProviders(ctx context.Context, workers int) ([]Provider, error) {
	if workers < 1 || workers > 5 {
		return nil, errors.New("workers must be between 1 and 5")
	}
	a.init()
	if err := a.bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	tlds, e := a.allTLDs(ctx)
	if e != nil {
		return nil, e
	}
	jobs := make(chan string)
	type result struct {
		provider Provider
		err      error
	}
	results := make(chan result, len(tlds))
	var wg sync.WaitGroup
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				p, err := a.provider(ctx, t)
				results <- result{provider: p, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, t := range tlds {
			select {
			case jobs <- t:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	o := make([]Provider, 0, len(tlds))
	var failures []error
	for r := range results {
		o = append(o, r.provider)
		if isRateLimitError(r.err) {
			failures = append(failures, r.err)
		}
	}
	sort.Slice(o, func(i, j int) bool { return o[i].TLD < o[j].TLD })
	return o, errors.Join(append(failures, ctx.Err())...)
}
