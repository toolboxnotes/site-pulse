package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 60 * time.Second}

const userAgent = "SitePulse/" + appVersion + " (+local status check)"

var (
	bingBase = "https://ssl.bing.com/webmaster/api.svc/json"
	rdapBase = "https://rdap.org/domain/"
)

// ---------- Bing ----------

var bingDateRe = regexp.MustCompile(`/Date\((-?\d+)([+-]\d{4})?\)/`)

func parseBingDate(s string) (time.Time, bool) {
	m := bingDateRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	ms, _ := strconv.ParseInt(m[1], 10, 64)
	t := time.UnixMilli(ms).UTC()
	if m[2] != "" {
		sign := 1
		if m[2][0] == '-' {
			sign = -1
		}
		h, _ := strconv.Atoi(m[2][1:3])
		mi, _ := strconv.Atoi(m[2][3:5])
		t = t.In(time.FixedZone("", sign*(h*3600+mi*60)))
	}
	return t, true
}

func bingSites(ctx context.Context, key string) ([]string, error) {
	var r struct {
		D []struct {
			URL        string `json:"Url"`
			IsVerified bool   `json:"IsVerified"`
		} `json:"d"`
	}
	if err := getJSON(ctx, "", bingBase+"/GetUserSites?apikey="+url.QueryEscape(key), &r); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range r.D {
		out = append(out, s.URL)
	}
	return out, nil
}

func bingDaily(ctx context.Context, key, site string) (map[string]map[string]float64, error) {
	var r struct {
		D []struct {
			Clicks      float64 `json:"Clicks"`
			Impressions float64 `json:"Impressions"`
			Date        string  `json:"Date"`
		} `json:"d"`
	}
	u := bingBase + "/GetRankAndTrafficStats?siteUrl=" + url.QueryEscape(site) + "&apikey=" + url.QueryEscape(key)
	if err := getJSON(ctx, "", u, &r); err != nil {
		return nil, err
	}
	out := map[string]map[string]float64{}
	for _, d := range r.D {
		t, ok := parseBingDate(d.Date)
		if !ok {
			continue
		}
		out[t.Format("2006-01-02")] = map[string]float64{"bing_clicks": d.Clicks, "bing_impr": d.Impressions}
	}
	return out, nil
}

// ---------- site status ----------

type fetchResult struct {
	Status   int
	Body     []byte
	Final    string
	Duration time.Duration
	TLS      *tls.ConnectionState
}

var (
	reMetaRefresh = regexp.MustCompile(`(?is)<meta[^>]+http-equiv\s*=\s*["']?refresh["']?[^>]*content\s*=\s*["']?\s*\d*\s*;?\s*url\s*=\s*([^"'>\s]+)`)
	// A literal target only: the quoted string must close the call or statement
	// (location.replace("/" + lang) is a computed target, handled below).
	reJSRedirect = regexp.MustCompile(`(?is)(?:window\.)?location(?:\.href)?\s*(?:=|\.replace\(|\.assign\()\s*["']([^"']+)["']\s*(?:\)|;|$|\n|<)`)
	// A script redirect whose target is computed (e.g. location.replace("/" + lang + "/")).
	reJSComputed = regexp.MustCompile(`(?is)\blocation(?:\.href)?\s*=[^=]|\blocation\.(?:replace|assign)\s*\(`)
	reLinkTag    = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	reAttrRel    = regexp.MustCompile(`(?is)\brel\s*=\s*["']?alternate\b`)
	reAttrLang   = regexp.MustCompile(`(?is)\bhreflang\s*=\s*["']?([a-z0-9-]+)`)
	reAttrHref   = regexp.MustCompile(`(?is)\bhref\s*=\s*["']([^"']+)["']`)
)

// computedRedirectTarget picks where a language-detecting redirect page would send a
// visitor when the script builds the URL at runtime: the hreflang x-default
// alternate, otherwise the first hreflang alternate. Returns "" if there is none.
func computedRedirectTarget(body string) string {
	if !reJSComputed.MatchString(body) {
		return ""
	}
	first := ""
	for _, tag := range reLinkTag.FindAllString(body, -1) {
		if !reAttrRel.MatchString(tag) {
			continue
		}
		lm := reAttrLang.FindStringSubmatch(tag)
		hm := reAttrHref.FindStringSubmatch(tag)
		if lm == nil || hm == nil {
			continue
		}
		if strings.EqualFold(lm[1], "x-default") {
			return hm[1]
		}
		if first == "" {
			first = hm[1]
		}
	}
	return first
}

// fetchPage is fetch plus following page-level redirects (meta refresh or a simple
// location= script), which static sites often use for "/ → /ko/". When the script
// computes the target (browser-language detection), the hreflang x-default page is used.
func fetchPage(ctx context.Context, u string, limit int64) (*fetchResult, error) {
	r, err := fetch(ctx, u, limit)
	for i := 0; err == nil && i < 3 && r.Status == 200 && len(r.Body) < 64<<10; i++ {
		body := string(r.Body)
		m := reMetaRefresh.FindStringSubmatch(body)
		if m == nil && len(visibleText(body)) < 400 {
			m = reJSRedirect.FindStringSubmatch(body)
			if m == nil {
				if t := computedRedirectTarget(body); t != "" {
					m = []string{"", t}
				}
			}
		}
		if m == nil {
			break
		}
		base, e1 := url.Parse(r.Final)
		next, e2 := url.Parse(strings.Trim(m[1], `"'`))
		if e1 != nil || e2 != nil {
			break
		}
		target := base.ResolveReference(next).String()
		if target == r.Final {
			break
		}
		start := r.Duration
		nr, nerr := fetch(ctx, target, limit)
		if nerr != nil {
			break
		}
		nr.Duration += start
		r = nr
	}
	return r, err
}

func fetch(ctx context.Context, u string, limit int64) (*fetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, limit))
	return &fetchResult{Status: res.StatusCode, Body: b, Final: res.Request.URL.String(),
		Duration: time.Since(start), TLS: res.TLS}, nil
}

func hostOf(site Site) string {
	u, err := url.Parse(site.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func pathOf(site Site) string {
	u, err := url.Parse(site.URL)
	if err != nil {
		return ""
	}
	return u.Path
}

func originOf(site Site) string {
	u, err := url.Parse(site.URL)
	if err != nil {
		return site.URL
	}
	return u.Scheme + "://" + u.Host
}

// registrable domain (simple: last two labels, or three for co.kr-style)
func registrableDomain(host string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	second := parts[len(parts)-2]
	tld := parts[len(parts)-1]
	if len(tld) == 2 && (second == "co" || second == "or" || second == "ne" || second == "go" || second == "ac" || second == "com" || second == "net" || second == "org" || second == "re") {
		return strings.Join(parts[len(parts)-3:], ".")
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

func checkHTTP(ctx context.Context, site Site) map[string]any {
	out := map[string]any{"url": site.URL}
	r, err := fetchPage(ctx, site.URL, 2<<20)
	if err != nil {
		out["ok"] = false
		out["error"] = shortErr(err)
		return out
	}
	out["status"] = r.Status
	out["ms"] = r.Duration.Milliseconds()
	out["final"] = r.Final
	out["ok"] = r.Status >= 200 && r.Status < 400
	out["bytes"] = len(r.Body)
	low := strings.ToLower(string(r.Body))
	out["noindex"] = regexp.MustCompile(`<meta[^>]+name=["']robots["'][^>]+noindex`).MatchString(low)
	out["adsenseCode"] = strings.Contains(low, "pagead2.googlesyndication.com") || strings.Contains(low, "adsbygoogle")
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		c := r.TLS.PeerCertificates[0]
		out["sslExpiry"] = c.NotAfter.Format(time.RFC3339)
		out["sslDays"] = int(time.Until(c.NotAfter).Hours() / 24)
		out["sslIssuer"] = c.Issuer.CommonName
	}
	return out
}

// checkSSLDirect handles sites where the HTTP check failed but TLS still answers.
func checkSSLDirect(ctx context.Context, host string) (time.Time, error) {
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: &tls.Config{ServerName: host}}
	c, err := d.DialContext(ctx, "tcp", host+":443")
	if err != nil {
		return time.Time{}, err
	}
	defer c.Close()
	st := c.(*tls.Conn).ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return time.Time{}, errors.New("no certificate")
	}
	return st.PeerCertificates[0].NotAfter, nil
}

func checkRobotsSitemap(ctx context.Context, site Site) (robots, sitemap map[string]any) {
	origin := originOf(site)
	robots = map[string]any{"url": origin + "/robots.txt"}
	var sitemaps []string
	r, err := fetch(ctx, origin+"/robots.txt", 512<<10)
	if err != nil {
		robots["ok"] = false
		robots["error"] = shortErr(err)
	} else {
		robots["status"] = r.Status
		robots["ok"] = r.Status == 200
		blockAll := false
		ua := ""
		sc := bufio.NewScanner(strings.NewReader(string(r.Body)))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if i := strings.Index(line, "#"); i >= 0 {
				line = strings.TrimSpace(line[:i])
			}
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			k = strings.ToLower(strings.TrimSpace(k))
			v = strings.TrimSpace(v)
			switch k {
			case "user-agent":
				ua = v
			case "disallow":
				if ua == "*" && v == "/" {
					blockAll = true
				}
			case "sitemap":
				sitemaps = append(sitemaps, v)
			}
		}
		robots["blocksAll"] = blockAll
		robots["sitemaps"] = len(sitemaps)
	}
	if len(sitemaps) == 0 {
		sitemaps = []string{origin + "/sitemap.xml"}
	}
	sitemap = map[string]any{"url": sitemaps[0]}
	total := 0
	for i, sm := range sitemaps {
		if i >= 5 {
			break
		}
		n, isIndex, status, err := countSitemap(ctx, sm, 0)
		if i == 0 {
			sitemap["status"] = status
			sitemap["index"] = isIndex
			if err != nil {
				sitemap["ok"] = false
				sitemap["error"] = shortErr(err)
				continue
			}
			sitemap["ok"] = status == 200
		}
		total += n
	}
	sitemap["urls"] = total
	sitemap["files"] = len(sitemaps)
	return
}

// countSitemap counts <url> entries; follows a sitemap index one level deep (max 50 children).
func countSitemap(ctx context.Context, u string, depth int) (count int, isIndex bool, status int, err error) {
	r, err := fetch(ctx, u, 50<<20)
	if err != nil {
		return 0, false, 0, err
	}
	if r.Status != 200 {
		return 0, false, r.Status, fmt.Errorf("HTTP %d", r.Status)
	}
	dec := xml.NewDecoder(strings.NewReader(string(r.Body)))
	dec.Strict = false
	var children []string
	inLoc, inSitemap := false, false
	for {
		tok, e := dec.Token()
		if e != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "url":
				count++
			case "sitemapindex":
				isIndex = true
			case "sitemap":
				inSitemap = true
			case "loc":
				inLoc = true
			}
		case xml.EndElement:
			if t.Name.Local == "loc" {
				inLoc = false
			} else if t.Name.Local == "sitemap" {
				inSitemap = false
			}
		case xml.CharData:
			if inLoc && inSitemap {
				children = append(children, strings.TrimSpace(string(t)))
			}
		}
	}
	if isIndex && depth == 0 {
		for i, c := range children {
			if i >= 50 {
				break
			}
			n, _, _, e := countSitemap(ctx, c, 1)
			if e == nil {
				count += n
			}
		}
	}
	return count, isIndex, 200, nil
}

// checkAdsTxt verifies the Google line for the given publisher ID.
func checkAdsTxt(ctx context.Context, site Site, pubID string) map[string]any {
	origin := originOf(site)
	out := map[string]any{"url": origin + "/ads.txt"}
	r, err := fetch(ctx, origin+"/ads.txt", 512<<10)
	if err != nil {
		out["ok"] = false
		out["error"] = shortErr(err)
		return out
	}
	out["status"] = r.Status
	if r.Status != 200 {
		out["ok"] = false
		out["found"] = false
		return out
	}
	out["found"] = true
	hasGoogle, hasPub := false, false
	for _, line := range strings.Split(string(r.Body), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(f[0]), "google.com") {
			hasGoogle = true
			if pubID != "" && strings.EqualFold(strings.TrimSpace(f[1]), pubID) &&
				strings.EqualFold(strings.TrimSpace(f[2]), "DIRECT") {
				hasPub = true
			}
		}
	}
	out["google"] = hasGoogle
	if pubID != "" {
		out["pubId"] = pubID
		out["pubMatch"] = hasPub
		out["ok"] = hasPub
	} else {
		out["ok"] = hasGoogle
	}
	return out
}

func checkDomainExpiry(ctx context.Context, domain string) map[string]any {
	out := map[string]any{"domain": domain}
	var r struct {
		Events []struct {
			Action string `json:"eventAction"`
			Date   string `json:"eventDate"`
		} `json:"events"`
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", rdapBase+domain, nil)
	req.Header.Set("Accept", "application/rdap+json")
	req.Header.Set("User-Agent", userAgent)
	res, err := httpClient.Do(req)
	if err != nil {
		out["error"] = shortErr(err)
		return out
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		out["error"] = fmt.Sprintf("RDAP HTTP %d", res.StatusCode)
		return out
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err := jsonUnmarshal(b, &r); err != nil {
		out["error"] = "RDAP parse error"
		return out
	}
	for _, e := range r.Events {
		if e.Action == "expiration" {
			t, err := time.Parse(time.RFC3339, e.Date)
			if err != nil {
				continue
			}
			out["expiry"] = t.Format(time.RFC3339)
			out["days"] = int(time.Until(t).Hours() / 24)
		}
	}
	if _, ok := out["expiry"]; !ok {
		out["error"] = "no_expiry"
	}
	return out
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
