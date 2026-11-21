package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func writeJSONResp(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func errResp(w http.ResponseWriter, code int, err error) {
	writeJSONResp(w, code, map[string]string{"error": err.Error()})
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { writeJSONResp(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("POST /api/config", a.handleConfig)
	mux.HandleFunc("POST /api/client", a.handleClient)
	mux.HandleFunc("POST /api/client/clear", a.handleClientClear)
	mux.HandleFunc("GET /oauth/start", a.handleOAuthStart)
	mux.HandleFunc("GET /oauth/callback", a.handleOAuthCallback)
	mux.HandleFunc("POST /api/signout", a.handleSignOut)
	mux.HandleFunc("GET /api/options", a.handleOptions)
	mux.HandleFunc("POST /api/sites", a.handleSites)
	mux.HandleFunc("POST /api/collect", a.handleCollect)
	mux.HandleFunc("GET /api/run", a.handleRun)
	mux.HandleFunc("GET /api/metrics", a.handleMetrics)
	mux.HandleFunc("GET /api/gsc/queries", a.handleGSCQueries)
	mux.HandleFunc("POST /api/gsc/series", a.handleGSCSeries)
	mux.HandleFunc("GET /api/trends", a.handleTrends)
	mux.HandleFunc("POST /api/trends/import", a.handleTrendsImport)
	mux.HandleFunc("POST /api/trends/delete", a.handleTrendsDelete)
	// each open window holds this connection; it drops the moment the window closes
	mux.HandleFunc("GET /api/live", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", 500)
			return
		}
		// only the window Site Pulse opened last counts. Any other page (an older window, maybe
		// hidden behind other windows or left over from an older version) gets 204, which makes
		// its EventSource stop for good instead of reconnecting every 3 seconds.
		replaced, ok := a.currentWindow(r.URL.Query().Get("w"))
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		io.WriteString(w, ": hello\n\n")
		fl.Flush()
		a.liveChange(replaced, 1)
		defer a.liveChange(replaced, -1)
		tk := time.NewTicker(15 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-a.quit:
				return
			case <-replaced:
				io.WriteString(w, "event: replaced\ndata: 1\n\n")
				fl.Flush()
				return
			case <-tk.C:
				if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
					return
				}
				fl.Flush()
			}
		}
	})
	// a second launch of Site Pulse asks for a new window; that window replaces the current one
	mux.HandleFunc("POST /api/window", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, map[string]string{"url": fmt.Sprintf("http://127.0.0.1:%d/?w=%s", a.port, a.newWindow())})
	})
	mux.HandleFunc("POST /api/backup/export", a.handleExport)
	mux.HandleFunc("POST /api/backup/import", a.handleImport)
	mux.HandleFunc("POST /api/ignore", a.handleIgnore)
	mux.HandleFunc("GET /api/pages", a.handlePages)
	mux.HandleFunc("POST /api/xlsx", a.handleXLSX)
	mux.HandleFunc("GET /api/toppages", func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.siteByID(r.URL.Query().Get("site"))
		if !ok {
			errResp(w, 400, errors.New("no site"))
			return
		}
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		if days <= 0 || days > 90 {
			days = 28
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		out, err := a.topPages(ctx, s, days)
		if err != nil {
			errResp(w, 401, err)
			return
		}
		writeJSONResp(w, 200, out)
	})
	mux.HandleFunc("GET /api/tracked", func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.siteByID(r.URL.Query().Get("site"))
		if !ok {
			errResp(w, 400, errors.New("no site"))
			return
		}
		kwMu.Lock()
		ks := a.loadKw(s.ID)
		kwMu.Unlock()
		out := map[string]any{}
		for _, q := range s.Tracked {
			out[q] = ks[q]
		}
		writeJSONResp(w, 200, map[string]any{"tracked": s.Tracked, "data": out})
	})
	mux.HandleFunc("POST /api/track", a.handleTrack)
	mux.HandleFunc("GET /api/links", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, a.loadLinkScan(r.URL.Query().Get("site")))
	})
	mux.HandleFunc("POST /api/links", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Site string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		s, ok := a.siteByID(in.Site)
		if !ok {
			errResp(w, 400, errors.New("no site"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		ls, err := a.runLinkScan(ctx, s)
		if err != nil {
			errResp(w, 502, err)
			return
		}
		writeJSONResp(w, 200, ls)
	})
	mux.HandleFunc("POST /api/pages/refresh", a.handlePagesRefresh)
	mux.HandleFunc("POST /api/gsc/sitemaps/submit", a.handleSitemapSubmit)
	mux.HandleFunc("POST /api/indexnow/key", a.handleIndexNowKey)
	mux.HandleFunc("GET /api/indexnow/keyfile", a.handleIndexNowKeyFile)
	mux.HandleFunc("POST /api/indexnow/submit", a.handleIndexNowSubmit)
	mux.HandleFunc("GET /api/audit", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, a.loadAudit(r.URL.Query().Get("site")))
	})
	mux.HandleFunc("POST /api/audit", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Site string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		s, ok := a.siteByID(in.Site)
		if !ok {
			errResp(w, 400, errors.New("no site"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		writeJSONResp(w, 200, a.runAudit(ctx, s))
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, map[string]any{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); a.Quit() }()
	})
	return a.guard(mux)
}

// guard blocks DNS-rebinding (Host check) and cross-site writes (custom header).
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, port, err := net.SplitHostPort(r.Host)
		if err != nil || (host != "127.0.0.1" && host != "localhost") || port != strconv.Itoa(a.port) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != "GET" && r.Header.Get("X-Site-Pulse") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Google redirects back to the root URL after sign-in
	if q := r.URL.Query(); q.Get("state") != "" && (q.Get("code") != "" || q.Get("error") != "") {
		a.handleOAuthCallback(w, r)
		return
	}
	b, _ := webFS.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	c := a.store.Config
	st := map[string]any{
		"version":    appVersion,
		"dataDir":    a.store.dir,
		"lang":       c.Lang,
		"hasClient":  c.ClientID != "",
		"clientId":   maskID(c.ClientID),
		"hasBingKey": c.BingKey != "",
		"hasPsiKey":  c.PSIKey != "",
		"sites":      c.Sites,
		"schedule":   c.ScheduleTime,
		"threshold":  c.AlertThreshold,
		"sslWarn":    c.SSLWarnDays,
		"domainWarn": c.DomainWarnDays,
		"signedIn":   a.store.Token != nil,
		"busy":       a.isBusy(),
		"currency":   a.currency,
		"browser":    usedBrowser,
		"ignored":    c.Ignored,
	}
	if a.store.Token != nil {
		st["email"] = a.store.Token.Email
		st["scope"] = a.store.Token.Scope
	}
	a.store.mu.Unlock()
	dates := a.store.runDates()
	st["runDates"] = dates
	if st["currency"] == "" && len(dates) > 0 {
		if run, err := a.store.loadRun(dates[len(dates)-1]); err == nil {
			if cur, ok := run.Account["currency"].(string); ok {
				st["currency"] = cur
			}
		}
	}
	writeJSONResp(w, 200, st)
}

func maskID(id string) string {
	i := strings.Index(id, ".apps")
	if len(id) < 16 || i < 8 {
		return id
	}
	return id[:8] + "…" + id[i:]
}

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	var in map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		errResp(w, 400, err)
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	c := &a.store.Config
	str := func(k string, dst *string) {
		if v, ok := in[k]; ok {
			json.Unmarshal(v, dst)
			*dst = strings.TrimSpace(*dst)
		}
	}
	str("lang", &c.Lang)
	str("bingKey", &c.BingKey)
	str("psiKey", &c.PSIKey)
	if v, ok := in["schedule"]; ok {
		var s string
		json.Unmarshal(v, &s)
		if regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`).MatchString(s) {
			c.ScheduleTime = s
		}
	}
	if v, ok := in["threshold"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil && f >= 5 && f <= 95 {
			c.AlertThreshold = f
		}
	}
	if v, ok := in["sslWarn"]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil && n > 0 && n <= 120 {
			c.SSLWarnDays = n
		}
	}
	if v, ok := in["domainWarn"]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil && n > 0 && n <= 365 {
			c.DomainWarnDays = n
		}
	}
	if err := a.store.saveConfig(); err != nil {
		errResp(w, 500, err)
		return
	}
	writeJSONResp(w, 200, map[string]any{"ok": true})
}

func (a *App) handleClient(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	id, sec, err := parseClientJSON(b)
	if err != nil {
		errResp(w, 400, err)
		return
	}
	a.store.mu.Lock()
	changed := a.store.Config.ClientID != id
	a.store.Config.ClientID, a.store.Config.ClientSecret = id, sec
	if changed {
		a.store.Token = nil
		a.store.saveToken()
	}
	err = a.store.saveConfig()
	a.store.mu.Unlock()
	if err != nil {
		errResp(w, 500, err)
		return
	}
	writeJSONResp(w, 200, map[string]any{"ok": true, "clientId": maskID(id)})
}

func (a *App) handleClientClear(w http.ResponseWriter, r *http.Request) {
	a.signOut(r.Context())
	a.store.mu.Lock()
	a.store.Config.ClientID, a.store.Config.ClientSecret = "", ""
	a.store.saveConfig()
	a.store.mu.Unlock()
	writeJSONResp(w, 200, map[string]any{"ok": true})
}

func (a *App) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	u, err := a.authStartURL(a.port)
	if err != nil {
		http.Redirect(w, r, "/#settings?auth=no_client", http.StatusFound)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

func (a *App) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		pendingMu.Lock()
		delete(pending, q.Get("state"))
		authReturnedAt = time.Now()
		pendingMu.Unlock()
		http.Redirect(w, r, "/#settings?auth="+url.QueryEscape(e), http.StatusFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.finishAuth(ctx, q.Get("state"), q.Get("code")); err != nil {
		log.Printf("oauth: %v", err)
		http.Redirect(w, r, "/#settings?auth="+url.QueryEscape("error:"+err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/#settings?auth=ok", http.StatusFound)
}

func (a *App) handleSignOut(w http.ResponseWriter, r *http.Request) {
	a.signOut(r.Context())
	writeJSONResp(w, 200, map[string]any{"ok": true})
}

// handleOptions lists what the signed-in account can see, for the site setup form.
func (a *App) handleOptions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	out := map[string]any{}
	errs := map[string]string{}
	tok, err := a.accessToken(ctx)
	if err == nil {
		if v, e := gscListSites(ctx, tok); e == nil {
			out["gsc"] = v
		} else {
			errs["gsc"] = e.Error()
		}
		if v, e := gaListProperties(ctx, tok); e == nil {
			out["ga"] = v
		} else {
			errs["ga"] = e.Error()
		}
		if accts, e := adsAccounts(ctx, tok); e == nil {
			var doms []AdsSite
			if len(accts) > 0 {
				out["adsAccount"] = accts[0].PubID()
				if ss, e2 := adsSites(ctx, tok, accts[0].Name); e2 == nil {
					doms = ss
				} else {
					errs["ads"] = e2.Error()
				}
			}
			out["ads"] = doms
		} else {
			errs["ads"] = e.Error()
		}
	} else {
		errs["google"] = err.Error()
	}
	a.store.mu.Lock()
	key := a.store.Config.BingKey
	a.store.mu.Unlock()
	if key != "" {
		if v, e := bingSites(ctx, key); e == nil {
			out["bing"] = v
		} else {
			errs["bing"] = e.Error()
		}
	}
	out["errors"] = errs
	writeJSONResp(w, 200, out)
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

func (a *App) handleSites(w http.ResponseWriter, r *http.Request) {
	var sites []Site
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&sites); err != nil {
		errResp(w, 400, err)
		return
	}
	seen := map[string]bool{}
	for i := range sites {
		s := &sites[i]
		s.URL = strings.TrimSpace(s.URL)
		if s.URL == "" {
			errResp(w, 400, fmt.Errorf("site %d: url required", i+1))
			return
		}
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			s.URL = "https://" + s.URL
		}
		u, err := url.Parse(s.URL)
		if err != nil || u.Host == "" {
			errResp(w, 400, fmt.Errorf("site %d: invalid url", i+1))
			return
		}
		if u.Path == "" {
			s.URL += "/"
		}
		if !idRe.MatchString(s.ID) || seen[s.ID] {
			s.ID = randString(6)
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Name) == "" {
			s.Name = u.Host + strings.TrimSuffix(u.Path, "/")
		}
		s.AdsDomain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s.AdsDomain)), "www.")
		var kp []string
		for _, p := range s.KeyPages {
			p = strings.TrimSpace(p)
			if p != "" {
				kp = append(kp, p)
			}
		}
		s.KeyPages = kp
	}
	a.store.mu.Lock()
	a.store.Config.Sites = sites
	err := a.store.saveConfig()
	a.store.mu.Unlock()
	if err != nil {
		errResp(w, 500, err)
		return
	}
	writeJSONResp(w, 200, sites)
}

func (a *App) handleCollect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	run, err := a.Collect(ctx)
	if err != nil {
		errResp(w, 409, err)
		return
	}
	writeJSONResp(w, 200, run)
}

func (a *App) handleRun(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		ds := a.store.runDates()
		if len(ds) == 0 {
			writeJSONResp(w, 200, nil)
			return
		}
		date = ds[len(ds)-1]
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(date) {
		errResp(w, 400, errors.New("bad date"))
		return
	}
	run, err := a.store.loadRun(date)
	if err != nil {
		writeJSONResp(w, 200, nil) // no check on that day
		return
	}
	writeJSONResp(w, 200, run)
}

func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	out := map[string]map[string]map[string]float64{}
	for site, dates := range a.store.Metrics {
		if s := q.Get("site"); s != "" && s != site {
			continue
		}
		m := map[string]map[string]float64{}
		for d, v := range dates {
			if (from == "" || d >= from) && (to == "" || d <= to) {
				m[d] = v
			}
		}
		out[site] = m
	}
	writeJSONResp(w, 200, out)
}

func (a *App) siteByID(id string) (Site, bool) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for _, s := range a.store.Config.Sites {
		if s.ID == id {
			return s, true
		}
	}
	return Site{}, false
}

func (a *App) handleGSCQueries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s, ok := a.siteByID(q.Get("site"))
	if !ok || s.GSCProperty == "" {
		errResp(w, 400, errors.New("no_gsc"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tok, err := a.accessToken(ctx)
	if err != nil {
		errResp(w, 401, err)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	body := map[string]any{"startDate": q.Get("start"), "endDate": q.Get("end"),
		"dimensions": []string{"query"}, "rowLimit": limit}
	gscPageFilter(body, s.URL)
	cur, err := gscQuery(ctx, tok, s.GSCProperty, body)
	if err != nil {
		errResp(w, 502, err)
		return
	}
	// previous period of equal length for comparison
	out := map[string]any{"rows": cur}
	if st, e1 := time.Parse("2006-01-02", q.Get("start")); e1 == nil {
		if en, e2 := time.Parse("2006-01-02", q.Get("end")); e2 == nil {
			days := int(en.Sub(st).Hours()/24) + 1
			pe := st.AddDate(0, 0, -1)
			ps := pe.AddDate(0, 0, -(days - 1))
			pb := map[string]any{"startDate": dateStr(ps), "endDate": dateStr(pe), "dimensions": []string{"query"}, "rowLimit": 5000}
			gscPageFilter(pb, s.URL)
			prev, err := gscQuery(ctx, tok, s.GSCProperty, pb)
			if err == nil {
				pm := map[string]GSCRow{}
				for _, p := range prev {
					if len(p.Keys) > 0 {
						pm[p.Keys[0]] = p
					}
				}
				out["prev"] = pm
				out["prevRange"] = []string{dateStr(ps), dateStr(pe)}
			}
		}
	}
	writeJSONResp(w, 200, out)
}

func (a *App) handleGSCSeries(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Site    string   `json:"site"`
		Start   string   `json:"start"`
		End     string   `json:"end"`
		Queries []string `json:"queries"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		errResp(w, 400, err)
		return
	}
	s, ok := a.siteByID(in.Site)
	if !ok || s.GSCProperty == "" || len(in.Queries) == 0 {
		errResp(w, 400, errors.New("no_gsc"))
		return
	}
	if len(in.Queries) > 10 {
		in.Queries = in.Queries[:10]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tok, err := a.accessToken(ctx)
	if err != nil {
		errResp(w, 401, err)
		return
	}
	var parts []string
	for _, q := range in.Queries {
		parts = append(parts, regexp.QuoteMeta(q))
	}
	body := map[string]any{
		"startDate": in.Start, "endDate": in.End, "dimensions": []string{"date", "query"}, "rowLimit": 25000,
		"dimensionFilterGroups": []any{map[string]any{"filters": []any{map[string]any{
			"dimension": "query", "operator": "includingRegex", "expression": "^(" + strings.Join(parts, "|") + ")$"}}}},
	}
	gscPageFilter(body, s.URL)
	rows, err := gscQuery(ctx, tok, s.GSCProperty, body)
	if err != nil {
		errResp(w, 502, err)
		return
	}
	out := map[string]map[string]GSCRow{}
	for _, q := range in.Queries {
		out[q] = map[string]GSCRow{}
	}
	for _, row := range rows {
		if len(row.Keys) < 2 {
			continue
		}
		if m, ok := out[row.Keys[1]]; ok {
			m[row.Keys[0]] = row
		}
	}
	writeJSONResp(w, 200, out)
}

func (a *App) handleTrends(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	t := a.store.Trends
	if t == nil {
		t = []TrendSeries{}
	}
	writeJSONResp(w, 200, t)
}

func (a *App) handleTrendsImport(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	name := r.Header.Get("X-File-Name")
	if n, err := url.QueryUnescape(name); err == nil {
		name = n
	}
	series, err := parseTrendsCSV(b, name)
	if err != nil {
		errResp(w, 400, err)
		return
	}
	a.store.mu.Lock()
	// same term + geo replaces the older import (merging points)
	for _, ns := range series {
		replaced := false
		for i := range a.store.Trends {
			old := &a.store.Trends[i]
			if strings.EqualFold(old.Term, ns.Term) && old.Geo == ns.Geo {
				for d, v := range ns.Points {
					old.Points[d] = v
				}
				old.Imported, old.File = ns.Imported, ns.File
				replaced = true
			}
		}
		if !replaced {
			a.store.Trends = append(a.store.Trends, ns)
		}
	}
	sort.SliceStable(a.store.Trends, func(i, j int) bool { return a.store.Trends[i].Imported > a.store.Trends[j].Imported })
	err = a.store.saveTrends()
	a.store.mu.Unlock()
	if err != nil {
		errResp(w, 500, err)
		return
	}
	writeJSONResp(w, 200, map[string]any{"ok": true, "count": len(series)})
}

func (a *App) handleTrendsDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	a.store.mu.Lock()
	var keep []TrendSeries
	for _, t := range a.store.Trends {
		if t.ID != in.ID {
			keep = append(keep, t)
		}
	}
	a.store.Trends = keep
	a.store.saveTrends()
	a.store.mu.Unlock()
	writeJSONResp(w, 200, map[string]any{"ok": true})
}

func (a *App) handleIgnore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
		On  bool   `json:"on"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil || in.Key == "" || len(in.Key) > 600 {
		errResp(w, 400, errors.New("bad key"))
		return
	}
	a.store.mu.Lock()
	var keep []string
	for _, k := range a.store.Config.Ignored {
		if k != in.Key {
			keep = append(keep, k)
		}
	}
	if in.On {
		keep = append(keep, in.Key)
	}
	if len(keep) > 500 {
		keep = keep[len(keep)-500:]
	}
	a.store.Config.Ignored = keep
	err := a.store.saveConfig()
	a.store.mu.Unlock()
	if err != nil {
		errResp(w, 500, err)
		return
	}
	writeJSONResp(w, 200, map[string]any{"ok": true})
}

func (a *App) handlePages(w http.ResponseWriter, r *http.Request) {
	s, ok := a.siteByID(r.URL.Query().Get("site"))
	if !ok {
		errResp(w, 400, errors.New("no site"))
		return
	}
	pagesMu.Lock()
	ps := a.loadPages(s.ID)
	pagesMu.Unlock()
	type row struct {
		URL string `json:"url"`
		*PageTrack
	}
	var rows []row
	for u, p := range ps.Pages {
		rows = append(rows, row{u, p})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Initial != rows[j].Initial {
			return !rows[i].Initial
		}
		if rows[i].FirstSeen != rows[j].FirstSeen {
			return rows[i].FirstSeen > rows[j].FirstSeen
		}
		if rows[i].LastMod != rows[j].LastMod {
			return rows[i].LastMod > rows[j].LastMod
		}
		return rows[i].URL < rows[j].URL
	})
	a.store.mu.Lock()
	key := a.store.Config.IndexNowKey
	a.store.mu.Unlock()
	in := map[string]any{"key": key}
	if key != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		okv, why := indexNowVerified(ctx, s, key)
		in["keyUrl"], in["verified"], in["why"] = indexNowKeyURL(s, key), okv, why
	}
	gsc := map[string]any{"canSubmit": a.canSubmitSitemaps(), "submitted": ps.SitemapSubmitted, "files": ps.SitemapFiles}
	writeJSONResp(w, 200, map[string]any{"updated": ps.Updated, "pages": rows, "summary": pageSummary(ps), "indexNow": in, "gscSitemaps": gsc})
}

// handleSitemapSubmit resubmits the site's sitemaps to Search Console on request.
func (a *App) handleSitemapSubmit(w http.ResponseWriter, r *http.Request) {
	var in struct{ Site string }
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	s, ok := a.siteByID(in.Site)
	if !ok || s.GSCProperty == "" {
		errResp(w, 400, errors.New("no_gsc_property"))
		return
	}
	if !a.canSubmitSitemaps() {
		errResp(w, 403, errors.New("need_gsc_write"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	tok, err := a.accessToken(ctx)
	if err != nil {
		errResp(w, 401, err)
		return
	}
	files, err := resubmitSitemaps(ctx, s, tok)
	if err != nil {
		errResp(w, 502, err)
		return
	}
	if len(files) == 0 {
		errResp(w, 404, errors.New("no_sitemap"))
		return
	}
	pagesMu.Lock()
	ps := a.loadPages(s.ID)
	ps.SitemapSubmitted, ps.SitemapFiles = dateStr(time.Now()), files
	writeJSON(a.pagesPath(s.ID), ps)
	pagesMu.Unlock()
	log.Printf("resubmitted %d sitemap(s) to Search Console for %s (manual)", len(files), s.ID)
	writeJSONResp(w, 200, map[string]any{"ok": true, "files": files})
}

func (a *App) handlePagesRefresh(w http.ResponseWriter, r *http.Request) {
	var in struct{ Site string }
	json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	s, ok := a.siteByID(in.Site)
	if !ok {
		errResp(w, 400, errors.New("no site"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	tok, err := a.accessToken(ctx)
	_, sum, err2 := a.trackPages(ctx, s, tok, err == nil)
	if err2 != nil {
		errResp(w, 502, err2)
		return
	}
	writeJSONResp(w, 200, sum)
}

func (a *App) handleIndexNowKey(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	if a.store.Config.IndexNowKey == "" {
		a.store.Config.IndexNowKey = newIndexNowKey()
		a.store.saveConfig()
	}
	key := a.store.Config.IndexNowKey
	a.store.mu.Unlock()
	writeJSONResp(w, 200, map[string]any{"key": key})
}

func (a *App) handleIndexNowKeyFile(w http.ResponseWriter, r *http.Request) {
	a.store.mu.Lock()
	key := a.store.Config.IndexNowKey
	a.store.mu.Unlock()
	if key == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+key+`.txt"`)
	io.WriteString(w, key)
}

func (a *App) handleIndexNowSubmit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Site string   `json:"site"`
		URLs []string `json:"urls"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
	s, ok := a.siteByID(in.Site)
	if !ok {
		errResp(w, 400, errors.New("no site"))
		return
	}
	a.store.mu.Lock()
	key := a.store.Config.IndexNowKey
	a.store.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if okv, why := indexNowVerified(ctx, s, key); !okv {
		errResp(w, 400, errors.New("key_not_verified:"+why))
		return
	}
	pagesMu.Lock()
	ps := a.loadPages(s.ID)
	pagesMu.Unlock()
	urls := in.URLs
	if len(urls) == 0 { // default: everything Google hasn't indexed yet
		for u, p := range ps.Pages {
			if p.Removed == "" && p.Verdict != "PASS" {
				urls = append(urls, u)
			}
		}
	}
	if len(urls) == 0 {
		writeJSONResp(w, 200, map[string]any{"ok": true, "count": 0})
		return
	}
	if err := indexNowSubmit(ctx, s, key, urls); err != nil {
		errResp(w, 502, err)
		return
	}
	today := dateStr(time.Now())
	pagesMu.Lock()
	ps = a.loadPages(s.ID)
	for _, u := range urls {
		if p := ps.Pages[u]; p != nil {
			p.Submitted = today
		}
	}
	writeJSON(a.pagesPath(s.ID), ps)
	pagesMu.Unlock()
	writeJSONResp(w, 200, map[string]any{"ok": true, "count": len(urls)})
}

// handleTrack adds or removes a tracked query and backfills 90 days for a new one.
func (a *App) handleTrack(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Site  string `json:"site"`
		Query string `json:"query"`
		On    bool   `json:"on"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in)
	q := strings.TrimSpace(in.Query)
	if q == "" || len(q) > 200 {
		errResp(w, 400, errors.New("bad query"))
		return
	}
	a.store.mu.Lock()
	var site Site
	found := false
	for i := range a.store.Config.Sites {
		s := &a.store.Config.Sites[i]
		if s.ID != in.Site {
			continue
		}
		var keep []string
		for _, t := range s.Tracked {
			if t != q {
				keep = append(keep, t)
			}
		}
		if in.On {
			keep = append(keep, q)
		}
		if len(keep) > 50 {
			keep = keep[len(keep)-50:]
		}
		s.Tracked = keep
		site, found = *s, true
	}
	a.store.saveConfig()
	a.store.mu.Unlock()
	if !found {
		errResp(w, 400, errors.New("no site"))
		return
	}
	if in.On {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		if tok, err := a.accessToken(ctx); err == nil {
			one := site
			one.Tracked = []string{q}
			a.collectTracked(ctx, one, tok, time.Now().AddDate(0, 0, -93), time.Now().AddDate(0, 0, -1))
		}
	}
	writeJSONResp(w, 200, map[string]any{"ok": true, "tracked": site.Tracked})
}
