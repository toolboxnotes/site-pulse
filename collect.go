package main

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

func dateStr(t time.Time) string { return t.Format("2006-01-02") }

// Collect runs every check for every site and stores the result as today's run.
func (a *App) Collect(ctx context.Context) (*Run, error) {
	if !a.collectMu.TryLock() {
		return nil, errors.New("busy")
	}
	defer a.collectMu.Unlock()
	a.setBusy(true)
	defer a.setBusy(false)

	start := time.Now()
	a.store.mu.Lock()
	cfg := a.store.Config
	sites := append([]Site(nil), cfg.Sites...)
	a.store.mu.Unlock()

	run := &Run{Time: start.Unix(), Date: dateStr(start), Auth: map[string]any{}, Account: map[string]any{}}
	today := time.Now()
	to := today.AddDate(0, 0, -1)
	// normally re-read the last 10 days (late data fills in); backfill 90 days for sites with little history
	from := today.AddDate(0, 0, -10)
	a.store.mu.Lock()
	for _, s := range sites {
		if len(a.store.Metrics[s.ID]) < 45 {
			from = today.AddDate(0, 0, -90)
		}
	}
	a.store.mu.Unlock()

	// --- Google auth ---
	tok, terr := a.accessToken(ctx)
	switch {
	case cfg.ClientID == "":
		run.Auth["google"] = "no_client"
	case errors.Is(terr, ErrNotSignedIn):
		run.Auth["google"] = "signed_out"
	case errors.Is(terr, ErrTokenRevoked):
		run.Auth["google"] = "expired"
	case terr != nil:
		run.Auth["google"] = "error"
		run.Auth["googleError"] = shortErr(terr)
	default:
		run.Auth["google"] = "ok"
		a.store.mu.Lock()
		if a.store.Token != nil {
			run.Auth["googleEmail"] = a.store.Token.Email
			run.Auth["googleScope"] = a.store.Token.Scope
		}
		a.store.mu.Unlock()
	}
	if cfg.BingKey == "" {
		run.Auth["bing"] = "no_key"
	} else {
		run.Auth["bing"] = "ok" // downgraded below if calls fail with 401/403
	}
	gOK := run.Auth["google"] == "ok"

	// --- AdSense account level ---
	var adsAcct *AdsAccount
	var adsByDomain map[string]map[string]map[string]float64
	adsSiteState := map[string]AdsSite{}
	var policy []AdsPolicyIssue
	if gOK {
		accts, err := adsAccounts(ctx, tok)
		if err != nil {
			run.Account["error"] = shortErr(err)
		} else if len(accts) == 0 {
			run.Account["none"] = true
		} else {
			adsAcct = &accts[0]
			run.Account["pubId"] = adsAcct.PubID()
			run.Account["name"] = adsAcct.DisplayName
			run.Account["state"] = adsAcct.State
			run.Account["pendingTasks"] = adsAcct.PendingTasks
			if ss, err := adsSites(ctx, tok, adsAcct.Name); err == nil {
				for _, s := range ss {
					adsSiteState[strings.TrimPrefix(strings.ToLower(s.Domain), "www.")] = s
				}
			} else {
				run.Account["sitesError"] = shortErr(err)
			}
			lang := cfg.Lang
			if lang == "" {
				lang = "en"
			}
			if al, err := adsAlerts(ctx, tok, adsAcct.Name, lang); err == nil {
				run.Account["alerts"] = al
				for _, x := range al {
					lvl := "info"
					switch x.Severity {
					case "SEVERE":
						lvl = "error"
					case "WARNING":
						lvl = "warn"
					}
					run.Alerts = append(run.Alerts, Alert{Level: lvl, Code: "ads_alert",
						Params: map[string]any{"message": x.Message, "type": x.Type}})
				}
			} else {
				run.Account["alertsError"] = shortErr(err)
			}
			if pi, err := adsPolicyIssues(ctx, tok, adsAcct.Name); err == nil {
				policy = pi
				run.Account["policyIssues"] = pi
			} else {
				run.Account["policyError"] = shortErr(err)
			}
			if m, cur, err := adsDaily(ctx, tok, adsAcct.Name, from, to); err == nil {
				adsByDomain = m
				run.Account["currency"] = cur
			} else {
				run.Account["reportError"] = shortErr(err)
			}
			if st := adsAcct.State; st != "" && st != "READY" {
				lvl := "warn"
				if st == "CLOSED" || st == "NEEDS_ATTENTION" {
					lvl = "error"
				}
				run.Alerts = append(run.Alerts, Alert{Level: lvl, Code: "ads_account_state", Params: map[string]any{"state": st}})
			}
		}
	}
	if cur, ok := run.Account["currency"].(string); ok && cur != "" {
		a.store.mu.Lock()
		a.currency = cur
		a.store.mu.Unlock()
	}

	// --- per site, in parallel ---
	results := make([]SiteCheck, len(sites))
	newMetrics := make([]map[string]map[string]float64, len(sites))
	var wg sync.WaitGroup
	for i, s := range sites {
		wg.Add(1)
		go func(i int, s Site) {
			defer wg.Done()
			sc, m := a.collectSite(ctx, s, tok, gOK, cfg, adsAcct, adsByDomain, adsSiteState, from, to)
			results[i] = sc
			newMetrics[i] = m
		}(i, s)
	}
	wg.Wait()
	run.Sites = results

	for _, sc := range results {
		if msg, ok := sc.Errors["bing"]; ok && (strings.Contains(msg, "401") || strings.Contains(msg, "403") || strings.Contains(strings.ToLower(msg), "apikey")) {
			run.Auth["bing"] = "invalid"
		}
	}

	// --- store metrics ---
	a.store.mu.Lock()
	for i, s := range sites {
		for date, m := range newMetrics[i] {
			for k, v := range m {
				a.store.setMetric(s.ID, date, k, v)
			}
		}
	}
	a.store.saveMetrics()
	metrics := a.store.Metrics
	a.store.mu.Unlock()

	// --- alerts ---
	if run.Auth["google"] == "expired" || run.Auth["google"] == "signed_out" {
		run.Alerts = append(run.Alerts, Alert{Level: "error", Code: "auth_google", Params: map[string]any{"state": run.Auth["google"]}})
	}
	if run.Auth["bing"] == "invalid" {
		run.Alerts = append(run.Alerts, Alert{Level: "error", Code: "auth_bing"})
	}
	for i, s := range sites {
		run.Alerts = append(run.Alerts, siteAlerts(s, results[i], cfg, adsAcct != nil)...)
		a.store.mu.Lock()
		run.Alerts = append(run.Alerts, metricAlerts(s.ID, metrics[s.ID], cfg.AlertThreshold, dateStr(today))...)
		run.Alerts = append(run.Alerts, psiAlerts(s.ID, results[i], metrics[s.ID], dateStr(today))...)
		kwMu.Lock()
		run.Alerts = append(run.Alerts, kwAlerts(s, a.loadKw(s.ID))...)
		kwMu.Unlock()
		a.store.mu.Unlock()
		for _, p := range policy {
			if s.AdsDomain != "" && p.Site != "" && !strings.EqualFold(strings.TrimPrefix(p.Site, "www."), s.AdsDomain) {
				continue
			}
			topics := []string{}
			for _, t := range p.PolicyTopics {
				topics = append(topics, t.Topic)
			}
			run.Alerts = append(run.Alerts, Alert{Level: "error", SiteID: s.ID, Code: "ads_policy",
				Params: map[string]any{"uri": p.URI, "action": p.Action, "topics": strings.Join(topics, ", "),
					"since": ymd(p.FirstDetectedDate)}})
		}
	}
	if today.Weekday() == time.Monday && len(sites) > 0 {
		run.Alerts = append(run.Alerts, Alert{Level: "info", Code: "weekly_report",
			Params: map[string]any{"week": dateStr(today.AddDate(0, 0, -7))}})
	}
	sort.SliceStable(run.Alerts, func(i, j int) bool { return levelRank(run.Alerts[i].Level) < levelRank(run.Alerts[j].Level) })
	run.Duration = time.Since(start).Milliseconds()

	a.store.mu.Lock()
	a.store.saveRun(run)
	a.store.mu.Unlock()
	return run, nil
}

func levelRank(l string) int {
	switch l {
	case "error":
		return 0
	case "warn":
		return 1
	}
	return 2
}

func (a *App) collectSite(ctx context.Context, s Site, tok string, gOK bool, cfg Config, adsAcct *AdsAccount,
	adsByDomain map[string]map[string]map[string]float64, adsSiteState map[string]AdsSite, from, to time.Time) (SiteCheck, map[string]map[string]float64) {

	sc := SiteCheck{SiteID: s.ID, Status: map[string]any{}, GSC: map[string]any{}, AdSense: map[string]any{}, Errors: map[string]string{}}
	metrics := map[string]map[string]float64{}
	merge := func(m map[string]map[string]float64) {
		for d, kv := range m {
			if metrics[d] == nil {
				metrics[d] = map[string]float64{}
			}
			for k, v := range kv {
				metrics[d][k] = v
			}
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	set := func(f func()) { mu.Lock(); f(); mu.Unlock() }
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	host := hostOf(s)
	pubID := ""
	if adsAcct != nil {
		pubID = adsAcct.PubID()
	}

	// status checks
	// final URL after redirects (HTTP and in-page, e.g. / → /en/); closed once known
	var homeFinal string
	httpDone := make(chan struct{})
	run(func() {
		h := checkHTTP(ctx, s)
		homeFinal, _ = h["final"].(string)
		close(httpDone)
		if ok, _ := h["ok"].(bool); ok {
			if cold, warm, err := measure(ctx, s.URL); err == nil {
				h["timing"], h["warm"] = cold, warm
				today := dateStr(time.Now())
				set(func() {
					if metrics[today] == nil {
						metrics[today] = map[string]float64{}
					}
					metrics[today]["http_ms"] = float64(cold.Total)
					if warm != nil {
						metrics[today]["http_ttfb"] = float64(warm.TTFB)
					}
				})
			}
		}
		if _, ok := h["sslDays"]; !ok && strings.HasPrefix(s.URL, "https://") && host != "" {
			if t, err := checkSSLDirect(ctx, host); err == nil {
				h["sslExpiry"] = t.Format(time.RFC3339)
				h["sslDays"] = int(time.Until(t).Hours() / 24)
			}
		}
		set(func() { sc.Status["http"] = h })
	})
	run(func() {
		rb, sm := checkRobotsSitemap(ctx, s)
		set(func() { sc.Status["robots"] = rb; sc.Status["sitemap"] = sm })
	})
	if adsAcct != nil || s.AdsDomain != "" {
		run(func() {
			r := checkAdsTxt(ctx, s, pubID)
			set(func() { sc.Status["adstxt"] = r })
		})
	}
	if host != "" && net.ParseIP(host) == nil && host != "localhost" {
		run(func() {
			r := checkDomainExpiry(ctx, registrableDomain(host))
			set(func() { sc.Status["domain"] = r })
		})
	}

	if gOK {
		if s.GAProperty != "" {
			run(func() {
				m, err := gaDaily(ctx, tok, s.GAProperty, dateStr(from), dateStr(to), pathOf(s))
				set(func() {
					if err != nil {
						sc.Errors["ga"] = shortErr(err)
					} else {
						merge(m)
					}
				})
			})
		}
		if s.GSCProperty != "" {
			run(func() {
				m, err := gscDaily(ctx, tok, s.GSCProperty, dateStr(from.AddDate(0, 0, -3)), dateStr(to), s.URL)
				set(func() {
					if err != nil {
						sc.Errors["gsc"] = shortErr(err)
					} else {
						merge(m)
					}
				})
			})
			run(func() {
				if err := a.collectTracked(ctx, s, tok, from.AddDate(0, 0, -3), to); err != nil {
					set(func() { sc.Errors["gscKeywords"] = shortErr(err) })
				}
			})
			run(func() {
				sm, err := gscSitemaps(ctx, tok, s.GSCProperty)
				set(func() {
					if err != nil {
						sc.Errors["gscSitemaps"] = shortErr(err)
					} else {
						sc.GSC["sitemaps"] = sm
					}
				})
			})
			run(func() {
				<-httpDone
				final := homeFinal
				pages := s.KeyPages
				if len(pages) == 0 && s.URL != "" {
					// the homepage may redirect (e.g. / → /ko/); Google indexes the target
					p := s.URL
					if final != "" && strings.HasPrefix(final, "http") {
						p = final
					}
					pages = []string{p}
				}
				var ins []Inspection
				for i, p := range pages {
					if i >= 20 {
						break
					}
					ins = append(ins, gscInspectURL(ctx, tok, s.GSCProperty, p))
				}
				set(func() { sc.GSC["inspections"] = ins })
			})
		}
	}
	// PageSpeed (mobile + desktop) for the page the homepage lands on
	run(func() {
		// A language-redirect root paints nothing before it moves on, so Lighthouse
		// reports NO_FCP there; measure the page visitors actually land on.
		<-httpDone
		page := s.URL
		if strings.HasPrefix(homeFinal, "http") {
			page = homeFinal
		}
		var wg2 sync.WaitGroup
		res := map[string]PSIResult{}
		var mu2 sync.Mutex
		for _, st := range []string{"mobile", "desktop"} {
			wg2.Add(1)
			go func(st string) {
				defer wg2.Done()
				r := runPSI(ctx, page, st, cfg.PSIKey)
				if psiTransient(r.Error) {
					// Lighthouse sometimes fails a run (NO_FCP, timeouts); one retry usually works
					select {
					case <-time.After(5 * time.Second):
						r = runPSI(ctx, page, st, cfg.PSIKey)
					case <-ctx.Done():
					}
				}
				mu2.Lock()
				res[st] = r
				mu2.Unlock()
			}(st)
		}
		wg2.Wait()
		today := dateStr(time.Now())
		set(func() {
			out := map[string]any{"url": page}
			for st, r := range res {
				out[st] = r
				if r.Error != "" {
					sc.Errors["psi"] = r.Error
					continue
				}
				pre := "psi_m_"
				if st == "desktop" {
					pre = "psi_d_"
				}
				if metrics[today] == nil {
					metrics[today] = map[string]float64{}
				}
				metrics[today][pre+"score"] = r.Score
				metrics[today][pre+"lcp"] = r.LCP / 1000
				metrics[today][pre+"cls"] = r.CLS
				metrics[today][pre+"tbt"] = r.TBT
			}
			sc.Status["psi"] = out
		})
	})
	// weekly broken-link scan
	run(func() {
		ls := a.loadLinkScan(s.ID)
		if ls == nil || time.Since(time.Unix(ls.Time, 0)) > 7*24*time.Hour-time.Hour {
			if n, err := a.runLinkScan(ctx, s); err == nil {
				ls = n
			} else {
				set(func() { sc.Errors["links"] = shortErr(err) })
			}
		}
		if sum := linkSummary(ls); sum != nil {
			set(func() { sc.Status["links"] = sum })
		}
	})
	// sitemap pages: new pages, Google index status, IndexNow
	run(func() {
		_, sum, err := a.trackPages(ctx, s, tok, gOK)
		set(func() {
			if err != nil {
				sc.Errors["pages"] = shortErr(err)
			} else {
				sc.GSC["pages"] = sum
			}
		})
	})
	if cfg.BingKey != "" && s.BingSite != "" {
		run(func() {
			m, err := bingDaily(ctx, cfg.BingKey, s.BingSite)
			set(func() {
				if err != nil {
					sc.Errors["bing"] = shortErr(err)
				} else {
					merge(m)
				}
			})
		})
	}
	wg.Wait()

	if adsAcct != nil {
		dom := strings.TrimPrefix(strings.ToLower(s.AdsDomain), "www.")
		if dom == "" {
			dom = strings.TrimPrefix(host, "www.")
		}
		if st, ok := adsSiteState[dom]; ok {
			sc.AdSense["state"] = st.State
			sc.AdSense["autoAds"] = st.AutoAdsEnabled
		} else {
			sc.AdSense["state"] = "NOT_ADDED"
		}
		if m, ok := adsByDomain[dom]; ok {
			merge(m)
		}
		// fill zero earnings for days with no rows so charts are continuous
		for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
			ds := dateStr(d)
			if metrics[ds] == nil {
				metrics[ds] = map[string]float64{}
			}
			if _, ok := metrics[ds]["ads_earnings"]; !ok && adsByDomain != nil {
				metrics[ds]["ads_earnings"] = 0
				metrics[ds]["ads_pageviews"] = 0
			}
		}
	}
	return sc, metrics
}

func siteAlerts(s Site, sc SiteCheck, cfg Config, hasAds bool) []Alert {
	var out []Alert
	add := func(level, code string, p map[string]any) {
		out = append(out, Alert{Level: level, SiteID: s.ID, Code: code, Params: p})
	}
	if h, ok := sc.Status["http"].(map[string]any); ok {
		if ok2, _ := h["ok"].(bool); !ok2 {
			add("error", "site_down", map[string]any{"status": h["status"], "error": h["error"]})
		} else {
			if ms, _ := h["ms"].(int64); ms > 3000 {
				add("warn", "site_slow", map[string]any{"ms": ms})
			}
			if ni, _ := h["noindex"].(bool); ni {
				add("error", "noindex", nil)
			}
			if hasAds {
				if c, _ := h["adsenseCode"].(bool); !c {
					add("warn", "adsense_code_missing", nil)
				}
			}
		}
		if d, ok := h["sslDays"].(int); ok && d < cfg.SSLWarnDays {
			lvl := "warn"
			if d < 7 {
				lvl = "error"
			}
			add(lvl, "ssl_expiring", map[string]any{"days": d})
		}
	}
	if rb, ok := sc.Status["robots"].(map[string]any); ok {
		if b, _ := rb["blocksAll"].(bool); b {
			add("error", "robots_block_all", nil)
		} else if ok2, _ := rb["ok"].(bool); !ok2 {
			add("warn", "robots_missing", map[string]any{"status": rb["status"]})
		}
	}
	if sm, ok := sc.Status["sitemap"].(map[string]any); ok {
		if ok2, _ := sm["ok"].(bool); !ok2 {
			add("warn", "sitemap_missing", map[string]any{"url": sm["url"], "status": sm["status"]})
		}
	}
	if at, ok := sc.Status["adstxt"].(map[string]any); ok && hasAds {
		if f, _ := at["found"].(bool); !f {
			add("error", "adstxt_missing", nil)
		} else if ok2, _ := at["ok"].(bool); !ok2 {
			add("error", "adstxt_mismatch", map[string]any{"pubId": at["pubId"]})
		}
	}
	if dm, ok := sc.Status["domain"].(map[string]any); ok {
		if d, ok := dm["days"].(int); ok && d < cfg.DomainWarnDays {
			lvl := "warn"
			if d < 7 {
				lvl = "error"
			}
			add(lvl, "domain_expiring", map[string]any{"days": d})
		}
	}
	if sms, ok := sc.GSC["sitemaps"].([]GSCSitemap); ok {
		for _, sm := range sms {
			if sm.Errors > 0 {
				add("warn", "gsc_sitemap_errors", map[string]any{"path": sm.Path, "errors": float64(sm.Errors)})
			}
		}
	}
	if ins, ok := sc.GSC["inspections"].([]Inspection); ok {
		for _, in := range ins {
			if in.Error != "" {
				continue
			}
			if in.Verdict != "PASS" {
				lvl := "warn"
				if strings.Contains(strings.ToLower(in.Coverage), "redirect") {
					lvl = "info" // a redirecting URL is not indexed by design
				}
				add(lvl, "page_not_indexed", map[string]any{"url": in.URL, "coverage": in.Coverage})
			}
		}
	}
	if ls, ok := sc.Status["links"].(map[string]any); ok {
		if n, _ := ls["brokenInternal"].(int); n > 0 {
			add("warn", "broken_links", map[string]any{"n": n, "total": ls["broken"]})
		} else if n, _ := ls["broken"].(int); n > 0 {
			add("info", "broken_links_ext", map[string]any{"n": n})
		}
	}
	if ps, ok := sc.GSC["pages"].(map[string]any); ok {
		if n, _ := ps["slow"].(int); n > 0 {
			add("warn", "new_not_indexed", map[string]any{"n": n, "urls": ps["slowUrls"]})
		}
	}
	if st, ok := sc.AdSense["state"].(string); ok && st != "READY" {
		lvl := "info"
		if st == "NEEDS_ATTENTION" {
			lvl = "error"
		}
		add(lvl, "ads_site_state", map[string]any{"state": st})
	}
	for src, msg := range sc.Errors {
		lvl := "warn"
		if src == "psi" && psiTransient(msg) {
			lvl = "info" // a one-off Lighthouse failure, not a problem with the site
		}
		add(lvl, "source_error", map[string]any{"source": src, "message": msg})
	}
	return out
}

// alertSourceLagDays: days at the end of Search Console / Bing data that are still incomplete.
const alertSourceLagDays = 3

// dateBefore returns the YYYY-MM-DD date n days before d (d itself if it does not parse).
func dateBefore(d string, n int) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.AddDate(0, 0, -n).Format("2006-01-02")
}

var alertMetrics = []string{"ga_users", "ga_sessions", "gsc_clicks", "gsc_impr", "ads_earnings", "ads_pageviews", "bing_clicks"}

// minimum baseline below which percentage swings are noise
var alertMinBase = map[string]float64{
	"ga_users": 20, "ga_sessions": 20, "gsc_clicks": 10, "gsc_impr": 50,
	"ads_earnings": 0.5, "ads_pageviews": 50, "bing_clicks": 10,
}

// metricAlerts compares the latest complete day of each metric with the mean of the 7 days before it.
func metricAlerts(siteID string, m map[string]map[string]float64, threshold float64, today string) []Alert {
	var out []Alert
	if m == nil || threshold <= 0 {
		return out
	}
	var dates []string
	for d := range m {
		if d < today {
			dates = append(dates, d)
		}
	}
	sort.Strings(dates)
	for _, key := range alertMetrics {
		// Search Console and Bing keep filling in the last 2–3 days, so a fresh day
		// looks like a drop that later disappears; only compare finished days.
		cutoff := today
		if strings.HasPrefix(key, "gsc_") || strings.HasPrefix(key, "bing_") {
			cutoff = dateBefore(today, alertSourceLagDays)
		}
		var ds []string
		for _, d := range dates {
			if d >= cutoff {
				continue
			}
			if _, ok := m[d][key]; ok {
				ds = append(ds, d)
			}
		}
		if len(ds) < 4 {
			continue
		}
		last := ds[len(ds)-1]
		prev := ds[max(0, len(ds)-8) : len(ds)-1]
		sum := 0.0
		for _, d := range prev {
			sum += m[d][key]
		}
		avg := sum / float64(len(prev))
		if avg < alertMinBase[key] {
			continue
		}
		v := m[last][key]
		pct := (v - avg) / avg * 100
		if pct <= -threshold {
			lvl := "warn"
			if pct <= -2*threshold || pct <= -60 {
				lvl = "error"
			}
			out = append(out, Alert{Level: lvl, SiteID: siteID, Code: "metric_drop",
				Params: map[string]any{"metric": key, "date": last, "value": v, "avg": avg, "pct": pct}})
		} else if pct >= 2*threshold {
			out = append(out, Alert{Level: "info", SiteID: siteID, Code: "metric_rise",
				Params: map[string]any{"metric": key, "date": last, "value": v, "avg": avg, "pct": pct}})
		}
	}
	return out
}

// psiAlerts: mobile score under 50, or 15+ points lower than the last earlier measurement.
func psiAlerts(siteID string, sc SiteCheck, m map[string]map[string]float64, today string) []Alert {
	ps, ok := sc.Status["psi"].(map[string]any)
	if !ok {
		return nil
	}
	mob, ok := ps["mobile"].(PSIResult)
	if !ok || mob.Error != "" {
		return nil
	}
	page, _ := ps["url"].(string)
	var out []Alert
	if mob.Score < 50 {
		out = append(out, Alert{Level: "warn", SiteID: siteID, Code: "psi_poor", Params: map[string]any{"score": mob.Score, "url": page}})
	}
	prevDate := ""
	for d, v := range m {
		if _, ok := v["psi_m_score"]; ok && d < today && d > prevDate {
			prevDate = d
		}
	}
	if prevDate != "" {
		if prev := m[prevDate]["psi_m_score"]; prev-mob.Score >= 15 {
			out = append(out, Alert{Level: "warn", SiteID: siteID, Code: "psi_drop", Params: map[string]any{"score": mob.Score, "prev": prev, "url": page}})
		}
	}
	return out
}
