package main

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tracked keyword positions: kw-<site>.json  query -> date -> {p, c, i}

type KwPoint struct {
	P float64 `json:"p"` // average position
	C float64 `json:"c"` // clicks
	I float64 `json:"i"` // impressions
}

type KwStore map[string]map[string]KwPoint

var kwMu sync.Mutex

func (a *App) kwPath(siteID string) string { return filepath.Join(a.store.dir, "kw-"+siteID+".json") }

func (a *App) loadKw(siteID string) KwStore {
	ks := KwStore{}
	readJSON(a.kwPath(siteID), &ks)
	return ks
}

func (a *App) collectTracked(ctx context.Context, s Site, tok string, from, to time.Time) error {
	if len(s.Tracked) == 0 || s.GSCProperty == "" {
		return nil
	}
	var parts []string
	for _, q := range s.Tracked {
		parts = append(parts, regexp.QuoteMeta(q))
	}
	body := map[string]any{
		"startDate": dateStr(from), "endDate": dateStr(to), "dimensions": []string{"date", "query"}, "rowLimit": 25000,
		"dimensionFilterGroups": []any{map[string]any{"filters": []any{map[string]any{
			"dimension": "query", "operator": "includingRegex", "expression": "^(" + strings.Join(parts, "|") + ")$"}}}},
	}
	gscPageFilter(body, s.URL)
	rows, err := gscQuery(ctx, tok, s.GSCProperty, body)
	if err != nil {
		return err
	}
	kwMu.Lock()
	defer kwMu.Unlock()
	ks := a.loadKw(s.ID)
	for _, r := range rows {
		if len(r.Keys) < 2 {
			continue
		}
		q := r.Keys[1]
		if ks[q] == nil {
			ks[q] = map[string]KwPoint{}
		}
		ks[q][r.Keys[0]] = KwPoint{P: r.Position, C: r.Clicks, I: r.Impressions}
	}
	return writeJSON(a.kwPath(s.ID), ks)
}

// kwAlerts: tracked query whose last-3-day position is 3+ places worse than the 7 days before.
func kwAlerts(s Site, ks KwStore) []Alert {
	var out []Alert
	for _, q := range s.Tracked {
		m := ks[q]
		var ds []string
		for d := range m {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		if len(ds) < 6 {
			continue
		}
		recent := ds[max(0, len(ds)-3):]
		prev := ds[max(0, len(ds)-10) : len(ds)-3]
		w := func(dates []string) (float64, float64) {
			sp, si := 0.0, 0.0
			for _, d := range dates {
				p := m[d]
				wt := p.I
				if wt <= 0 {
					wt = 1
				}
				sp += p.P * wt
				si += wt
			}
			return sp / si, si
		}
		rp, ri := w(recent)
		pp, _ := w(prev)
		if ri >= 5 && rp-pp >= 3 {
			out = append(out, Alert{Level: "warn", SiteID: s.ID, Code: "kw_drop",
				Params: map[string]any{"query": q, "pos": rp, "prev": pp}})
		}
	}
	return out
}
