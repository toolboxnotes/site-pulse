package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// ---------- sitemap URL listing ----------

type SitemapURL struct {
	Loc     string `json:"loc"`
	LastMod string `json:"lastmod,omitempty"`
}

// listSitemap returns page URLs from a sitemap (following a sitemap index one level deep).
func listSitemap(ctx context.Context, u string, depth, max int) ([]SitemapURL, error) {
	r, err := fetch(ctx, u, 50<<20)
	if err != nil {
		return nil, err
	}
	if r.Status != 200 {
		return nil, fmt.Errorf("HTTP %d", r.Status)
	}
	dec := xml.NewDecoder(strings.NewReader(string(r.Body)))
	dec.Strict = false
	var out []SitemapURL
	var children []string
	var cur SitemapURL
	inURL, inSitemap, field := false, false, ""
	for {
		tok, e := dec.Token()
		if e != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "url":
				inURL, cur = true, SitemapURL{}
			case "sitemap":
				inSitemap = true
			case "loc", "lastmod":
				field = t.Name.Local
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "url":
				inURL = false
				if cur.Loc != "" && len(out) < max {
					out = append(out, cur)
				}
			case "sitemap":
				inSitemap = false
			case "loc", "lastmod":
				field = ""
			}
		case xml.CharData:
			v := strings.TrimSpace(string(t))
			if v == "" {
				continue
			}
			if inURL && field == "loc" {
				cur.Loc = v
			} else if inURL && field == "lastmod" {
				cur.LastMod = v
			} else if inSitemap && field == "loc" {
				children = append(children, v)
			}
		}
	}
	if depth == 0 {
		for i, c := range children {
			if i >= 50 || len(out) >= max {
				break
			}
			sub, err := listSitemap(ctx, c, 1, max-len(out))
			if err == nil {
				out = append(out, sub...)
			}
		}
	}
	return out, nil
}

// siteSitemapURLs collects page URLs from the site's sitemaps, limited to the site's path (e.g. /ko/).
func siteSitemapURLs(ctx context.Context, s Site, max int) ([]SitemapURL, error) {
	origin := originOf(s)
	var sms []string
	if r, err := fetch(ctx, origin+"/robots.txt", 512<<10); err == nil && r.Status == 200 {
		for _, line := range strings.Split(string(r.Body), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
			if ok && strings.EqualFold(strings.TrimSpace(k), "sitemap") {
				sms = append(sms, strings.TrimSpace(v))
			}
		}
	}
	if len(sms) == 0 {
		// common locations (WordPress, Astro, Yoast, per-language folders)
		sms = []string{origin + "/sitemap.xml", origin + "/sitemap-index.xml", origin + "/sitemap_index.xml", origin + "/wp-sitemap.xml"}
		if p := strings.TrimSuffix(pathOf(s), "/"); p != "" {
			sms = append([]string{origin + p + "/sitemap-index.xml", origin + p + "/sitemap.xml"}, sms...)
		}
	}
	seen := map[string]bool{}
	var out []SitemapURL
	var firstErr error
	for i, sm := range sms {
		if i >= 6 {
			break
		}
		list, err := listSitemap(ctx, sm, 0, max*3)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if len(list) == 0 {
			continue
		}
		for _, u := range list {
			if !seen[u.Loc] && inSiteSection(s, u.Loc) {
				seen[u.Loc] = true
				out = append(out, u)
			}
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

// inSiteSection: same host (ignoring www) and under the site's path.
func inSiteSection(s Site, raw string) bool {
	su, err1 := url.Parse(s.URL)
	u, err2 := url.Parse(raw)
	if err1 != nil || err2 != nil {
		return false
	}
	if strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") != strings.TrimPrefix(strings.ToLower(su.Hostname()), "www.") {
		return false
	}
	p := su.Path
	if p == "" || p == "/" {
		return true
	}
	return strings.HasPrefix(u.Path, p) || strings.TrimSuffix(u.Path, "/") == strings.TrimSuffix(p, "/")
}

// ---------- HTML helpers (regex based; good enough for checks) ----------

var (
	reScript       = regexp.MustCompile(`(?is)<(script|style|noscript|svg|template)\b.*?</(script|style|noscript|svg|template)>`)
	reComment      = regexp.MustCompile(`(?s)<!--.*?-->`)
	reTag          = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpace        = regexp.MustCompile(`\s+`)
	reAnchor       = regexp.MustCompile(`(?is)<a\b([^>]*)>(.*?)</a>`)
	reHref         = regexp.MustCompile(`(?is)\bhref\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	reTitle        = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reMetaDesc     = regexp.MustCompile(`(?is)<meta[^>]+name\s*=\s*["']description["'][^>]*>`)
	reViewport     = regexp.MustCompile(`(?is)<meta[^>]+name\s*=\s*["']viewport["'][^>]*>`)
	reHTMLLang     = regexp.MustCompile(`(?is)<html[^>]*\blang\s*=\s*["']?([A-Za-z-]+)`)
	reMain         = regexp.MustCompile(`(?is)<(main|article)\b[^>]*>(.*)</(main|article)>`)
	reNav          = regexp.MustCompile(`(?is)<nav\b`)
	reHeaderFooter = regexp.MustCompile(`(?is)<(header|footer|nav|aside)\b.*?</(header|footer|nav|aside)>`)
)

func visibleText(h string) string {
	h = reComment.ReplaceAllString(h, " ")
	h = reScript.ReplaceAllString(h, " ")
	h = reTag.ReplaceAllString(h, " ")
	return strings.TrimSpace(reSpace.ReplaceAllString(html.UnescapeString(h), " "))
}

// mainText is the page's body text without header, footer, nav and sidebars.
func mainText(h string) string {
	if m := reMain.FindStringSubmatch(h); m != nil {
		return visibleText(m[2])
	}
	return visibleText(reHeaderFooter.ReplaceAllString(h, " "))
}

// contentWords counts words; Chinese/Japanese characters count as half a word each
// (Korean uses spaces between words, so it is counted like English).
func contentWords(text string) int {
	words, han := 0, 0
	for _, tok := range strings.Fields(text) {
		hasLetter := false
		for _, r := range tok {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
				han++
			} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
				hasLetter = true
			}
		}
		if hasLetter {
			words++
		}
	}
	return words + han/2
}

type link struct {
	URL  string
	Text string
}

func extractLinks(h, base string) []link {
	b, err := url.Parse(base)
	if err != nil {
		return nil
	}
	var out []link
	for _, m := range reAnchor.FindAllStringSubmatch(h, -1) {
		hm := reHref.FindStringSubmatch(m[1])
		if hm == nil {
			continue
		}
		raw := hm[2] + hm[3] + hm[4]
		raw = html.UnescapeString(strings.TrimSpace(raw))
		if raw == "" || strings.HasPrefix(raw, "#") || strings.HasPrefix(strings.ToLower(raw), "javascript:") ||
			strings.HasPrefix(strings.ToLower(raw), "mailto:") || strings.HasPrefix(strings.ToLower(raw), "tel:") {
			continue
		}
		u, err := b.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		// Cloudflare's email obfuscation rewrites mailto links to /cdn-cgi/l/email-protection,
		// which a script decodes in the browser; fetched directly it is a 404, not a broken link.
		if strings.HasPrefix(u.Path, "/cdn-cgi/") {
			continue
		}
		u.Fragment = ""
		out = append(out, link{URL: u.String(), Text: visibleText(m[2])})
	}
	return out
}

func sameHost(a, b string) bool {
	ua, e1 := url.Parse(a)
	ub, e2 := url.Parse(b)
	if e1 != nil || e2 != nil {
		return false
	}
	return strings.TrimPrefix(strings.ToLower(ua.Hostname()), "www.") == strings.TrimPrefix(strings.ToLower(ub.Hostname()), "www.")
}

func pageTitle(h string) string {
	if m := reTitle.FindStringSubmatch(h); m != nil {
		return strings.TrimSpace(html.UnescapeString(reSpace.ReplaceAllString(m[1], " ")))
	}
	return ""
}

// newest sorts sitemap URLs by lastmod, newest first (URLs without lastmod keep their order at the end).
func newest(list []SitemapURL) []SitemapURL {
	out := append([]SitemapURL(nil), list...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastMod > out[j].LastMod })
	return out
}

// siteSitemapFiles lists the sitemap files a site declares in robots.txt, limited to the
// site's own section when its address has a path (e.g. only /ko/ sitemaps for /ko/).
func siteSitemapFiles(ctx context.Context, s Site) []string {
	origin := originOf(s)
	prefix := strings.TrimSuffix(pathOf(s), "/")
	var out []string
	if r, err := fetch(ctx, origin+"/robots.txt", 512<<10); err == nil && r.Status == 200 {
		for _, line := range strings.Split(string(r.Body), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "sitemap") {
				continue
			}
			v = strings.TrimSpace(v)
			if pu, err := url.Parse(v); err == nil && (prefix == "" || strings.HasPrefix(pu.Path, prefix+"/")) {
				out = append(out, v)
			}
		}
	}
	return out
}
