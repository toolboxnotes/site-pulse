package main

import (
	"testing"
	"time"
)

func TestSigningInKeepsAppAlive(t *testing.T) {
	pendingMu.Lock()
	pending = map[string]pendingAuth{}
	authReturnedAt = time.Time{}
	pendingMu.Unlock()
	if signingIn() {
		t.Fatal("no sign-in yet")
	}
	pendingMu.Lock()
	pending["s"] = pendingAuth{created: time.Now()}
	pendingMu.Unlock()
	if !signingIn() {
		t.Fatal("sign-in in progress must keep the app alive")
	}
	pendingMu.Lock()
	pending["s"] = pendingAuth{created: time.Now().Add(-11 * time.Minute)}
	pendingMu.Unlock()
	if signingIn() {
		t.Fatal("abandoned sign-in must not keep the app alive forever")
	}
	pendingMu.Lock()
	delete(pending, "s")
	authReturnedAt = time.Now()
	pendingMu.Unlock()
	if !signingIn() {
		t.Fatal("just after Google redirects back, wait for the page to reconnect")
	}
}
