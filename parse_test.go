package main

import (
	"fmt"
	"testing"
	"time"
)

func TestTrendsCSV(t *testing.T) {
	csv := "\xef\xbb\xbfCategory: All categories\n\nDay,pdf: (South Korea),word: (South Korea)\n2026-11-01,50,<1\n2026-11-02,100,3\n"
	s, err := parseTrendsCSV([]byte(csv), "x.csv")
	if err != nil || len(s) != 2 || s[0].Term != "pdf" || s[0].Geo != "South Korea" || s[1].Points["2026-11-01"] != 0.5 {
		t.Fatalf("%v %+v", err, s)
	}
	// newer export without category line, monthly dates
	s, err = parseTrendsCSV([]byte("Month,\"a: (Worldwide)\"\n2025-01,10\n2025-02,20\n"), "m.csv")
	if err != nil || s[0].Points["2025-02-01"] != 20 {
		t.Fatalf("%v %+v", err, s)
	}
	if _, err := parseTrendsCSV([]byte("hello,world\n"), "bad"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBingDate(t *testing.T) {
	tm, ok := parseBingDate("/Date(1762675200000-0800)/")
	if !ok || tm.Format("2006-01-02") != "2025-11-09" {
		t.Fatal(tm, ok)
	}
}

func TestRegistrable(t *testing.T) {
	for in, want := range map[string]string{"toolboxnotes.com": "toolboxnotes.com", "www.toolboxnotes.com": "toolboxnotes.com", "blog.example.co.kr": "example.co.kr", "a.b.example.com": "example.com"} {
		if g := registrableDomain(in); g != want {
			t.Errorf("%s: %s", in, g)
		}
	}
}

func TestMetricAlerts(t *testing.T) {
	m := map[string]map[string]float64{}
	for i, d := range []string{"2026-11-01", "2026-11-02", "2026-11-03", "2026-11-04", "2026-11-05", "2026-11-06", "2026-11-07", "2026-11-08"} {
		v := 100.0
		if i == 7 {
			v = 40
		}
		m[d] = map[string]float64{"ga_users": v, "gsc_clicks": 5}
	}
	m["2026-11-09"] = map[string]float64{"ga_users": 1} // today, partial: ignored
	a := metricAlerts("s", m, 30, "2026-11-09")
	if len(a) != 1 || a[0].Code != "metric_drop" || a[0].Level != "error" {
		t.Fatalf("%+v", a)
	}
}

func TestPageFilter(t *testing.T) {
	b := map[string]any{}
	gscPageFilter(b, "https://toolboxnotes.com/ko/")
	f := b["dimensionFilterGroups"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)
	if f["expression"] != `^https?://(www\.)?toolboxnotes\.com/ko/` {
		t.Fatal(f["expression"])
	}
	b = map[string]any{}
	gscPageFilter(b, "https://toolboxnotes.com/")
	if _, ok := b["dimensionFilterGroups"]; ok {
		t.Fatal("root should not filter")
	}
}

func TestKwAlerts(t *testing.T) {
	ks := KwStore{"pdf": {}}
	for i := 1; i <= 10; i++ {
		p := 4.0
		if i > 7 {
			p = 9
		}
		ks["pdf"][fmt.Sprintf("2026-11-%02d", i)] = KwPoint{P: p, I: 20}
	}
	a := kwAlerts(Site{ID: "s", Tracked: []string{"pdf"}}, ks)
	if len(a) != 1 || a[0].Code != "kw_drop" {
		t.Fatalf("%+v", a)
	}
}

func TestContentWords(t *testing.T) {
	if n := contentWords("PDF 변환 방법을 설명합니다"); n != 4 {
		t.Fatal(n)
	}
	if n := contentWords("如何把PDF转换成Word文档"); n < 4 || n > 8 {
		t.Fatal(n)
	}
}

func TestExtractLinks(t *testing.T) {
	l := extractLinks(`<a href="/about/">About</a><a href='https://x.com/a#top'>x</a><a href="mailto:a@b">m</a><a href="#s">s</a>`, "https://site.com/ko/")
	if len(l) != 2 || l[0].URL != "https://site.com/about/" || l[1].URL != "https://x.com/a" || l[0].Text != "About" {
		t.Fatalf("%+v", l)
	}
}

func TestXLSX(t *testing.T) {
	b, err := buildXLSX([]xlsxSheet{{Name: "a/b", Rows: [][]any{{"x", 1.0}, {nil, "<&>"}}}})
	if err != nil || len(b) < 500 {
		t.Fatal(err, len(b))
	}
}

func TestComputedRedirectTarget(t *testing.T) {
	root := `<!doctype html><html lang="en"><head>
<link rel="alternate" hreflang="en" href="https://toolboxnotes.com/en/">
<link rel="alternate" hreflang="ko" href="https://toolboxnotes.com/ko/">
<link rel="alternate" hreflang="x-default" href="https://toolboxnotes.com/en/">
<script>(function(){var lang="en";location.replace("/" + lang + "/");})();</script>
</head><body><a href="/en/">English</a> <a href="/ko/">한국어</a></body></html>`
	if got := computedRedirectTarget(root); got != "https://toolboxnotes.com/en/" {
		t.Fatalf("x-default: got %q", got)
	}
	noDefault := `<link hreflang="ko" rel="alternate" href="/ko/"><script>window.location.href = base + lang;</script>`
	if got := computedRedirectTarget(noDefault); got != "/ko/" {
		t.Fatalf("first alternate: got %q", got)
	}
	if got := computedRedirectTarget(`<link rel="alternate" hreflang="ko" href="/ko/"><p>no script</p>`); got != "" {
		t.Fatalf("no script: got %q", got)
	}
	if got := computedRedirectTarget(`<script>if (location == x) {}</script><link rel="alternate" hreflang="ko" href="/ko/">`); got != "" {
		t.Fatalf("comparison is not a redirect: got %q", got)
	}
}

func TestListingPatterns(t *testing.T) {
	for _, p := range []string{"/ko/3", "/2", "/en/12"} {
		if !rePagination.MatchString(p) {
			t.Errorf("pagination %q not matched", p)
		}
	}
	for _, p := range []string{"/ko/posts/amazon-seller-3-sales-data", "/2026/11/my-post"} {
		if rePagination.MatchString(p) {
			t.Errorf("article %q matched as pagination", p)
		}
	}
	for _, p := range []string{"/ko", "/en", "/zh-cn"} {
		if !reLangRoot.MatchString(p) {
			t.Errorf("lang root %q not matched", p)
		}
	}
	if reLangRoot.MatchString("/about") {
		t.Error("/about matched as lang root")
	}
}

func TestExtractLinksSkipsCloudflareEmailProtection(t *testing.T) {
	h := `<a href="/cdn-cgi/l/email-protection#abc123">email</a><a href="/ko/contact/">문의</a>`
	got := extractLinks(h, "https://toolboxnotes.com/ko/privacy/")
	if len(got) != 1 || got[0].URL != "https://toolboxnotes.com/ko/contact/" {
		t.Fatalf("got %+v", got)
	}
}

func TestPickRequiredPageSkipsTagLinks(t *testing.T) {
	links := []link{
		{URL: "https://toolboxnotes.com/en/archive/?tag=privacy", Text: "privacy"},
		{URL: "https://toolboxnotes.com/en/tags/privacy/", Text: "Privacy"},
		{URL: "https://toolboxnotes.com/en/legal/", Text: "Privacy Policy"},
		{URL: "https://toolboxnotes.com/en/privacy/", Text: "Privacy"},
	}
	if got := pickRequiredPage(links, rePrivacy, "https://toolboxnotes.com/en/"); got != "https://toolboxnotes.com/en/privacy/" {
		t.Fatalf("got %q", got)
	}
	if got := pickRequiredPage(links[:3], rePrivacy, "https://toolboxnotes.com/en/"); got != "https://toolboxnotes.com/en/legal/" {
		t.Fatalf("text fallback: got %q", got)
	}
}

func TestMetricAlertsIgnoreUnfinishedSearchConsoleDays(t *testing.T) {
	m := map[string]map[string]float64{}
	for i, v := range []float64{50, 55, 60, 52, 58, 54, 56, 57, 60, 10} { // last day partial
		d := time.Date(2026, 11, 6+i, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		m[d] = map[string]float64{"gsc_impr": v, "ga_users": 20}
	}
	m["2026-11-15"]["ga_users"] = 2 // a real GA drop on the last finished day
	got := metricAlerts("s", m, 30, "2026-11-16")
	for _, a := range got {
		if a.Code == "metric_drop" && a.Params["metric"] == "gsc_impr" {
			t.Fatalf("partial Search Console day should not alert: %+v", a)
		}
	}
	found := false
	for _, a := range got {
		if a.Code == "metric_drop" && a.Params["metric"] == "ga_users" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GA drop should still alert: %+v", got)
	}
	if dateBefore("2026-11-16", 3) != "2026-11-13" {
		t.Fatal("dateBefore")
	}
}
