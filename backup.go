package main

// Settings backup: move Site Pulse to another computer.
//
// The file holds the settings (sites, schedule, ignored alerts, tracked keywords) and the keys
// (OAuth client, Bing, PageSpeed, IndexNow), optionally the Google sign-in and the history.
// It is always encrypted with a password the user chooses: AES-256-GCM, key from
// PBKDF2-SHA256 (600,000 rounds, random salt). Without the password the file is useless.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	backupFormat = "site-pulse-backup"
	backupIter   = 600000
	backupMinPw  = 8
	backupMaxLen = 200 << 20
)

type backupFile struct {
	Format string `json:"format"`
	V      int    `json:"v"`
	KDF    string `json:"kdf"`
	Iter   int    `json:"iter"`
	Salt   []byte `json:"salt"`
	Nonce  []byte `json:"nonce"`
	Data   []byte `json:"data"`
}

type backupPayload struct {
	App      string            `json:"app"`
	Version  string            `json:"version"`
	Exported int64             `json:"exported"`
	Config   Config            `json:"config"`
	Token    *Token            `json:"token,omitempty"`
	Files    map[string][]byte `json:"files,omitempty"` // history: data-folder-relative path -> content
}

func backupKey(pw string, salt []byte, iter int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pw, salt, iter, 32)
}

func sealBackup(p backupPayload, pw string) ([]byte, error) {
	plain, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	salt, nonce := make([]byte, 16), make([]byte, 12)
	rand.Read(salt)
	rand.Read(nonce)
	key, err := backupKey(pw, salt, backupIter)
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blk)
	f := backupFile{Format: backupFormat, V: 1, KDF: "pbkdf2-sha256", Iter: backupIter, Salt: salt, Nonce: nonce}
	f.Data = gcm.Seal(nil, nonce, plain, []byte(backupFormat))
	return json.Marshal(f)
}

var errBadPassword = errors.New("wrong password, or the file is damaged")

func openBackup(b []byte, pw string) (*backupPayload, error) {
	var f backupFile
	if json.Unmarshal(b, &f) != nil || f.Format != backupFormat {
		return nil, errors.New("not a Site Pulse backup file")
	}
	if f.V != 1 || f.KDF != "pbkdf2-sha256" || f.Iter < 100000 || f.Iter > 10000000 || len(f.Nonce) != 12 {
		return nil, errors.New("this backup was made by a newer Site Pulse; update first")
	}
	key, err := backupKey(pw, f.Salt, f.Iter)
	if err != nil {
		return nil, err
	}
	blk, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blk)
	plain, err := gcm.Open(nil, f.Nonce, f.Data, []byte(backupFormat))
	if err != nil {
		return nil, errBadPassword
	}
	var p backupPayload
	if err := json.Unmarshal(plain, &p); err != nil || p.App != "Site Pulse" {
		return nil, errBadPassword
	}
	return &p, nil
}

// historyFile reports whether a data-folder file is history worth carrying over.
func historyFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "runs/") {
		return strings.Count(rel, "/") == 1 && strings.HasSuffix(rel, ".json")
	}
	if strings.Contains(rel, "/") || !strings.HasSuffix(rel, ".json") {
		return false
	}
	switch rel {
	case "config.json", "token.json", "instance.json":
		return false
	}
	return true
}

func (a *App) collectHistory() (map[string][]byte, error) {
	files := map[string][]byte{}
	add := func(rel string) error {
		if !historyFile(rel) {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(a.store.dir, rel))
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	}
	top, _ := os.ReadDir(a.store.dir)
	for _, e := range top {
		if !e.IsDir() {
			if err := add(e.Name()); err != nil {
				return nil, err
			}
		}
	}
	runs, _ := os.ReadDir(filepath.Join(a.store.dir, "runs"))
	for _, e := range runs {
		if !e.IsDir() {
			if err := add("runs/" + e.Name()); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password       string `json:"password"`
		IncludeLogin   bool   `json:"includeLogin"`
		IncludeHistory bool   `json:"includeHistory"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		errResp(w, 400, err)
		return
	}
	if len([]rune(in.Password)) < backupMinPw {
		errResp(w, 400, fmt.Errorf("password must be at least %d characters", backupMinPw))
		return
	}
	p := backupPayload{App: "Site Pulse", Version: appVersion, Exported: time.Now().Unix()}
	a.store.mu.Lock()
	p.Config = a.store.Config
	if in.IncludeLogin && a.store.Token != nil {
		t := *a.store.Token
		t.AccessToken, t.Expiry = "", 0 // short-lived; the new computer gets a fresh one
		p.Token = &t
	}
	a.store.mu.Unlock()
	if in.IncludeHistory {
		a.collectMu.Lock() // not while a check is writing files
		files, err := a.collectHistory()
		a.collectMu.Unlock()
		if err != nil {
			errResp(w, 500, err)
			return
		}
		p.Files = files
	}
	b, err := sealBackup(p, in.Password)
	if err != nil {
		errResp(w, 500, err)
		return
	}
	name := "site-pulse-backup-" + time.Now().Format("2006-01-02") + ".sitepulse"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
	log.Printf("settings exported (login %v, history %v)", p.Token != nil, p.Files != nil)
}

// handleImport: body is the backup file, password in the X-Backup-Password header (UTF-8,
// URL-encoded). Replaces settings and keys; the Google sign-in and history only if included.
func (a *App) handleImport(w http.ResponseWriter, r *http.Request) {
	pw, err := url.PathUnescape(r.Header.Get("X-Backup-Password"))
	if err != nil || pw == "" {
		errResp(w, 400, errors.New("password missing"))
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, backupMaxLen+1))
	if err != nil || len(b) > backupMaxLen {
		errResp(w, 400, errors.New("file too large"))
		return
	}
	p, err := openBackup(b, pw)
	if err != nil {
		errResp(w, 400, err)
		return
	}
	if a.isBusy() {
		errResp(w, 409, errors.New("a check is running; try again when it has finished"))
		return
	}
	a.collectMu.Lock()
	defer a.collectMu.Unlock()
	for rel, content := range p.Files {
		if !historyFile(rel) || strings.Contains(rel, "..") || strings.ContainsAny(rel, `\:`) {
			continue
		}
		dst := filepath.Join(a.store.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(dst), 0o700)
		if err := os.WriteFile(dst, content, 0o600); err != nil {
			errResp(w, 500, err)
			return
		}
	}
	s := a.store
	s.mu.Lock()
	keepLang := s.Config.Lang
	s.Config = p.Config
	if s.Config.Lang == "" {
		s.Config.Lang = keepLang
	}
	err = s.saveConfig()
	if err == nil && p.Token != nil {
		s.Token = p.Token
		err = s.saveToken()
	}
	if err == nil && p.Files != nil {
		m := Metrics{}
		if readJSON(filepath.Join(s.dir, "metrics.json"), &m) == nil {
			s.Metrics = m
		}
		var tr []TrendSeries
		if readJSON(filepath.Join(s.dir, "trends.json"), &tr) == nil {
			s.Trends = tr
		}
	}
	s.mu.Unlock()
	if err != nil {
		errResp(w, 500, err)
		return
	}
	log.Printf("settings imported from a %s backup (login %v, history files %d)", p.Version, p.Token != nil, len(p.Files))
	writeJSONResp(w, 200, map[string]any{"ok": true, "login": p.Token != nil, "history": len(p.Files) > 0, "sites": len(p.Config.Sites)})
}
