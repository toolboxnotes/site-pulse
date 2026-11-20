package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Broken link scan: every sitemap page plus every link found on those pages.

type LinkProblem struct {
	URL       string   `json:"url"`
	Status    int      `json:"status,omitempty"`
	Error     string   `json:"error,omitempty"`
	Redirects int      `json:"redirects,omitempty"`
	Final     string   `json:"final,omitempty"`
	Ms        int64    `json:"ms"`
	Kind      string   `json:"kind"` // broken | redirect_chain | slow
	External  bool     `json:"external,omitempty"`
	FoundOn   []string `json:"foundOn,omitempty"`
}

type LinkScan struct {
	SiteID   string        `json:"siteId"`
	Time     int64         `json:"time"`
	Pages    int           `json:"pages"`   // pages crawled
	Checked  int           `json:"checked"` // unique links checked
	Problems []LinkProblem `json:"problems"`
	Duration int64         `json:"durationMs"`
	Capped   bool          `json:"capped,omitempty"`
}

const (
	linkMaxPages    = 1000
	linkMaxLinks    = 3000
	linkMaxExternal = 400
	linkSlowMs      = 5000
)

var errTooManyRedirects = errors.New("too many redirects")

type linkResult struct {
	status    int
	err       string
	redirects int
	final     string
	ms        int64
	body      []byte
}

// checkLink fetches a URL counting redirects. Body is kept only when wantBody (for crawling).
func checkLink(ctx context.Context, u string, wantBody bool) linkResult {
	hops := 0
	c := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		hops = len(via)
		if len(via) >= 10 {
			return errTooManyRedirects
		}
		return nil
	}}
	method := "HEAD"
	if wantBody {
		method = "GET"
	}
	start := time.Now()
	do := func(m string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, m, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		return c.Do(req)
	}
	res, err := do(method)
	// some servers reject HEAD; retry with GET
	if err == nil && method == "HEAD" && (res.StatusCode == 405 || res.StatusCode == 403 || res.StatusCode == 501) {
		res.Body.Close()
		res, err = do("GET")
	}
	r := linkResult{ms: time.Since(start).Milliseconds(), redirects: hops}
	if err != nil {
		r.err = shortErr(err)
		return r
	}
	defer res.Body.Close()
	r.status = res.StatusCode
	r.final = res.Request.URL.String()
	if wantBody && res.StatusCode == 200 {
		r.body, _ = io.ReadAll(io.LimitReader(res.Body, 4<<20))
	}
	r.ms = time.Since(start).Milliseconds()
	return r
}

func (a *App) runLinkScan(ctx context.Context, s Site) (*LinkScan, error) {
	start := time.Now()
	sm, err := siteSitemapURLs(ctx, s, linkMaxPages)
	if err != nil {
		return nil, err
	}
	scan := &LinkScan{SiteID: s.ID, Time: start.Unix()}
	var mu sync.Mutex
	results := map[string]linkResult{}
	foundOn := map[string][]string{}
	external := map[string]bool{}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup

	// 1. crawl sitemap pages and collect their links
	pages := []string{s.URL}
	for _, u := range sm {
		pages = append(pages, u.Loc)
	}
	seenPage := map[string]bool{}
	var crawl []string
	for _, p := range pages {
		if !seenPage[p] {
			seenPage[p] = true
			crawl = append(crawl, p)
		}
	}
	scan.Pages = len(crawl)
	for _, p := range crawl {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := checkLink(ctx, p, true)
			var links []link
			if r.body != nil {
				links = extractLinks(string(r.body), r.final)
			}
			r.body = nil
			mu.Lock()
			defer mu.Unlock()
			results[p] = r
			for _, l := range links {
				if l.URL == p {
					continue
				}
				if len(foundOn[l.URL]) < 5 {
					foundOn[l.URL] = append(foundOn[l.URL], p)
				}
				if !sameHost(l.URL, s.URL) {
					external[l.URL] = true
				}
			}
		}(p)
	}
	wg.Wait()

	// 2. check links found on the pages (internal first)
	var todo []string
	extCount := 0
	var linksSorted []string
	for u := range foundOn {
		linksSorted = append(linksSorted, u)
	}
	sort.Slice(linksSorted, func(i, j int) bool {
		if external[linksSorted[i]] != external[linksSorted[j]] {
			return !external[linksSorted[i]]
		}
		return linksSorted[i] < linksSorted[j]
	})
	for _, u := range linksSorted {
		if _, done := results[u]; done {
			continue
		}
		if len(results)+len(todo) >= linkMaxLinks {
			scan.Capped = true
			break
		}
		if external[u] {
			if extCount >= linkMaxExternal {
				scan.Capped = true
				continue
			}
			extCount++
		}
		todo = append(todo, u)
	}
	for _, u := range todo {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := checkLink(ctx, u, false)
			mu.Lock()
			results[u] = r
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	scan.Checked = len(results)

	// 3. problems
	for u, r := range results {
		p := LinkProblem{URL: u, Status: r.status, Error: r.err, Redirects: r.redirects, Ms: r.ms, External: external[u], FoundOn: foundOn[u]}
		switch {
		case r.err != "" || r.status >= 400:
			// many sites block bots on HEAD/GET with 403/429: report external ones only when clearly gone
			if p.External && (r.status == 403 || r.status == 429 || r.status == 999) {
				continue
			}
			p.Kind = "broken"
		case r.redirects >= 2 && !p.External:
			p.Kind, p.Final = "redirect_chain", r.final
		case r.ms > linkSlowMs && !p.External:
			p.Kind = "slow"
		default:
			continue
		}
		scan.Problems = append(scan.Problems, p)
	}
	rank := map[string]int{"broken": 0, "redirect_chain": 1, "slow": 2}
	sort.Slice(scan.Problems, func(i, j int) bool {
		a, b := scan.Problems[i], scan.Problems[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		if a.External != b.External {
			return !a.External
		}
		return a.URL < b.URL
	})
	scan.Duration = time.Since(start).Milliseconds()
	a.store.mu.Lock()
	writeJSON(filepath.Join(a.store.dir, "links-"+s.ID+".json"), scan)
	a.store.mu.Unlock()
	return scan, nil
}

func (a *App) loadLinkScan(siteID string) *LinkScan {
	var ls LinkScan
	if readJSON(filepath.Join(a.store.dir, "links-"+siteID+".json"), &ls) != nil {
		return nil
	}
	return &ls
}

// linkSummary is what the daily run reports (and alerts on).
func linkSummary(ls *LinkScan) map[string]any {
	if ls == nil {
		return nil
	}
	broken, brokenInt := 0, 0
	for _, p := range ls.Problems {
		if p.Kind == "broken" {
			broken++
			if !p.External {
				brokenInt++
			}
		}
	}
	return map[string]any{"time": ls.Time, "checked": ls.Checked, "broken": broken, "brokenInternal": brokenInt, "problems": len(ls.Problems)}
}
