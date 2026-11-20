package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var trendsHeaderRe = regexp.MustCompile(`^(.*?):\s*\((.*)\)\s*$`)
var isoDateRe = regexp.MustCompile(`^\d{4}-\d{2}(-\d{2})?`)

// parseTrendsCSV reads a Google Trends "Interest over time" export (multiTimeline.csv).
func parseTrendsCSV(b []byte, fileName string) ([]TrendSeries, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	// some exports are UTF-16 LE
	if len(b) > 2 && b[0] == 0xff && b[1] == 0xfe {
		b = utf16le(b[2:])
	}
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	// header = the row right before the first row that starts with a date
	hdr := -1
	for i := 1; i < len(rows); i++ {
		if len(rows[i]) >= 2 && isoDateRe.MatchString(strings.TrimSpace(rows[i][0])) && len(rows[i-1]) == len(rows[i]) {
			hdr = i - 1
			break
		}
	}
	if hdr < 0 {
		return nil, errors.New("not_trends_csv")
	}
	header := rows[hdr]
	now := time.Now().Unix()
	var out []TrendSeries
	for c := 1; c < len(header); c++ {
		h := strings.TrimSpace(header[c])
		term, geo := h, ""
		if m := trendsHeaderRe.FindStringSubmatch(h); m != nil {
			term, geo = strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		}
		ts := TrendSeries{ID: randString(9), Term: term, Geo: geo, Imported: now, File: fileName, Points: map[string]float64{}}
		for _, row := range rows[hdr+1:] {
			if len(row) <= c {
				continue
			}
			d := strings.TrimSpace(row[0])
			if !isoDateRe.MatchString(d) {
				continue
			}
			d = isoDateRe.FindString(d)
			if len(d) == 7 {
				d += "-01"
			}
			v := strings.TrimSpace(row[c])
			var f float64
			if strings.HasPrefix(v, "<") {
				f = 0.5
			} else {
				f, err = strconv.ParseFloat(v, 64)
				if err != nil {
					continue
				}
			}
			ts.Points[d] = f
		}
		if len(ts.Points) > 0 {
			out = append(out, ts)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("not_trends_csv")
	}
	return out, nil
}

func utf16le(b []byte) []byte {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		sb.WriteRune(rune(uint16(b[i]) | uint16(b[i+1])<<8))
	}
	return []byte(sb.String())
}
