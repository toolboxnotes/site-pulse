package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	gaAdminBase = "https://analyticsadmin.googleapis.com/v1beta"
	gaDataBase  = "https://analyticsdata.googleapis.com/v1beta"
	gscBase     = "https://www.googleapis.com/webmasters/v3"
	gscInspect  = "https://searchconsole.googleapis.com/v1/urlInspection/index:inspect"
	adsenseBase = "https://adsense.googleapis.com/v2"
)

// flexible number: Google sometimes encodes int64 as string
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	*n = num(f)
	return nil
}

// ---------- GA4 ----------

type GAProperty struct {
	ID      string `json:"id"` // properties/123
	Name    string `json:"name"`
	Account string `json:"account"`
}

func gaListProperties(ctx context.Context, tok string) ([]GAProperty, error) {
	var out []GAProperty
	pageToken := ""
	for i := 0; i < 20; i++ {
		u := gaAdminBase + "/accountSummaries?pageSize=200"
		if pageToken != "" {
			u += "&pageToken=" + url.QueryEscape(pageToken)
		}
		var r struct {
			AccountSummaries []struct {
				DisplayName       string `json:"displayName"`
				PropertySummaries []struct {
					Property    string `json:"property"`
					DisplayName string `json:"displayName"`
				} `json:"propertySummaries"`
			} `json:"accountSummaries"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := getJSON(ctx, tok, u, &r); err != nil {
			return nil, err
		}
		for _, a := range r.AccountSummaries {
			for _, p := range a.PropertySummaries {
				out = append(out, GAProperty{ID: p.Property, Name: p.DisplayName, Account: a.DisplayName})
			}
		}
		if r.NextPageToken == "" {
			break
		}
		pageToken = r.NextPageToken
	}
	return out, nil
}

// gaDaily returns date -> {ga_users, ga_sessions, ga_views}
func gaDaily(ctx context.Context, tok, property, start, end, pathPrefix string) (map[string]map[string]float64, error) {
	body := map[string]any{
		"dateRanges": []map[string]string{{"startDate": start, "endDate": end}},
		"dimensions": []map[string]string{{"name": "date"}},
		"metrics": []map[string]string{
			{"name": "activeUsers"}, {"name": "sessions"}, {"name": "screenPageViews"},
		},
		"limit": 1000,
	}
	if pathPrefix != "" && pathPrefix != "/" {
		body["dimensionFilter"] = map[string]any{"filter": map[string]any{"fieldName": "pagePath",
			"stringFilter": map[string]any{"matchType": "BEGINS_WITH", "value": pathPrefix}}}
	}
	var r struct {
		Rows []struct {
			DimensionValues []struct{ Value string } `json:"dimensionValues"`
			MetricValues    []struct{ Value num }    `json:"metricValues"`
		} `json:"rows"`
	}
	if err := doJSON(ctx, tok, "POST", gaDataBase+"/"+property+":runReport", body, &r); err != nil {
		return nil, err
	}
	out := map[string]map[string]float64{}
	keys := []string{"ga_users", "ga_sessions", "ga_views"}
	for _, row := range r.Rows {
		if len(row.DimensionValues) == 0 {
			continue
		}
		d := row.DimensionValues[0].Value // 20261109
		if len(d) != 8 {
			continue
		}
		date := d[:4] + "-" + d[4:6] + "-" + d[6:]
		m := map[string]float64{}
		for i, k := range keys {
			if i < len(row.MetricValues) {
				m[k] = float64(row.MetricValues[i].Value)
			}
		}
		out[date] = m
	}
	return out, nil
}

// ---------- Search Console ----------

type GSCSite struct {
	URL        string `json:"url"`
	Permission string `json:"permission"`
}

func gscListSites(ctx context.Context, tok string) ([]GSCSite, error) {
	var r struct {
		SiteEntry []struct {
			SiteURL         string `json:"siteUrl"`
			PermissionLevel string `json:"permissionLevel"`
		} `json:"siteEntry"`
	}
	if err := getJSON(ctx, tok, gscBase+"/sites", &r); err != nil {
		return nil, err
	}
	var out []GSCSite
	for _, s := range r.SiteEntry {
		out = append(out, GSCSite{URL: s.SiteURL, Permission: s.PermissionLevel})
	}
	return out, nil
}

type GSCRow struct {
	Keys        []string `json:"keys"`
	Clicks      float64  `json:"clicks"`
	Impressions float64  `json:"impressions"`
	CTR         float64  `json:"ctr"`
	Position    float64  `json:"position"`
}

func gscQuery(ctx context.Context, tok, site string, body map[string]any) ([]GSCRow, error) {
	if _, ok := body["dataState"]; !ok {
		body["dataState"] = "all"
	}
	var r struct {
		Rows []GSCRow `json:"rows"`
	}
	u := gscBase + "/sites/" + url.PathEscape(site) + "/searchAnalytics/query"
	if err := doJSON(ctx, tok, "POST", u, body, &r); err != nil {
		return nil, err
	}
	return r.Rows, nil
}

// gscPageFilter limits Search Console rows to one section of a site (e.g. https://example.com/ko/).
func gscPageFilter(body map[string]any, siteURL string) {
	u, err := url.Parse(siteURL)
	if err != nil || u.Path == "" || u.Path == "/" {
		return
	}
	host := strings.TrimPrefix(u.Hostname(), "www.")
	expr := "^https?://(www\\.)?" + regexp.QuoteMeta(host) + regexp.QuoteMeta(u.Path)
	f := map[string]any{"dimension": "page", "operator": "includingRegex", "expression": expr}
	if g, ok := body["dimensionFilterGroups"].([]any); ok && len(g) > 0 {
		grp := g[0].(map[string]any)
		grp["filters"] = append(grp["filters"].([]any), f)
		return
	}
	body["dimensionFilterGroups"] = []any{map[string]any{"filters": []any{f}}}
}

func gscDaily(ctx context.Context, tok, site, start, end, siteURL string) (map[string]map[string]float64, error) {
	body := map[string]any{"startDate": start, "endDate": end, "dimensions": []string{"date"}, "rowLimit": 1000}
	gscPageFilter(body, siteURL)
	rows, err := gscQuery(ctx, tok, site, body)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]float64{}
	for _, r := range rows {
		if len(r.Keys) == 0 {
			continue
		}
		out[r.Keys[0]] = map[string]float64{
			"gsc_clicks": r.Clicks, "gsc_impr": r.Impressions,
			"gsc_ctr": r.CTR * 100, "gsc_pos": r.Position,
		}
	}
	return out, nil
}

type GSCSitemap struct {
	Path           string  `json:"path"`
	LastSubmitted  string  `json:"lastSubmitted"`
	LastDownloaded string  `json:"lastDownloaded"`
	IsPending      bool    `json:"isPending"`
	Errors         num     `json:"errors"`
	Warnings       num     `json:"warnings"`
	Submitted      float64 `json:"submitted"`
	Indexed        float64 `json:"indexed"`
}

func gscSitemaps(ctx context.Context, tok, site string) ([]GSCSitemap, error) {
	var r struct {
		Sitemap []struct {
			Path           string `json:"path"`
			LastSubmitted  string `json:"lastSubmitted"`
			LastDownloaded string `json:"lastDownloaded"`
			IsPending      bool   `json:"isPending"`
			Errors         num    `json:"errors"`
			Warnings       num    `json:"warnings"`
			Contents       []struct {
				Type      string `json:"type"`
				Submitted num    `json:"submitted"`
				Indexed   num    `json:"indexed"`
			} `json:"contents"`
		} `json:"sitemap"`
	}
	if err := getJSON(ctx, tok, gscBase+"/sites/"+url.PathEscape(site)+"/sitemaps", &r); err != nil {
		return nil, err
	}
	var out []GSCSitemap
	for _, s := range r.Sitemap {
		m := GSCSitemap{Path: s.Path, LastSubmitted: s.LastSubmitted, LastDownloaded: s.LastDownloaded,
			IsPending: s.IsPending, Errors: s.Errors, Warnings: s.Warnings}
		for _, c := range s.Contents {
			m.Submitted += float64(c.Submitted)
			m.Indexed += float64(c.Indexed)
		}
		out = append(out, m)
	}
	return out, nil
}

type Inspection struct {
	URL           string `json:"url"`
	Verdict       string `json:"verdict"`
	Coverage      string `json:"coverage"`
	LastCrawl     string `json:"lastCrawl"`
	IndexingState string `json:"indexingState"`
	PageFetch     string `json:"pageFetch"`
	Error         string `json:"error,omitempty"`
}

func gscInspectURL(ctx context.Context, tok, site, pageURL string) Inspection {
	var r struct {
		InspectionResult struct {
			IndexStatusResult struct {
				Verdict        string `json:"verdict"`
				CoverageState  string `json:"coverageState"`
				LastCrawlTime  string `json:"lastCrawlTime"`
				IndexingState  string `json:"indexingState"`
				PageFetchState string `json:"pageFetchState"`
			} `json:"indexStatusResult"`
		} `json:"inspectionResult"`
	}
	in := Inspection{URL: pageURL}
	err := doJSON(ctx, tok, "POST", gscInspect, map[string]string{
		"inspectionUrl": pageURL, "siteUrl": site, "languageCode": "en-US",
	}, &r)
	if err != nil {
		in.Error = err.Error()
		return in
	}
	s := r.InspectionResult.IndexStatusResult
	in.Verdict, in.Coverage, in.LastCrawl = s.Verdict, s.CoverageState, s.LastCrawlTime
	in.IndexingState, in.PageFetch = s.IndexingState, s.PageFetchState
	return in
}

// ---------- AdSense ----------

type AdsAccount struct {
	Name         string   `json:"name"` // accounts/pub-123
	DisplayName  string   `json:"displayName"`
	State        string   `json:"state"`
	PendingTasks []string `json:"pendingTasks"`
	TimeZone     struct {
		ID string `json:"id"`
	} `json:"timeZone"`
}

func (a AdsAccount) PubID() string { return strings.TrimPrefix(a.Name, "accounts/") }

func adsAccounts(ctx context.Context, tok string) ([]AdsAccount, error) {
	var r struct {
		Accounts []AdsAccount `json:"accounts"`
	}
	if err := getJSON(ctx, tok, adsenseBase+"/accounts", &r); err != nil {
		return nil, err
	}
	return r.Accounts, nil
}

type AdsSite struct {
	Domain         string `json:"domain"`
	State          string `json:"state"`
	AutoAdsEnabled bool   `json:"autoAdsEnabled"`
}

func adsSites(ctx context.Context, tok, account string) ([]AdsSite, error) {
	var out []AdsSite
	page := ""
	for i := 0; i < 20; i++ {
		u := adsenseBase + "/" + account + "/sites?pageSize=500"
		if page != "" {
			u += "&pageToken=" + url.QueryEscape(page)
		}
		var r struct {
			Sites         []AdsSite `json:"sites"`
			NextPageToken string    `json:"nextPageToken"`
		}
		if err := getJSON(ctx, tok, u, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Sites...)
		if r.NextPageToken == "" {
			break
		}
		page = r.NextPageToken
	}
	return out, nil
}

type AdsAlert struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Type     string `json:"type"`
}

func adsAlerts(ctx context.Context, tok, account, lang string) ([]AdsAlert, error) {
	var r struct {
		Alerts []AdsAlert `json:"alerts"`
	}
	u := adsenseBase + "/" + account + "/alerts"
	if lang != "" {
		u += "?languageCode=" + url.QueryEscape(lang)
	}
	if err := getJSON(ctx, tok, u, &r); err != nil {
		return nil, err
	}
	return r.Alerts, nil
}

type AdsPolicyIssue struct {
	Site         string `json:"site"`
	SiteSection  string `json:"siteSection"`
	URI          string `json:"uri"`
	Action       string `json:"action"`
	EntityType   string `json:"entityType"`
	PolicyTopics []struct {
		Topic   string `json:"topic"`
		MustFix bool   `json:"mustFix"`
	} `json:"policyTopics"`
	FirstDetectedDate     map[string]int `json:"firstDetectedDate"`
	LastDetectedDate      map[string]int `json:"lastDetectedDate"`
	WarningEscalationDate map[string]int `json:"warningEscalationDate"`
}

func adsPolicyIssues(ctx context.Context, tok, account string) ([]AdsPolicyIssue, error) {
	var out []AdsPolicyIssue
	page := ""
	for i := 0; i < 20; i++ {
		u := adsenseBase + "/" + account + "/policyIssues?pageSize=100"
		if page != "" {
			u += "&pageToken=" + url.QueryEscape(page)
		}
		var r struct {
			PolicyIssues  []AdsPolicyIssue `json:"policyIssues"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := getJSON(ctx, tok, u, &r); err != nil {
			return nil, err
		}
		out = append(out, r.PolicyIssues...)
		if r.NextPageToken == "" {
			break
		}
		page = r.NextPageToken
	}
	return out, nil
}

// adsDaily returns domain -> date -> metrics, plus the currency code.
func adsDaily(ctx context.Context, tok, account string, start, end time.Time) (map[string]map[string]map[string]float64, string, error) {
	q := url.Values{}
	q.Set("dateRange", "CUSTOM")
	q.Set("startDate.year", strconv.Itoa(start.Year()))
	q.Set("startDate.month", strconv.Itoa(int(start.Month())))
	q.Set("startDate.day", strconv.Itoa(start.Day()))
	q.Set("endDate.year", strconv.Itoa(end.Year()))
	q.Set("endDate.month", strconv.Itoa(int(end.Month())))
	q.Set("endDate.day", strconv.Itoa(end.Day()))
	q.Add("dimensions", "DATE")
	q.Add("dimensions", "DOMAIN_NAME")
	metrics := []string{"ESTIMATED_EARNINGS", "PAGE_VIEWS", "PAGE_VIEWS_RPM", "IMPRESSIONS", "CLICKS"}
	keys := []string{"ads_earnings", "ads_pageviews", "ads_rpm", "ads_impressions", "ads_clicks"}
	for _, m := range metrics {
		q.Add("metrics", m)
	}
	var r struct {
		Headers []struct {
			Name         string `json:"name"`
			CurrencyCode string `json:"currencyCode"`
		} `json:"headers"`
		Rows []struct {
			Cells []struct {
				Value string `json:"value"`
			} `json:"cells"`
		} `json:"rows"`
	}
	if err := getJSON(ctx, tok, adsenseBase+"/"+account+"/reports:generate?"+q.Encode(), &r); err != nil {
		return nil, "", err
	}
	idx := map[string]int{}
	cur := ""
	for i, h := range r.Headers {
		idx[h.Name] = i
		if h.CurrencyCode != "" {
			cur = h.CurrencyCode
		}
	}
	out := map[string]map[string]map[string]float64{}
	di, ok1 := idx["DATE"]
	ni, ok2 := idx["DOMAIN_NAME"]
	if !ok1 || !ok2 {
		return out, cur, nil
	}
	for _, row := range r.Rows {
		if len(row.Cells) != len(r.Headers) {
			continue
		}
		date := row.Cells[di].Value
		dom := strings.ToLower(strings.TrimPrefix(row.Cells[ni].Value, "www."))
		if out[dom] == nil {
			out[dom] = map[string]map[string]float64{}
		}
		m := out[dom][date]
		if m == nil {
			m = map[string]float64{}
			out[dom][date] = m
		}
		for i, mn := range metrics {
			if j, ok := idx[mn]; ok {
				f, _ := strconv.ParseFloat(row.Cells[j].Value, 64)
				if keys[i] == "ads_rpm" {
					m[keys[i]] = f // recomputed below when merging www/non-www
				} else {
					m[keys[i]] += f
				}
			}
		}
	}
	// recompute RPM from merged totals
	for _, dates := range out {
		for _, m := range dates {
			if m["ads_pageviews"] > 0 {
				m["ads_rpm"] = m["ads_earnings"] / m["ads_pageviews"] * 1000
			}
		}
	}
	return out, cur, nil
}

func ymd(m map[string]int) string {
	if m == nil || m["year"] == 0 {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02d", m["year"], m["month"], m["day"])
}

var _ = json.Marshal

// gscSubmitSitemap (re)submits a sitemap to a Search Console property, which makes
// Google fetch it again soon. Needs the full webmasters scope.
func gscSubmitSitemap(ctx context.Context, tok, site, feed string) error {
	return doJSON(ctx, tok, "PUT", gscBase+"/sites/"+url.PathEscape(site)+"/sitemaps/"+url.PathEscape(feed), nil, nil)
}
