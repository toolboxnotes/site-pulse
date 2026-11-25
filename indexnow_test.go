package main

import "testing"

func TestNeedsIndexNow(t *testing.T) {
	cases := []struct {
		p    *PageTrack
		want bool
	}{
		{&PageTrack{}, true}, // never sent
		{&PageTrack{Submitted: "2026-11-20", LastMod: "2026-11-19T00:00:00Z"}, false},    // unchanged since send
		{&PageTrack{Submitted: "2026-11-20", LastMod: "2026-11-24T15:00:00.000Z"}, true}, // changed after send
		{&PageTrack{Submitted: "2026-11-20"}, false},                                     // no lastmod: do not resend
		{&PageTrack{Removed: "2026-11-21"}, false},
		{nil, false},
	}
	for i, c := range cases {
		if got := needsIndexNow(c.p); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}
