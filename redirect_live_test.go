package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The toolboxnotes.com root as deployed on 2026-11-10: a language-detecting
// redirect page with fallback links, then the /en/ home with nav and footer.
func TestRootLanguageRedirectFindsLegalPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<!doctype html><html lang="en"><head><title>Toolbox Notes</title>
<link rel="alternate" hreflang="en" href="/en/"><link rel="alternate" hreflang="ko" href="/ko/">
<link rel="alternate" hreflang="x-default" href="/en/">
<script>(function(){var lang=null;try{lang=localStorage.getItem("lang")}catch(e){}if(lang!=="en"&&lang!=="ko"){lang="en"}location.replace("/" + lang + "/");})();</script>
</head><body><main><h1>Toolbox Notes</h1><p>Small tools, built for convenience. / 편하려고 만든 작은 도구들.</p>
<nav aria-label="Language"><p><a href="/en/">English</a> · <a href="/ko/">한국어</a></p></nav>
<footer><p><a href="/en/about/">About</a> · <a href="/en/privacy/">Privacy Policy</a> · <a href="/en/terms/">Terms of Use</a> · <a href="/en/contact/">Contact</a></p>
<p><a href="/ko/about/">소개</a> · <a href="/ko/privacy/">개인정보처리방침</a> · <a href="/ko/terms/">이용약관</a> · <a href="/ko/contact/">문의</a></p></footer></main></body></html>`))
	})
	mux.HandleFunc("/en/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<!doctype html><html lang="en"><head><title>Toolbox Notes - Small tools</title></head><body>
<nav id="navbar"><a href="/en/">Home</a><a href="/en/archive/">Archive</a><a href="/en/about/">About</a><a href="/en/contact/">Contact</a></nav>
<main><article>Post list ...</article><footer><a href="/en/privacy/">Privacy Policy</a> / <a href="/en/terms/">Terms of Use</a> / <a href="/en/contact/">Contact</a></footer></main></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := fetchPage(context.Background(), srv.URL+"/", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if r.Final != srv.URL+"/en/" {
		t.Fatalf("final = %q, want /en/", r.Final)
	}
	links := extractLinks(string(r.Body), r.Final)
	found := map[string]bool{}
	for _, l := range links {
		for name, re := range map[string]interface{ MatchString(string) bool }{"privacy": rePrivacy, "about": reAbout, "contact": reContact, "terms": reTerms} {
			if re.MatchString(l.Text) || re.MatchString(l.URL) {
				found[name] = true
			}
		}
	}
	for _, n := range []string{"privacy", "about", "contact", "terms"} {
		if !found[n] {
			t.Errorf("%s link not found on landing page", n)
		}
	}
	if !reNav.MatchString(string(r.Body)) {
		t.Error("nav not detected")
	}
}

func TestJSRedirectLiteralOnly(t *testing.T) {
	cases := map[string]string{
		`<script>location.replace("/ko/");</script>`:     "/ko/",
		`<script>window.location.href = '/en/'</script>`: "/en/",
		`<script>location.href="/ko/"
</script>`: "/ko/",
		`<script>location.replace("/" + lang + "/");</script>`: "",
		`<script>location.href = "/" + lang;</script>`:         "",
	}
	for in, want := range cases {
		got := ""
		if m := reJSRedirect.FindStringSubmatch(in); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}
