package main

import "testing"

func TestBackupRoundTrip(t *testing.T) {
	p := backupPayload{App: "Site Pulse", Version: "x", Config: Config{BingKey: "bk", Sites: []Site{{ID: "a"}}},
		Token: &Token{RefreshToken: "rt"}, Files: map[string][]byte{"metrics.json": []byte("{}")}}
	b, err := sealBackup(p, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openBackup(b, "wrong password"); err != errBadPassword {
		t.Fatalf("wrong password: %v", err)
	}
	got, err := openBackup(b, "correct horse")
	if err != nil || got.Config.BingKey != "bk" || got.Token.RefreshToken != "rt" || string(got.Files["metrics.json"]) != "{}" {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	b[len(b)/2] ^= 1
	if _, err := openBackup(b, "correct horse"); err == nil {
		t.Fatal("tampered file accepted")
	}
}

func TestHistoryFile(t *testing.T) {
	for rel, want := range map[string]bool{"metrics.json": true, "runs/2026-11-17.json": true, "audit-a.json": true,
		"config.json": false, "token.json": false, "instance.json": false, "log.txt": false,
		"Browser-chrome/Default/Preferences": false, "runs/x/y.json": false, "../evil.json": false} {
		if historyFile(rel) != want {
			t.Errorf("%s: want %v", rel, want)
		}
	}
}
