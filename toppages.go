package main

import (
	"context"
	"net/url"
	"sort"
	"sync"
	"time"
)

type TopPage struct {
	Path       string  `json:"path"`
	URL        string  `json:"url,omitempty"`
	Views      float64 `json:"views"`
	ViewsPrev  float64 `json:"viewsPrev"`
	Users      float64 `json:"users"`
	Clicks     float64 `json:"clicks"`
	ClicksPrev float64 `json:"clicksPrev"`
	Impr       float64 `json:"impr"`
	Pos        float64 `json:"pos"`
}

// gaPages returns pagePath -> {views, users} for a date range.
func gaPages(ctx context.Context, tok, property, start, end, prefix string) (map[string][2]float64, error) {
	body := map[string]any{
		"dateRanges": []map[string]string{{"startDate": start, "endDate": end}},
		"dimensions": []map[string]string{{"name": "pagePath"}},
		"metrics":    []map[string]string{{"name": "screenPageViews"}, {"name": "activeUsers"}},
		"orderBys":   []any{map[string]any{"metric": map[string]string{"metricName": "screenPageViews"}, "desc": true}},
		"limit":      1000,
	}
	if prefix != "" && prefix != "/" {
		body["dimensionFilter"] = map[string]any{"filter": map[string]any{"fieldName": "pagePath",
			"stringFilter": map[string]any{"matchType": "BEGINS_WITH", "value": prefix}}}
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
	out := map[string][2]float64{}
	for _, row := range r.Rows {
		if len(row.DimensionValues) == 0 || len(row.MetricValues) < 2 {
			continue
		}
		p := row.DimensionValues[0].Value
		v := out[p]
		v[0] += float64(row.MetricValues[0].Value)
		v[1] += float64(row.MetricValues[1].Value)
		out[p] = v
	}
	return out, nil
}

func (a *App) topPages(ctx context.Context, s Site, days int) (map[string]any, error) {
	tok, err := a.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	gaEnd := now.AddDate(0, 0, -1)
	gscEnd := now.AddDate(0, 0, -3) // Search Console lags 2–3 days
	rng := func(end time.Time) (string, string, string, string) {
		cs := end.AddDate(0, 0, -(days - 1))
		pe := cs.AddDate(0, 0, -1)
		ps := pe.AddDate(0, 0, -(days - 1))
		return dateStr(cs), dateStr(end), dateStr(ps), dateStr(pe)
	}
	rows := map[string]*TopPage{}
	get := func(path string) *TopPage {
		if rows[path] == nil {
			rows[path] = &TopPage{Path: path}
		}
		return rows[path]
	}
	errs := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	prefix := pathOf(s)
	if s.GAProperty != "" {
		cs, ce, ps, pe := rng(gaEnd)
		for i, rg := range [][2]string{{cs, ce}, {ps, pe}} {
			wg.Add(1)
			go func(i int, rg [2]string) {
				defer wg.Done()
				m, err := gaPages(ctx, tok, s.GAProperty, rg[0], rg[1], prefix)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs["ga"] = shortErr(err)
					return
				}
				for p, v := range m {
					r := get(p)
					if i == 0 {
						r.Views, r.Users = v[0], v[1]
					} else {
						r.ViewsPrev = v[0]
					}
				}
			}(i, rg)
		}
	}
	if s.GSCProperty != "" {
		cs, ce, ps, pe := rng(gscEnd)
		for i, rg := range [][2]string{{cs, ce}, {ps, pe}} {
			wg.Add(1)
			go func(i int, rg [2]string) {
				defer wg.Done()
				body := map[string]any{"startDate": rg[0], "endDate": rg[1], "dimensions": []string{"page"}, "rowLimit": 1000}
				gscPageFilter(body, s.URL)
				res, err := gscQuery(ctx, tok, s.GSCProperty, body)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs["gsc"] = shortErr(err)
					return
				}
				for _, g := range res {
					if len(g.Keys) == 0 {
						continue
					}
					u, err := url.Parse(g.Keys[0])
					if err != nil {
						continue
					}
					p := u.Path
					if u.RawQuery != "" {
						p += "?" + u.RawQuery
					}
					r := get(p)
					r.URL = g.Keys[0]
					if i == 0 {
						r.Clicks, r.Impr, r.Pos = r.Clicks+g.Clicks, r.Impr+g.Impressions, g.Position
					} else {
						r.ClicksPrev += g.Clicks
					}
				}
			}(i, rg)
		}
	}
	wg.Wait()
	origin := originOf(s)
	var list []*TopPage
	for _, r := range rows {
		if r.URL == "" {
			r.URL = origin + r.Path
		}
		if r.Views+r.ViewsPrev+r.Clicks+r.ClicksPrev+r.Impr == 0 {
			continue
		}
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Views != list[j].Views {
			return list[i].Views > list[j].Views
		}
		return list[i].Clicks > list[j].Clicks
	})
	if len(list) > 500 {
		list = list[:500]
	}
	cs, ce, _, _ := rng(gaEnd)
	gs, ge, _, _ := rng(gscEnd)
	return map[string]any{"pages": list, "errors": errs, "gaRange": []string{cs, ce}, "gscRange": []string{gs, ge}}, nil
}
