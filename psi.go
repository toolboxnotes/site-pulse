package main

import (
	"context"
	"net/url"
	"strings"
)

var psiBase = "https://www.googleapis.com/pagespeedonline/v5/runPagespeed"

type PSIResult struct {
	Strategy string  `json:"strategy"`
	Score    float64 `json:"score"` // 0–100 (lab)
	LCP      float64 `json:"lcp"`   // lab, ms
	CLS      float64 `json:"cls"`   // lab
	TBT      float64 `json:"tbt"`   // lab, ms
	FCP      float64 `json:"fcp"`   // lab, ms
	// real-user data (Chrome UX Report), if Google has enough visits
	FieldLCP float64 `json:"fieldLcp,omitempty"`
	FieldINP float64 `json:"fieldInp,omitempty"`
	FieldCLS float64 `json:"fieldCls,omitempty"`
	FieldCat string  `json:"fieldCategory,omitempty"`
	Error    string  `json:"error,omitempty"`
}

func runPSI(ctx context.Context, page, strategy, key string) PSIResult {
	q := url.Values{"url": {page}, "strategy": {strategy}, "category": {"performance"}}
	if key != "" {
		q.Set("key", key)
	}
	var r struct {
		LoadingExperience struct {
			Metrics map[string]struct {
				Percentile float64 `json:"percentile"`
			} `json:"metrics"`
			OverallCategory string `json:"overall_category"`
		} `json:"loadingExperience"`
		LighthouseResult struct {
			Categories struct {
				Performance struct {
					Score float64 `json:"score"`
				} `json:"performance"`
			} `json:"categories"`
			Audits map[string]struct {
				NumericValue float64 `json:"numericValue"`
			} `json:"audits"`
		} `json:"lighthouseResult"`
	}
	res := PSIResult{Strategy: strategy}
	if err := getJSON(ctx, "", psiBase+"?"+q.Encode(), &r); err != nil {
		res.Error = shortErr(err)
		if ae, ok := err.(*APIError); ok && ae.Status == 429 {
			res.Error = "quota"
		}
		return res
	}
	lh := r.LighthouseResult
	res.Score = lh.Categories.Performance.Score * 100
	res.LCP = lh.Audits["largest-contentful-paint"].NumericValue
	res.CLS = lh.Audits["cumulative-layout-shift"].NumericValue
	res.TBT = lh.Audits["total-blocking-time"].NumericValue
	res.FCP = lh.Audits["first-contentful-paint"].NumericValue
	m := r.LoadingExperience.Metrics
	if v, ok := m["LARGEST_CONTENTFUL_PAINT_MS"]; ok {
		res.FieldLCP = v.Percentile
	}
	if v, ok := m["INTERACTION_TO_NEXT_PAINT"]; ok {
		res.FieldINP = v.Percentile
	}
	if v, ok := m["CUMULATIVE_LAYOUT_SHIFT_SCORE"]; ok {
		res.FieldCLS = v.Percentile / 100
	}
	res.FieldCat = strings.ToLower(r.LoadingExperience.OverallCategory)
	return res
}

// psiTransient reports PageSpeed errors that come from a failed Lighthouse run rather
// than from the site: the same page usually measures fine on the next try.
func psiTransient(msg string) bool {
	if msg == "" {
		return false
	}
	for _, k := range []string{"NO_FCP", "NO_LCP", "FAILED_DOCUMENT_REQUEST", "PROTOCOL_TIMEOUT", "Lighthouse returned error", "HTTP 500", "HTTP 503"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}
