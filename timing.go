package main

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptrace"
	"time"
)

// Timing splits one page load into network, server and download time.
type Timing struct {
	DNS       int64 `json:"dns"`
	Connect   int64 `json:"connect"`
	TLS       int64 `json:"tls"`
	Server    int64 `json:"server"`   // waiting for the first byte (incl. redirect hops)
	Download  int64 `json:"download"` // first byte → last byte
	TTFB      int64 `json:"ttfb"`
	Total     int64 `json:"total"`
	Redirects int   `json:"redirects"`
	Reused    bool  `json:"reused,omitempty"`
}

// measure loads u twice on one connection pool: cold (fresh DNS/TCP/TLS) and warm (reused connection).
func measure(ctx context.Context, u string) (cold, warm *Timing, err error) {
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{},
		MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second, ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	once := func() (*Timing, error) {
		t := &Timing{}
		var dnsStart, connStart, tlsStart time.Time
		var firstByte time.Time
		trace := &httptrace.ClientTrace{
			DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
			DNSDone:              func(httptrace.DNSDoneInfo) { t.DNS += time.Since(dnsStart).Milliseconds() },
			ConnectStart:         func(_, _ string) { connStart = time.Now() },
			ConnectDone:          func(_, _ string, _ error) { t.Connect += time.Since(connStart).Milliseconds() },
			TLSHandshakeStart:    func() { tlsStart = time.Now() },
			TLSHandshakeDone:     func(tls.ConnectionState, error) { t.TLS += time.Since(tlsStart).Milliseconds() },
			GotConn:              func(i httptrace.GotConnInfo) { t.Reused = t.Reused || i.Reused },
			GotFirstResponseByte: func() { firstByte = time.Now() },
		}
		hops := 0
		c := &http.Client{Transport: tr, Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			hops = len(via)
			if len(via) >= 10 {
				return http.ErrUseLastResponse
			}
			return nil
		}}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)
		start := time.Now()
		res, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		end := time.Now()
		t.Total = end.Sub(start).Milliseconds()
		if firstByte.IsZero() {
			firstByte = end
		}
		t.TTFB = firstByte.Sub(start).Milliseconds()
		t.Download = end.Sub(firstByte).Milliseconds()
		t.Server = max(0, t.TTFB-t.DNS-t.Connect-t.TLS)
		t.Redirects = hops
		return t, nil
	}
	cold, err = once()
	if err != nil {
		return nil, nil, err
	}
	warm, _ = once()
	return cold, warm, nil
}
