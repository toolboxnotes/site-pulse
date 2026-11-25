package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Index tracking: every sitemap page, when we first saw it, and whether Google has indexed it.

type PageTrack struct {
	FirstSeen string `json:"firstSeen"`         // date we first saw it in the sitemap
	Initial   bool   `json:"initial,omitempty"` // was already there on the first scan (not "new")
	LastMod   string `json:"lastmod,omitempty"`
	Removed   string `json:"removed,omitempty"` // date it disappeared from the sitemap
	Checked   string `json:"checked,omitempty"` // date of the last URL inspection
	Verdict   string `json:"verdict,omitempty"`
	Coverage  string `json:"coverage,omitempty"`
	LastCrawl string `json:"lastCrawl,omitempty"`
	IndexedOn string `json:"indexedOn,omitempty"` // first date seen as indexed
	Submitted string `json:"submitted,omitempty"` // IndexNow submission date
}

type PageStore struct {
	Updated string                `json:"updated"`
	Pages   map[string]*PageTrack `json:"pages"`
	// last time the sitemaps were resubmitted to Search Console (date) and which files
	SitemapSubmitted string   `json:"sitemapSubmitted,omitempty"`
	SitemapFiles     []string `json:"sitemapFiles,omitempty"`
}

var pagesMu sync.Mutex

func (a *App) pagesPath(siteID string) string {
	return filepath.Join(a.store.dir, "pages-"+siteID+".json")
}

func (a *App) loadPages(siteID string) *PageStore {
	ps := &PageStore{Pages: map[string]*PageTrack{}}
	readJSON(a.pagesPath(siteID), ps)
	if ps.Pages == nil {
		ps.Pages = map[string]*PageTrack{}
	}
	return ps
}

const (
	inspectNewPerRun = 40 // not-yet-indexed pages checked per day
	inspectOldPerRun = 10 // indexed pages re-checked per day (oldest check first)
)

// needsIndexNow: IndexNow is only for new or changed pages. Sending the same unchanged
// URL again does not speed up crawling and can make search engines trust the pings less,
// so a page is sent only if it was never sent, or its sitemap lastmod is newer than the last send.
func needsIndexNow(p *PageTrack) bool {
	if p == nil || p.Removed != "" {
		return false
	}
	if p.Submitted == "" {
		return true
	}
	return len(p.LastMod) >= 10 && p.LastMod[:10] > p.Submitted
}

// trackPages updates the page list from the sitemap and inspects a batch of pages.
// It returns the URLs that are new today (for IndexNow) and a summary for the run.
// Pages whose sitemap lastmod changed are also sent to IndexNow (not counted as new).
func (a *App) trackPages(ctx context.Context, s Site, tok string, canInspect bool) (newURLs []string, summary map[string]any, err error) {
	urls, err := siteSitemapURLs(ctx, s, 5000)
	if err != nil {
		return nil, nil, err
	}
	today := dateStr(time.Now())
	pagesMu.Lock()
	ps := a.loadPages(s.ID)
	pagesMu.Unlock()
	first := len(ps.Pages) == 0
	inSitemap := map[string]bool{}
	var changedURLs []string
	for _, u := range urls {
		inSitemap[u.Loc] = true
		p := ps.Pages[u.Loc]
		if p == nil {
			p = &PageTrack{FirstSeen: today, Initial: first}
			ps.Pages[u.Loc] = p
			if !first {
				newURLs = append(newURLs, u.Loc)
			}
		}
		if p.LastMod != "" && u.LastMod != "" && u.LastMod != p.LastMod {
			changedURLs = append(changedURLs, u.Loc)
		}
		p.LastMod = u.LastMod
		p.Removed = ""
	}
	for u, p := range ps.Pages {
		if !inSitemap[u] && p.Removed == "" {
			p.Removed = today
		}
	}

	if canInspect && s.GSCProperty != "" {
		var notIdx, idx []string
		for u, p := range ps.Pages {
			if p.Removed != "" || p.Checked == today {
				continue
			}
			if p.Verdict == "PASS" {
				if p.Checked < dateStr(time.Now().AddDate(0, 0, -30)) {
					idx = append(idx, u)
				}
			} else {
				notIdx = append(notIdx, u)
			}
		}
		// newest pages first; never-checked before re-checks
		sort.Slice(notIdx, func(i, j int) bool {
			a, b := ps.Pages[notIdx[i]], ps.Pages[notIdx[j]]
			if (a.Checked == "") != (b.Checked == "") {
				return a.Checked == ""
			}
			if a.FirstSeen != b.FirstSeen {
				return a.FirstSeen > b.FirstSeen
			}
			return a.LastMod > b.LastMod
		})
		sort.Slice(idx, func(i, j int) bool { return ps.Pages[idx[i]].Checked < ps.Pages[idx[j]].Checked })
		batch := append(head(notIdx, inspectNewPerRun), head(idx, inspectOldPerRun)...)
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for _, u := range batch {
			wg.Add(1)
			go func(u string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				in := gscInspectURL(ctx, tok, s.GSCProperty, u)
				mu.Lock()
				defer mu.Unlock()
				if in.Error != "" {
					return
				}
				p := ps.Pages[u]
				p.Checked, p.Verdict, p.Coverage, p.LastCrawl = today, in.Verdict, in.Coverage, in.LastCrawl
				if in.Verdict == "PASS" && p.IndexedOn == "" {
					p.IndexedOn = today
				}
			}(u)
		}
		wg.Wait()
	}

	// IndexNow for new pages and pages whose lastmod changed (only ones not yet sent for this version)
	a.store.mu.Lock()
	key := a.store.Config.IndexNowKey
	a.store.mu.Unlock()
	var send []string
	for _, u := range append(append([]string{}, newURLs...), changedURLs...) {
		if p := ps.Pages[u]; needsIndexNow(p) && p.Submitted != today {
			send = append(send, u)
		}
	}
	if key != "" && len(send) > 0 {
		if ok, _ := indexNowVerified(ctx, s, key); ok {
			if err := indexNowSubmit(ctx, s, key, send); err == nil {
				for _, u := range send {
					ps.Pages[u].Submitted = today
				}
				log.Printf("IndexNow: sent %d page(s) for %s (%d new, %d changed)", len(send), s.ID, len(newURLs), len(changedURLs))
			}
		}
	}

	// Search Console: resubmit the sitemaps once a day when new pages appear, so Google
	// reads them now instead of on its own schedule (needs the full webmasters scope)
	if len(newURLs) > 0 && s.GSCProperty != "" && ps.SitemapSubmitted != today && a.canSubmitSitemaps() {
		if files, err := resubmitSitemaps(ctx, s, tok); err == nil && len(files) > 0 {
			ps.SitemapSubmitted, ps.SitemapFiles = today, files
			log.Printf("resubmitted %d sitemap(s) to Search Console for %s (%d new pages)", len(files), s.ID, len(newURLs))
		} else if err != nil {
			log.Printf("sitemap resubmit for %s failed: %v", s.ID, err)
		}
	}

	ps.Updated = today
	pagesMu.Lock()
	writeJSON(a.pagesPath(s.ID), ps)
	pagesMu.Unlock()
	return newURLs, pageSummary(ps), nil
}

func head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func pageSummary(ps *PageStore) map[string]any {
	total, indexed, checked, newN, slow := 0, 0, 0, 0, 0
	weekAgo := dateStr(time.Now().AddDate(0, 0, -7))
	twoWeeks := dateStr(time.Now().AddDate(0, 0, -14))
	var slowURLs []string
	for u, p := range ps.Pages {
		if p.Removed != "" {
			continue
		}
		total++
		if p.Checked != "" {
			checked++
		}
		if p.Verdict == "PASS" {
			indexed++
		}
		if !p.Initial && p.FirstSeen >= twoWeeks {
			newN++
		}
		if !p.Initial && p.Verdict != "PASS" && p.FirstSeen <= weekAgo && p.Checked != "" {
			slow++
			if len(slowURLs) < 5 {
				slowURLs = append(slowURLs, u)
			}
		}
	}
	return map[string]any{"total": total, "indexed": indexed, "checked": checked, "new": newN, "slow": slow, "slowUrls": slowURLs}
}

// ---------- IndexNow (Bing, Yandex, Naver, Seznam …) ----------

func newIndexNowKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func indexNowKeyURL(s Site, key string) string { return originOf(s) + "/" + key + ".txt" }

func indexNowVerified(ctx context.Context, s Site, key string) (bool, string) {
	u := indexNowKeyURL(s, key)
	r, err := fetch(ctx, u, 4096)
	if err != nil {
		return false, shortErr(err)
	}
	if r.Status != 200 {
		return false, fmt.Sprintf("HTTP %d", r.Status)
	}
	if strings.TrimSpace(string(r.Body)) != key {
		return false, "content"
	}
	return true, ""
}

var indexNowEndpoint = "https://api.indexnow.org/indexnow"

func indexNowSubmit(ctx context.Context, s Site, key string, urls []string) error {
	u, err := url.Parse(s.URL)
	if err != nil {
		return err
	}
	for i := 0; i < len(urls); i += 10000 {
		batch := urls[i:min(len(urls), i+10000)]
		body := map[string]any{"host": u.Hostname(), "key": key, "keyLocation": indexNowKeyURL(s, key), "urlList": batch}
		if err := doJSON(ctx, "", "POST", indexNowEndpoint, body, nil); err != nil {
			// 200 and 202 are both success; doJSON treats <300 as success
			return err
		}
	}
	return nil
}

// resubmitSitemaps submits the site's sitemap files to its Search Console property.
// Files come from robots.txt; if it lists none, the sitemaps already registered in
// Search Console for the same site are used.
func resubmitSitemaps(ctx context.Context, s Site, tok string) ([]string, error) {
	files := siteSitemapFiles(ctx, s)
	if len(files) == 0 {
		reg, err := gscSitemaps(ctx, tok, s.GSCProperty)
		if err != nil {
			return nil, err
		}
		prefix := strings.TrimSuffix(pathOf(s), "/")
		for _, sm := range reg {
			if sameHost(sm.Path, s.URL) {
				if pu, err := url.Parse(sm.Path); err == nil && (prefix == "" || strings.HasPrefix(pu.Path, prefix+"/")) {
					files = append(files, sm.Path)
				}
			}
		}
	}
	var done []string
	var firstErr error
	for _, f := range files {
		if err := gscSubmitSitemap(ctx, tok, s.GSCProperty, f); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		done = append(done, f)
	}
	if len(done) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return done, nil
}
