package main

import (
	"context"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// AdSense readiness checklist.

type AuditItem struct {
	ID     string         `json:"id"`
	Status string         `json:"status"` // ok | warn | fail | skip
	Detail map[string]any `json:"detail,omitempty"`
}

type Audit struct {
	SiteID string      `json:"siteId"`
	Time   int64       `json:"time"`
	Items  []AuditItem `json:"items"`
}

var (
	rePrivacy = regexp.MustCompile(`(?i)privacy|개인정보|개인 정보|隐私|隱私|プライバシー`)
	reAbout   = regexp.MustCompile(`(?i)\babout\b|about-us|aboutus|소개|关于|關於|運営者|会社概要`)
	reContact = regexp.MustCompile(`(?i)contact|문의|연락|联系|聯繫|お問い合わせ`)
	reListing = regexp.MustCompile(`(?i)/(archive|archives|tag|tags|category|categories|page/\d+|author|search)(/|$)`)
	// numbered list pages such as /2/, /ko/3/ (Astro, Hugo and others paginate this way)
	rePagination = regexp.MustCompile(`/\d{1,4}/?$`)
	// language home pages such as /ko, /en, /zh-cn
	reLangRoot    = regexp.MustCompile(`(?i)^/[a-z]{2}(-[a-z]{2,4})?$`)
	reTerms       = regexp.MustCompile(`(?i)terms|이용약관|약관|条款|條款|利用規約`)
	rePlaceholder = regexp.MustCompile(`(?i)lorem ipsum|coming soon|under construction|준비\s?중입니다|준비중입니다|공사\s?중|建设中|建設中|敬请期待|敬請期待|即将推出|hello world!|sample page|this is an example page`)
)

const thinWords = 300

func (a *App) runAudit(ctx context.Context, s Site) *Audit {
	au := &Audit{SiteID: s.ID, Time: time.Now().Unix()}
	add := func(id, st string, d map[string]any) {
		au.Items = append(au.Items, AuditItem{ID: id, Status: st, Detail: d})
	}

	// 1. homepage
	home, err := fetchPage(ctx, s.URL, 4<<20)
	if err != nil || home.Status >= 400 {
		d := map[string]any{"url": s.URL}
		if err != nil {
			d["error"] = shortErr(err)
		} else {
			d["status"] = home.Status
		}
		add("reachable", "fail", d)
		return au
	}
	homeHTML := string(home.Body)
	if strings.HasPrefix(home.Final, "https://") {
		add("reachable", "ok", map[string]any{"url": home.Final, "ms": home.Duration.Milliseconds()})
	} else {
		add("reachable", "warn", map[string]any{"url": home.Final, "https": false})
	}

	// 2. required pages: links on the homepage (after redirects), else sitemap addresses
	links := extractLinks(homeHTML, home.Final)
	urls, smErr := siteSitemapURLs(ctx, s, 5000)
	if len(urls) == 0 && smErr == nil {
		// the site URL may be a redirecting root; use the landing page's section instead
		alt := s
		alt.URL = home.Final
		urls, smErr = siteSitemapURLs(ctx, alt, 5000)
	}
	findPage := func(re *regexp.Regexp) (string, string) {
		if u := pickRequiredPage(links, re, home.Final); u != "" {
			return u, "home"
		}
		for _, u := range urls {
			if pu, err := url.Parse(u.Loc); err == nil && re.MatchString(pu.Path) {
				return u.Loc, "sitemap"
			}
		}
		return "", ""
	}
	type req struct {
		id   string
		re   *regexp.Regexp
		need bool
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	reqItems := map[string]AuditItem{}
	for _, r := range []req{{"privacy", rePrivacy, true}, {"about", reAbout, true}, {"contact", reContact, true}, {"terms", reTerms, false}} {
		u, via := findPage(r.re)
		wg.Add(1)
		go func(r req, u, via string) {
			defer wg.Done()
			it := AuditItem{ID: "page_" + r.id}
			switch {
			case u == "" && r.need:
				it.Status = "fail"
			case u == "":
				it.Status = "warn"
			default:
				p, err := fetch(ctx, u, 2<<20)
				if err != nil || p.Status >= 400 {
					it.Status = "fail"
					it.Detail = map[string]any{"url": u, "broken": true}
				} else {
					words := contentWords(mainText(string(p.Body)))
					it.Status = "ok"
					if words < 80 {
						it.Status = "warn"
					}
					it.Detail = map[string]any{"url": u, "words": words, "via": via}
					if via == "sitemap" && it.Status == "ok" {
						it.Status = "warn" // exists, but visitors can't reach it from the homepage
					}
				}
			}
			mu.Lock()
			reqItems[r.id] = it
			mu.Unlock()
		}(r, u, via)
	}
	wg.Wait()
	for _, id := range []string{"privacy", "about", "contact", "terms"} {
		au.Items = append(au.Items, reqItems[id])
	}

	// 3. content amount and quality, from the sitemap
	if smErr != nil {
		add("content_count", "fail", map[string]any{"error": shortErr(smErr)})
	} else {
		n := len(urls)
		st := "ok"
		if n < 10 {
			st = "fail"
		} else if n < 25 {
			st = "warn"
		}
		add("content_count", st, map[string]any{"count": n})
	}
	var articles []SitemapURL
	for _, u := range urls {
		pu, err := url.Parse(u.Loc)
		if err != nil {
			continue
		}
		p := strings.TrimSuffix(pu.Path, "/")
		// leave the homepage, legal/utility pages and listing pages out of the article sample
		if p == "" || p == strings.TrimSuffix(pathOf(s), "/") || p == strings.TrimSuffix(pathOfURL(home.Final), "/") ||
			rePrivacy.MatchString(p) || reAbout.MatchString(p) || reContact.MatchString(p) || reTerms.MatchString(p) || reListing.MatchString(p) || rePagination.MatchString(p) || reLangRoot.MatchString(p) {
			continue
		}
		articles = append(articles, u)
	}
	sample := newest(articles)
	if len(sample) > 25 {
		sample = sample[:25]
	}
	type pageRes struct {
		URL, Title string
		Words      int
		Status     int
		Holder     string
		Err        string
	}
	res := make([]pageRes, len(sample))
	sem := make(chan struct{}, 6)
	for i, u := range sample {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := pageRes{URL: u}
			p, err := fetch(ctx, u, 4<<20)
			if err != nil {
				r.Err = shortErr(err)
			} else {
				r.Status = p.Status
				h := string(p.Body)
				r.Title = pageTitle(h)
				txt := mainText(h)
				r.Words = contentWords(txt)
				if m := rePlaceholder.FindString(txt); m != "" {
					r.Holder = m
				}
			}
			res[i] = r
		}(i, u.Loc)
	}
	wg.Wait()
	var thin, broken, holders []map[string]any
	totalWords, okPages := 0, 0
	titles := map[string][]string{}
	for _, r := range res {
		if r.Err != "" || r.Status >= 400 {
			broken = append(broken, map[string]any{"url": r.URL, "status": r.Status, "error": r.Err})
			continue
		}
		okPages++
		totalWords += r.Words
		if r.Words < thinWords {
			thin = append(thin, map[string]any{"url": r.URL, "words": r.Words})
		}
		if r.Holder != "" {
			holders = append(holders, map[string]any{"url": r.URL, "text": r.Holder})
		}
		if r.Title != "" {
			titles[r.Title] = append(titles[r.Title], r.URL)
		}
	}
	if okPages > 0 {
		avg := totalWords / okPages
		st := "ok"
		if len(thin)*10 > okPages*3 { // more than 30% thin
			st = "fail"
		} else if len(thin) > 0 {
			st = "warn"
		}
		add("thin_content", st, map[string]any{"checked": okPages, "avg": avg, "thin": thin, "min": thinWords})
	}
	if rePlaceholder.MatchString(mainText(homeHTML)) {
		holders = append(holders, map[string]any{"url": home.Final, "text": rePlaceholder.FindString(mainText(homeHTML))})
	}
	if len(holders) > 0 {
		add("placeholder", "fail", map[string]any{"pages": holders})
	} else {
		add("placeholder", "ok", nil)
	}
	if len(broken) > 0 {
		add("sample_broken", "fail", map[string]any{"pages": broken})
	} else if len(sample) > 0 {
		add("sample_broken", "ok", map[string]any{"checked": len(sample)})
	}
	var dups []map[string]any
	for t, us := range titles {
		if len(us) > 1 {
			dups = append(dups, map[string]any{"title": t, "urls": us})
		}
	}
	if len(dups) > 0 {
		add("dup_titles", "warn", map[string]any{"groups": dups})
	} else if okPages > 1 {
		add("dup_titles", "ok", nil)
	}

	// 4. navigation, mobile, language, title/description
	internal := 0
	for _, l := range links {
		if sameHost(l.URL, home.Final) {
			internal++
		}
	}
	if reNav.MatchString(homeHTML) || internal >= 8 {
		add("navigation", "ok", map[string]any{"links": internal})
	} else {
		add("navigation", "warn", map[string]any{"links": internal})
	}
	if reViewport.MatchString(homeHTML) {
		add("mobile", "ok", nil)
	} else {
		add("mobile", "fail", nil)
	}
	if m := reHTMLLang.FindStringSubmatch(homeHTML); m != nil {
		add("lang", "ok", map[string]any{"lang": m[1]})
	} else {
		add("lang", "warn", nil)
	}
	if pageTitle(homeHTML) != "" && reMetaDesc.MatchString(homeHTML) {
		add("meta", "ok", map[string]any{"title": pageTitle(homeHTML)})
	} else {
		add("meta", "warn", map[string]any{"title": pageTitle(homeHTML), "description": reMetaDesc.MatchString(homeHTML)})
	}

	// 5. crawlability and AdSense technical pieces
	lowHome := strings.ToLower(homeHTML)
	if regexp.MustCompile(`<meta[^>]+name=["']robots["'][^>]+noindex`).MatchString(lowHome) {
		add("indexable", "fail", map[string]any{"noindex": true})
	} else {
		rb, _ := checkRobotsSitemap(ctx, s)
		if b, _ := rb["blocksAll"].(bool); b {
			add("indexable", "fail", map[string]any{"robotsBlocksAll": true})
		} else {
			add("indexable", "ok", nil)
		}
	}
	if strings.Contains(lowHome, "pagead2.googlesyndication.com") || strings.Contains(lowHome, "adsbygoogle") {
		add("adsense_code", "ok", nil)
	} else {
		add("adsense_code", "fail", nil)
	}
	pubID := ""
	tok, terr := a.accessToken(ctx)
	if terr == nil {
		if accts, err := adsAccounts(ctx, tok); err == nil && len(accts) > 0 {
			pubID = accts[0].PubID()
		}
	}
	at := checkAdsTxt(ctx, s, pubID)
	switch {
	case at["found"] == false:
		add("adstxt", "fail", map[string]any{"pubId": pubID})
	case at["ok"] == true:
		add("adstxt", "ok", map[string]any{"pubId": pubID})
	default:
		add("adstxt", "fail", map[string]any{"pubId": pubID, "mismatch": true})
	}
	if terr == nil && s.GSCProperty != "" {
		in := gscInspectURL(ctx, tok, s.GSCProperty, home.Final)
		switch {
		case in.Error != "":
			add("indexed", "skip", map[string]any{"error": in.Error})
		case in.Verdict == "PASS":
			add("indexed", "ok", map[string]any{"url": home.Final})
		default:
			add("indexed", "fail", map[string]any{"url": home.Final, "coverage": in.Coverage})
		}
	} else {
		add("indexed", "skip", nil)
	}

	a.store.mu.Lock()
	writeJSON(filepath.Join(a.store.dir, "audit-"+s.ID+".json"), au)
	a.store.mu.Unlock()
	return au
}

func (a *App) loadAudit(siteID string) *Audit {
	var au Audit
	if readJSON(filepath.Join(a.store.dir, "audit-"+siteID+".json"), &au) != nil {
		return nil
	}
	return &au
}

func pathOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Path
}

// pickRequiredPage finds the homepage link to a required page (privacy, about…).
// A tag or archive link such as /archive/?tag=privacy is a list of posts, not the
// page itself, so listing paths and query-string links are skipped. A link whose
// path names the page (/privacy/) wins over one where only the link text matches.
func pickRequiredPage(links []link, re *regexp.Regexp, base string) string {
	textMatch := ""
	for _, l := range links {
		if !sameHost(l.URL, base) {
			continue
		}
		pu, err := url.Parse(l.URL)
		if err != nil || pu.RawQuery != "" || reListing.MatchString(pu.Path) {
			continue
		}
		if re.MatchString(pu.Path) {
			return l.URL
		}
		if textMatch == "" && re.MatchString(l.Text) {
			textMatch = l.URL
		}
	}
	return textMatch
}
