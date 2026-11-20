// Fake Google / Bing / RDAP endpoints and two fake websites, for local testing.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

func j(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func auth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer fake-access" {
		w.WriteHeader(401)
		j(w, map[string]any{"error": map[string]any{"message": "bad token"}})
		return false
	}
	return true
}

func days(n int) []time.Time {
	var out []time.Time
	for i := n; i >= 1; i-- {
		out = append(out, time.Now().AddDate(0, 0, -i))
	}
	return out
}

// value with weekly wave; site "b" smaller; yesterday drops 50% for site a GA users
func val(base float64, d time.Time, site string, drop bool) float64 {
	v := base * (1 + 0.15*math.Sin(float64(d.YearDay())/1.3))
	if site == "b" {
		v *= 0.35
	}
	if drop && d.Format("2006-01-02") == time.Now().AddDate(0, 0, -1).Format("2006-01-02") {
		v *= 0.45
	}
	return math.Round(v)
}

func main() {
	mux := http.NewServeMux()
	// --- oauth ---
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		j(w, map[string]any{"access_token": "fake-access", "expires_in": 3600, "refresh_token": "fake-refresh",
			"scope": "openid https://www.googleapis.com/auth/analytics.readonly"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) { j(w, map[string]any{"email": "tester@example.com"}) })
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) { j(w, map[string]any{}) })
	// --- GA ---
	mux.HandleFunc("/gaadmin/accountSummaries", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		j(w, map[string]any{"accountSummaries": []any{map[string]any{"displayName": "Toolbox", "propertySummaries": []any{
			map[string]any{"property": "properties/111", "displayName": "127.0.0.1 KO"},
			map[string]any{"property": "properties/222", "displayName": "localhost EN"}}}}})
	})
	mux.HandleFunc("/gadata/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		site := "a"
		if strings.Contains(r.URL.Path, "222") {
			site = "b"
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"pagePath"`) && strings.Contains(string(raw), `"dimensions":[{"name":"pagePath"}]`) {
			prev := strings.Contains(string(raw), time.Now().AddDate(0, 0, -40).Format("2006-01")) // crude
			var prow []any
			for i := 0; i < 12; i++ {
				v := float64(900 - i*70)
				if i == 4 && !prev {
					v *= 2.5
				}
				prow = append(prow, map[string]any{"dimensionValues": []any{map[string]any{"value": fmt.Sprintf("/ko/p%d", i)}},
					"metricValues": []any{map[string]any{"value": fmt.Sprint(v)}, map[string]any{"value": fmt.Sprint(v * 0.6)}}})
			}
			j(w, map[string]any{"rows": prow})
			return
		}
		var rows []any
		for _, d := range days(90) {
			rows = append(rows, map[string]any{"dimensionValues": []any{map[string]any{"value": d.Format("20060102")}},
				"metricValues": []any{map[string]any{"value": fmt.Sprint(val(420, d, site, true))}, map[string]any{"value": fmt.Sprint(val(510, d, site, true))}, map[string]any{"value": fmt.Sprint(val(1300, d, site, false))}}})
		}
		j(w, map[string]any{"rows": rows})
	})
	// --- GSC ---
	mux.HandleFunc("/gsc/sites", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		j(w, map[string]any{"siteEntry": []any{
			map[string]any{"siteUrl": "http://127.0.0.1:9911/", "permissionLevel": "siteOwner"},
			map[string]any{"siteUrl": "http://localhost:9911/", "permissionLevel": "siteOwner"}}})
	})
	mux.HandleFunc("/gsc/sites/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		site := "a"
		if strings.Contains(r.URL.Path, "localhost") || strings.Contains(r.URL.RawPath, "localhost") {
			site = "b"
		}
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/sitemaps/") {
			log.Printf("fake GSC: sitemap submitted %s", r.URL.Path)
			w.WriteHeader(204)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/sitemaps") {
			j(w, map[string]any{"sitemap": []any{map[string]any{"path": "http://127.0.0.1:9911/sitemap.xml", "lastDownloaded": time.Now().Add(-30 * time.Hour).Format(time.RFC3339),
				"errors": "0", "warnings": "1", "contents": []any{map[string]any{"type": "web", "submitted": "42", "indexed": "0"}}}}})
			return
		}
		var body struct {
			StartDate, EndDate string
			Dimensions         []string
		}
		json.NewDecoder(r.Body).Decode(&body)
		queries := []string{"pdf to word", "pdf 변환", "merge pdf", "pdf compress", "ocr pdf", "pdf 합치기"}
		var rows []any
		dims := strings.Join(body.Dimensions, ",")
		switch dims {
		case "date":
			for _, d := range days(93) {
				rows = append(rows, map[string]any{"keys": []string{d.Format("2006-01-02")}, "clicks": val(85, d, site, false), "impressions": val(2400, d, site, false), "ctr": 0.035, "position": 12.4})
			}
		case "query":
			prev := body.StartDate < time.Now().AddDate(0, 0, -40).Format("2006-01-02")
			for i, q := range queries {
				if prev && i == 5 {
					continue // "new" query
				}
				c := float64(200 - i*30)
				if prev {
					c *= 0.8
				}
				rows = append(rows, map[string]any{"keys": []string{q}, "clicks": c, "impressions": c * 30, "ctr": 1.0 / 30, "position": 5.5 + float64(i)})
			}
		case "page":
			for i := 0; i < 10; i++ {
				c := float64(120 - i*10)
				rows = append(rows, map[string]any{"keys": []string{fmt.Sprintf("http://127.0.0.1:9911/ko/p%d", i)}, "clicks": c, "impressions": c * 25, "ctr": 0.04, "position": 4.0 + float64(i)})
			}
		case "date,query":
			for _, d := range days(30) {
				for i, q := range queries[:3] {
					rows = append(rows, map[string]any{"keys": []string{d.Format("2006-01-02"), q}, "clicks": val(float64(30-i*8), d, site, false), "impressions": val(float64(900-i*200), d, site, false), "ctr": 0.03, "position": 6 + float64(i)})
				}
			}
		}
		j(w, map[string]any{"rows": rows})
	})
	mux.HandleFunc("/inspect", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var b struct{ InspectionUrl string }
		json.NewDecoder(r.Body).Decode(&b)
		verdict, cov := "PASS", "Submitted and indexed"
		if strings.Contains(b.InspectionUrl, "/new") || strings.HasSuffix(b.InspectionUrl, "/p3") || strings.HasSuffix(b.InspectionUrl, "/p7") {
			verdict, cov = "NEUTRAL", "Discovered - currently not indexed"
		}
		j(w, map[string]any{"inspectionResult": map[string]any{"indexStatusResult": map[string]any{"verdict": verdict, "coverageState": cov,
			"lastCrawlTime": time.Now().Add(-50 * time.Hour).Format(time.RFC3339)}}})
	})
	// --- AdSense ---
	mux.HandleFunc("/adsense/accounts", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		j(w, map[string]any{"accounts": []any{map[string]any{"name": "accounts/pub-1234567890123456", "displayName": "Toolbox", "state": "READY", "timeZone": map[string]any{"id": "Asia/Seoul"}}}})
	})
	mux.HandleFunc("/adsense/accounts/pub-1234567890123456/sites", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		j(w, map[string]any{"sites": []any{map[string]any{"domain": "127.0.0.1", "state": "READY", "autoAdsEnabled": true}, map[string]any{"domain": "localhost", "state": "GETTING_READY"}}})
	})
	mux.HandleFunc("/adsense/accounts/pub-1234567890123456/alerts", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		j(w, map[string]any{"alerts": []any{map[string]any{"severity": "WARNING", "message": "Earnings at risk – one or more ad units need ads.txt", "type": "ADS_TXT_UNAUTHORIZED"}}})
	})
	mux.HandleFunc("/adsense/accounts/pub-1234567890123456/policyIssues", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		j(w, map[string]any{})
	})
	mux.HandleFunc("/adsense/accounts/pub-1234567890123456/reports:generate", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		hdr := []any{map[string]any{"name": "DATE"}, map[string]any{"name": "DOMAIN_NAME"}, map[string]any{"name": "ESTIMATED_EARNINGS", "currencyCode": "USD"},
			map[string]any{"name": "PAGE_VIEWS"}, map[string]any{"name": "PAGE_VIEWS_RPM", "currencyCode": "USD"}, map[string]any{"name": "IMPRESSIONS"}, map[string]any{"name": "CLICKS"}}
		var rows []any
		for _, d := range days(90) {
			for _, dom := range []string{"127.0.0.1", "localhost"} {
				site := "a"
				if dom == "localhost" {
					site = "b"
				}
				pv := val(1200, d, site, false)
				e := pv * 0.0042
				cell := func(s string) map[string]any { return map[string]any{"value": s} }
				rows = append(rows, map[string]any{"cells": []any{cell(d.Format("2006-01-02")), cell(dom), cell(fmt.Sprintf("%.2f", e)), cell(fmt.Sprint(pv)),
					cell(fmt.Sprintf("%.2f", e/pv*1000)), cell(fmt.Sprint(pv * 2)), cell(fmt.Sprint(math.Round(pv / 90)))}})
			}
		}
		j(w, map[string]any{"headers": hdr, "rows": rows})
	})
	// --- Bing ---
	mux.HandleFunc("/bing/GetUserSites", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"d": []any{map[string]any{"Url": "http://127.0.0.1:9911/", "IsVerified": true}, map[string]any{"Url": "http://localhost:9911/", "IsVerified": true}}})
	})
	mux.HandleFunc("/bing/GetRankAndTrafficStats", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "bingkey" {
			w.WriteHeader(401)
			io.WriteString(w, `{"ErrorCode":3,"Message":"InvalidApiKey"}`)
			return
		}
		site := "a"
		if strings.Contains(r.URL.Query().Get("siteUrl"), "localhost") {
			site = "b"
		}
		var d []any
		for _, t := range days(90) {
			d = append(d, map[string]any{"Clicks": val(14, t, site, false), "Impressions": val(300, t, site, false), "Date": fmt.Sprintf("/Date(%d-0800)/", time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.FixedZone("", -8*3600)).UnixMilli())})
		}
		j(w, map[string]any{"d": d})
	})
	mux.HandleFunc("/indexnow", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		log.Printf("indexnow: %s", b)
		w.WriteHeader(202)
	})
	mux.HandleFunc("/psi", func(w http.ResponseWriter, r *http.Request) {
		sc := 0.91
		if r.URL.Query().Get("strategy") == "mobile" {
			sc = 0.62
		}
		j(w, map[string]any{
			"loadingExperience": map[string]any{"overall_category": "AVERAGE", "metrics": map[string]any{
				"LARGEST_CONTENTFUL_PAINT_MS": map[string]any{"percentile": 2900}, "INTERACTION_TO_NEXT_PAINT": map[string]any{"percentile": 180},
				"CUMULATIVE_LAYOUT_SHIFT_SCORE": map[string]any{"percentile": 4}}},
			"lighthouseResult": map[string]any{"categories": map[string]any{"performance": map[string]any{"score": sc}},
				"audits": map[string]any{"largest-contentful-paint": map[string]any{"numericValue": 3400.0}, "cumulative-layout-shift": map[string]any{"numericValue": 0.05},
					"total-blocking-time": map[string]any{"numericValue": 320.0}, "first-contentful-paint": map[string]any{"numericValue": 1800.0}}}})
	})
	// --- RDAP ---
	mux.HandleFunc("/rdap/", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"events": []any{map[string]any{"eventAction": "registration", "eventDate": "2024-01-01T00:00:00Z"},
			map[string]any{"eventAction": "expiration", "eventDate": time.Now().AddDate(0, 0, 21).UTC().Format(time.RFC3339)}}})
	})
	go func() { log.Fatal(http.ListenAndServe("127.0.0.1:9910", mux)) }()

	// --- fake websites (127.0.0.1:9911 and localhost:9911 are the same server) ---
	site := http.NewServeMux()
	site.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "User-agent: *\nDisallow: /admin\nSitemap: http://%s/sitemap.xml\n", r.Host)
	})
	site.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><sitemap><loc>http://%s/sitemap-1.xml</loc></sitemap></sitemapindex>`, r.Host)
	})
	site.HandleFunc("/sitemap-1.xml", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
		for i := 0; i < 37; i++ {
			fmt.Fprintf(w, "<url><loc>http://%s/ko/p%d</loc><lastmod>2026-11-%02d</lastmod></url>", r.Host, i, 1+i%9)
		}
		fmt.Fprintf(w, "<url><loc>http://%s/ko/about/</loc></url><url><loc>http://%s/ko/privacy/</loc></url><url><loc>http://%s/ko/contact/</loc></url><url><loc>http://%s/ko/archive/</loc></url>", r.Host, r.Host, r.Host, r.Host)
		io.WriteString(w, "</urlset>")
	})
	site.HandleFunc("/ads.txt", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "localhost") {
			io.WriteString(w, "google.com, pub-0000000000000000, DIRECT, f08c47fec0942fa0\n")
			return
		}
		io.WriteString(w, "google.com, pub-1234567890123456, DIRECT, f08c47fec0942fa0\n")
	})
	site.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if k := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".txt"); len(k) == 32 && strings.HasSuffix(r.URL.Path, ".txt") {
			io.WriteString(w, k) // IndexNow key file
			return
		}
		if r.URL.Path == "/" && strings.HasPrefix(r.Host, "localhost") { // like toolboxnotes.com: script picks the language at runtime
			io.WriteString(w, `<!doctype html><html lang="en"><head><title>Toolbox Notes</title><link rel="alternate" hreflang="en" href="/en/"><link rel="alternate" hreflang="ko" href="/ko/"><link rel="alternate" hreflang="x-default" href="/ko/"><script>(function(){var lang="ko";try{lang=localStorage.getItem("lang")||lang}catch(e){};location.replace("/" + lang + "/");})();</script></head><body><a href="/en/">English</a> <a href="/ko/">한국어</a></body></html>`)
			return
		}
		if r.URL.Path == "/" { // like an Astro static site: page-level redirect to the language folder
			io.WriteString(w, `<!doctype html><title>Redirecting to: /ko/</title><meta http-equiv="refresh" content="0;url=/ko/"><meta name="robots" content="noindex"><link rel="canonical" href="/ko/">`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/en") {
			time.Sleep(50 * time.Millisecond)
		}
		head := `<html lang="ko"><head><title>Toolbox Notes</title><meta name="viewport" content="width=device-width"><meta name="description" content="d"><script async src="https://pagead2.googlesyndication.com/pagead/js/adsbygoogle.js?client=ca-pub-1234567890123456"></script></head><body>`
		nav := `<nav><a href="/ko/">홈</a><a href="/ko/about/">소개</a><a href="/ko/privacy/">개인정보처리방침</a><a href="/p1">글1</a><a href="/missing">깨진 링크</a></nav>`
		if strings.HasPrefix(r.URL.Path, "/missing") || strings.HasSuffix(r.URL.Path, "/p36") {
			w.WriteHeader(404)
			io.WriteString(w, "not found")
			return
		}
		body := strings.Repeat("이 글은 PDF 변환 방법을 자세히 설명합니다. ", 60)
		switch {
		case strings.HasSuffix(r.URL.Path, "/p3"):
			body = "짧은 글"
		case strings.HasSuffix(r.URL.Path, "/p5"):
			body = "이 페이지는 준비 중입니다"
		case strings.HasPrefix(r.URL.Path, "/ko/privacy"):
			body = strings.Repeat("개인정보 쿠키 Google 광고 ", 40)
		}
		io.WriteString(w, head+nav+`<main><h1>`+r.URL.Path+`</h1><p>`+body+`</p><a href="/p2">다음</a></main><footer>footer</footer></body></html>`)
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:9911", site))
}
