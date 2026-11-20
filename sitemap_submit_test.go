package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestResubmitSitemapsUsesRobotsAndSitePath(t *testing.T) {
	var mu sync.Mutex
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			fmt.Fprintf(w, "User-agent: *\nSitemap: http://%s/ko/sitemap-index.xml\nSitemap: http://%s/en/sitemap-index.xml\n", r.Host, r.Host)
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/gsc/sites/"):
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			// /gsc/sites/{site}/sitemaps/{feed}, both path-escaped
			parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/gsc/sites/"), "/sitemaps/", 2)
			feed, _ := url.PathUnescape(parts[1])
			mu.Lock()
			puts = append(puts, feed)
			mu.Unlock()
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := gscBase
	gscBase = srv.URL + "/gsc"
	defer func() { gscBase = old }()

	ctx := context.Background()
	got, err := resubmitSitemaps(ctx, Site{ID: "x", URL: srv.URL + "/", GSCProperty: "sc-domain:example.com"}, "tok")
	if err != nil || len(got) != 2 || len(puts) != 2 || !strings.HasSuffix(puts[0], "/ko/sitemap-index.xml") {
		t.Fatalf("root site: got %v err %v puts %v", got, err, puts)
	}
	puts = nil
	got, err = resubmitSitemaps(ctx, Site{ID: "y", URL: srv.URL + "/en/", GSCProperty: "sc-domain:example.com"}, "tok")
	if err != nil || len(got) != 1 || !strings.HasSuffix(puts[0], "/en/sitemap-index.xml") {
		t.Fatalf("/en/ site: got %v err %v puts %v", got, err, puts)
	}
	if _, err := resubmitSitemaps(ctx, Site{ID: "z", URL: srv.URL + "/", GSCProperty: "p"}, "bad"); err == nil {
		t.Fatal("expected error with a bad token")
	}
}

func TestCanSubmitSitemapsNeedsFullScope(t *testing.T) {
	a := &App{store: &Store{}}
	if a.canSubmitSitemaps() {
		t.Fatal("no token")
	}
	a.store.Token = &Token{Scope: "openid https://www.googleapis.com/auth/webmasters.readonly"}
	if a.canSubmitSitemaps() {
		t.Fatal("readonly token must not count")
	}
	a.store.Token.Scope += " " + scopeWebmasters
	if !a.canSubmitSitemaps() {
		t.Fatal("full scope")
	}
}
