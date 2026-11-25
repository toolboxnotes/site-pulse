package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
)

const appVersion = "1.3.11"

// ---------- data directory ----------

func dataDir() string {
	if d := os.Getenv("SITEPULSE_DATA"); d != "" {
		return d
	}
	base, err := os.UserConfigDir() // Windows: %APPDATA%, Mac: ~/Library/Application Support
	if err != nil || base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	name := "Site Pulse"
	if runtime.GOOS == "linux" {
		name = "site-pulse"
	}
	dir := filepath.Join(base, name)
	migrateOldDataDir(base, dir)
	return dir
}

// migrateOldDataDir moves settings and history from the old "Site Monitor" folder (v1.0–1.1).
func migrateOldDataDir(base, dir string) {
	old := filepath.Join(base, "Site Monitor")
	if runtime.GOOS == "linux" {
		old = filepath.Join(base, "site-monitor")
	}
	if _, err := os.Stat(dir); err == nil {
		return // already have new data
	}
	if st, err := os.Stat(old); err != nil || !st.IsDir() {
		return
	}
	os.Remove(filepath.Join(old, "instance.json"))
	if err := os.Rename(old, dir); err == nil {
		return
	}
	// rename can fail if something holds a file open: copy what matters instead
	os.MkdirAll(filepath.Join(dir, "runs"), 0o700)
	for _, f := range []string{"config.json", "token.json", "metrics.json", "trends.json"} {
		if b, err := os.ReadFile(filepath.Join(old, f)); err == nil {
			os.WriteFile(filepath.Join(dir, f), b, 0o600)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(old, "runs"))
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(old, "runs", e.Name())); err == nil {
			os.WriteFile(filepath.Join(dir, "runs", e.Name()), b, 0o600)
		}
	}
}

// ---------- config ----------

type Site struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	URL         string   `json:"url"`               // homepage, e.g. https://toolboxnotes.com/
	GSCProperty string   `json:"gscProperty"`       // sc-domain:toolboxnotes.com or https://.../
	GAProperty  string   `json:"gaProperty"`        // properties/123456
	AdsDomain   string   `json:"adsDomain"`         // toolboxnotes.com
	BingSite    string   `json:"bingSite"`          // https://toolboxnotes.com/
	KeyPages    []string `json:"keyPages"`          // URLs checked with URL Inspection
	Tracked     []string `json:"tracked,omitempty"` // Search Console queries whose position is tracked
}

type Config struct {
	Lang           string   `json:"lang"`
	ClientID       string   `json:"clientId"`
	ClientSecret   string   `json:"clientSecret"`
	BingKey        string   `json:"bingKey"`
	Sites          []Site   `json:"sites"`
	ScheduleTime   string   `json:"scheduleTime"`   // "09:00" local
	AlertThreshold float64  `json:"alertThreshold"` // percent drop, e.g. 30
	SSLWarnDays    int      `json:"sslWarnDays"`
	DomainWarnDays int      `json:"domainWarnDays"`
	Ignored        []string `json:"ignored,omitempty"` // alert keys the user chose to ignore
	IndexNowKey    string   `json:"indexNowKey,omitempty"`
	PSIKey         string   `json:"psiKey,omitempty"` // optional PageSpeed Insights API key
}

func defaultConfig() Config {
	return Config{ScheduleTime: "09:00", AlertThreshold: 30, SSLWarnDays: 14, DomainWarnDays: 30}
}

type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Expiry       int64  `json:"expiry"` // unix seconds
	Scope        string `json:"scope"`
	Email        string `json:"email"`
}

// ---------- metrics & checks ----------

// Metrics[siteID][date "2006-01-02"][metric] = value
type Metrics map[string]map[string]map[string]float64

type Alert struct {
	Level  string         `json:"level"` // error | warn | info
	SiteID string         `json:"siteId,omitempty"`
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

type SiteCheck struct {
	SiteID  string            `json:"siteId"`
	Status  map[string]any    `json:"status"`  // http, ssl, robots, sitemap, adstxt, domain
	GSC     map[string]any    `json:"gsc"`     // sitemaps, inspections
	AdSense map[string]any    `json:"adsense"` // site state
	Errors  map[string]string `json:"errors"`  // source -> error message
}

type Run struct {
	Time     int64          `json:"time"`
	Date     string         `json:"date"`
	Auth     map[string]any `json:"auth"`
	Account  map[string]any `json:"account"` // adsense account-level info
	Sites    []SiteCheck    `json:"sites"`
	Alerts   []Alert        `json:"alerts"`
	Duration int64          `json:"durationMs"`
}

// Trends imported from Google Trends CSV
type TrendSeries struct {
	ID       string             `json:"id"`
	Term     string             `json:"term"`
	Geo      string             `json:"geo"`
	Imported int64              `json:"imported"`
	File     string             `json:"file"`
	Points   map[string]float64 `json:"points"` // date -> 0..100
}

// ---------- store ----------

type Store struct {
	mu      sync.Mutex
	dir     string
	Config  Config
	Token   *Token
	Metrics Metrics
	Trends  []TrendSeries
}

func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, Config: defaultConfig(), Metrics: Metrics{}}
	readJSON(filepath.Join(dir, "config.json"), &s.Config)
	if s.Config.ScheduleTime == "" {
		s.Config.ScheduleTime = "09:00"
	}
	if s.Config.AlertThreshold <= 0 {
		s.Config.AlertThreshold = 30
	}
	if s.Config.SSLWarnDays <= 0 {
		s.Config.SSLWarnDays = 14
	}
	if s.Config.DomainWarnDays <= 0 {
		s.Config.DomainWarnDays = 30
	}
	var t Token
	if readJSON(filepath.Join(dir, "token.json"), &t) == nil && t.RefreshToken != "" {
		s.Token = &t
	}
	readJSON(filepath.Join(dir, "metrics.json"), &s.Metrics)
	if s.Metrics == nil {
		s.Metrics = Metrics{}
	}
	readJSON(filepath.Join(dir, "trends.json"), &s.Trends)
	return s, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// callers hold s.mu
func (s *Store) saveConfig() error { return writeJSON(filepath.Join(s.dir, "config.json"), s.Config) }
func (s *Store) saveMetrics() error {
	return writeJSON(filepath.Join(s.dir, "metrics.json"), s.Metrics)
}
func (s *Store) saveTrends() error { return writeJSON(filepath.Join(s.dir, "trends.json"), s.Trends) }
func (s *Store) saveToken() error {
	p := filepath.Join(s.dir, "token.json")
	if s.Token == nil {
		err := os.Remove(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeJSON(p, s.Token)
}

func (s *Store) saveRun(r *Run) error {
	return writeJSON(filepath.Join(s.dir, "runs", r.Date+".json"), r)
}

func (s *Store) loadRun(date string) (*Run, error) {
	var r Run
	if err := readJSON(filepath.Join(s.dir, "runs", date+".json"), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) runDates() []string {
	entries, _ := os.ReadDir(filepath.Join(s.dir, "runs"))
	var out []string
	for _, e := range entries {
		n := e.Name()
		if len(n) == len("2006-01-02.json") && filepath.Ext(n) == ".json" {
			out = append(out, n[:10])
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) setMetric(site, date, key string, v float64) {
	m := s.Metrics[site]
	if m == nil {
		m = map[string]map[string]float64{}
		s.Metrics[site] = m
	}
	d := m[date]
	if d == nil {
		d = map[string]float64{}
		m[date] = d
	}
	d[key] = v
}
