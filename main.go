package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultPort = 17380

var usedBrowser string // shown in Settings → About

type App struct {
	store     *Store
	port      int
	tokMu     sync.Mutex
	collectMu sync.Mutex
	busyMu    sync.Mutex
	busy      bool
	currency  string
	quit      chan struct{}
	quitOnce  sync.Once
	liveMu    sync.Mutex
	live      int           // open /api/live connections from the current window
	connected bool          // the current window has connected since it was opened
	zeroAt    time.Time     // when the current window's last connection dropped
	launchAt  time.Time     // when the current window was opened
	anyWindow bool          // -no-browser: any page counts as the window (testing)
	winToken  string        // identifies the one window Site Pulse currently opened
	winCh     chan struct{} // closed when that window is replaced by a newer one
}

// newWindow starts a new window generation: the window opened with the returned token is the
// only one that keeps Site Pulse running. Older windows (including ones hidden behind other
// windows, or left over from an older version) are told to close and are never counted again.
func (a *App) newWindow() string {
	b := make([]byte, 12)
	rand.Read(b)
	a.liveMu.Lock()
	defer a.liveMu.Unlock()
	if a.winCh != nil {
		close(a.winCh)
	}
	a.winToken = hex.EncodeToString(b)
	a.winCh = make(chan struct{})
	a.live = 0 // the old window's connections no longer count
	a.connected = false
	a.launchAt = time.Now()
	return a.winToken
}

// currentWindow reports whether tok belongs to the current window, and its replace signal.
func (a *App) currentWindow(tok string) (chan struct{}, bool) {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()
	if a.anyWindow {
		if a.winCh == nil {
			a.winCh = make(chan struct{})
		}
		return a.winCh, true
	}
	if tok == "" || tok != a.winToken {
		return nil, false
	}
	return a.winCh, true
}

func (a *App) setBusy(b bool) { a.busyMu.Lock(); a.busy = b; a.busyMu.Unlock() }
func (a *App) isBusy() bool   { a.busyMu.Lock(); defer a.busyMu.Unlock(); return a.busy }
func (a *App) Quit()          { a.quitOnce.Do(func() { close(a.quit) }) }

func (a *App) liveChange(ch chan struct{}, d int) {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()
	if ch != a.winCh { // a connection of a replaced window ended: not counted any more
		return
	}
	a.live += d
	if d > 0 && !a.connected {
		// first connection of a newly opened window: make sure the user can actually see it
		go func() {
			if msg := bringWindowToFront(); msg != "" {
				log.Printf("window to front: %s", msg)
			}
		}()
	}
	if d > 0 {
		a.connected = true
	}
	if a.live == 0 {
		a.zeroAt = time.Now()
	}
	log.Printf("window connections: %d", a.live)
}

// quitWhenUIGone: every open window keeps a live connection to us. When the last one
// closes (window closed, or Chrome quit) we exit a few seconds later; a page reload
// reconnects within a second, so it does not count. Until a newly opened window has
// connected, wait up to `grace` for it. A running check is always finished first.
func (a *App) quitWhenUIGone(grace time.Duration) {
	for {
		select {
		case <-a.quit:
			return
		case <-time.After(time.Second):
		}
		if a.isBusy() {
			continue
		}
		a.liveMu.Lock()
		live, connected, zeroAt, launchAt := a.live, a.connected, a.zeroAt, a.launchAt
		a.liveMu.Unlock()
		if live > 0 {
			continue
		}
		if (connected && time.Since(zeroAt) > 5*time.Second) || (!connected && time.Since(launchAt) > grace) {
			log.Printf("no open window, quitting")
			a.Quit()
			return
		}
	}
}

type instanceInfo struct {
	Port int `json:"port"`
	PID  int `json:"pid"`
}

func main() {
	noBrowser := flag.Bool("no-browser", false, "do not open a browser window")
	portFlag := flag.Int("port", 0, "port (default 17380, or next free)")
	flag.Parse()
	if f := os.Getenv("SITEPULSE_FAKE_API"); f != "" { // testing only
		googleAuthURL, googleTokenURL, googleRevokeURL = f+"/o/oauth2/v2/auth", f+"/token", f+"/revoke"
		googleUserinfoURL, gaAdminBase, gaDataBase = f+"/userinfo", f+"/gaadmin", f+"/gadata"
		gscBase, gscInspect, adsenseBase = f+"/gsc", f+"/inspect", f+"/adsense"
		bingBase, rdapBase, indexNowEndpoint = f+"/bing", f+"/rdap/", f+"/indexnow"
		psiBase = f + "/psi"
	}

	dir := dataDir()
	os.MkdirAll(dir, 0o700)
	if lf, err := os.OpenFile(filepath.Join(dir, "log.txt"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		if st, _ := lf.Stat(); st != nil && st.Size() > 2<<20 {
			lf.Truncate(0)
		}
		log.SetOutput(io.MultiWriter(lf, os.Stderr))
	}
	log.Printf("Site Pulse %s starting, data dir %s", appVersion, dir)

	// single instance: if one is already running, just show its window
	var inst instanceInfo
	if readJSON(filepath.Join(dir, "instance.json"), &inst) == nil && inst.Port > 0 && *portFlag == 0 {
		c := http.Client{Timeout: 2 * time.Second}
		if res, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", inst.Port)); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				log.Printf("already running on port %d", inst.Port)
				if !*noBrowser {
					cmd, _ := openWindow(askForWindow(inst.Port), dir)
					if cmd != nil {
						cmd.Wait()
					}
				}
				return
			}
		}
	}

	store, err := openStore(dir)
	if err != nil {
		log.Fatal(err)
	}
	ln, port := listen(*portFlag)
	if ln == nil {
		log.Fatal("no free port")
	}
	app := &App{store: store, port: port, quit: make(chan struct{}), anyWindow: *noBrowser}
	writeJSON(filepath.Join(dir, "instance.json"), instanceInfo{Port: port, PID: os.Getpid()})

	srv := &http.Server{Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	log.Printf("listening on %s", url)

	go app.scheduler()

	if !*noBrowser {
		cmd, err := openWindow(url+"?w="+app.newWindow(), dir)
		if err != nil {
			log.Printf("open window: %v", err)
		}
		if cmd != nil {
			go func() { cmd.Wait(); log.Printf("window process ended") }()
		}
		// Chrome/Edge often keep running after the window is closed (or hand the window to an
		// existing process), so the browser process tells us nothing: watch the page's live connection.
		go app.quitWhenUIGone(uiGrace())
	}
	<-app.quit
	// let a running collection finish (max 2 min)
	done := make(chan struct{})
	go func() { app.collectMu.Lock(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	os.Remove(filepath.Join(dir, "instance.json"))
	log.Printf("bye")
}

// askForWindow asks the running Site Pulse for a new window address (which replaces its
// current window). Older versions don't know the request: fall back to the plain address.
func askForWindow(port int) string {
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	req, _ := http.NewRequest("POST", base+"api/window", nil)
	req.Header.Set("X-Site-Pulse", "1")
	c := http.Client{Timeout: 3 * time.Second}
	res, err := c.Do(req)
	if err != nil {
		return base
	}
	defer res.Body.Close()
	var v struct {
		URL string `json:"url"`
	}
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&v) != nil || v.URL == "" {
		return base
	}
	return v.URL
}

func listen(p int) (net.Listener, int) {
	if p > 0 {
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			log.Fatal(err)
		}
		return ln, p
	}
	for port := defaultPort; port < defaultPort+50; port++ {
		if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			return ln, port
		}
	}
	return nil, 0
}

// scheduler collects once a day after the configured time (and on start if today's run is missing).
func (a *App) scheduler() {
	time.Sleep(3 * time.Second)
	for {
		a.maybeCollect()
		select {
		case <-a.quit:
			return
		case <-time.After(time.Minute):
		}
	}
}

func (a *App) maybeCollect() {
	a.store.mu.Lock()
	cfg := a.store.Config
	a.store.mu.Unlock()
	if len(cfg.Sites) == 0 {
		return
	}
	now := time.Now()
	hh, mm := 9, 0
	fmt.Sscanf(cfg.ScheduleTime, "%d:%d", &hh, &mm)
	due := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if now.Before(due) {
		return
	}
	if _, err := a.store.loadRun(dateStr(now)); err == nil {
		return
	}
	log.Printf("scheduled collection")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := a.Collect(ctx); err != nil {
		log.Printf("collect: %v", err)
	}
}

// openWindow opens url in a Chromium app-mode window with its own profile.
// It returns the started command when the window can be waited on.
func openWindow(url, dir string) (*exec.Cmd, error) {
	allowForeground()
	b := findBrowser()
	usedBrowser = b
	if b != "" {
		// one profile folder per browser (Chrome and Edge profiles must not be mixed)
		name := strings.ToLower(strings.TrimSuffix(filepath.Base(b), filepath.Ext(b)))
		name = strings.ReplaceAll(name, " ", "-")
		profile := filepath.Join(dir, "Browser-"+name)
		cmd := exec.Command(b, "--app="+url, "--user-data-dir="+profile,
			"--no-first-run", "--no-default-browser-check", "--window-size=1280,880")
		// no hideConsole here: Chrome is a GUI program, and on Windows a "hide" start flag makes
		// a freshly started Chrome open its first window invisible (the "click twice" bug)
		if err := cmd.Start(); err == nil {
			return cmd, nil
		}
	}
	return nil, openDefault(url)
}

// uiGrace: hidden windows throttle timers to about once a minute, so allow 2 minutes of silence.
func uiGrace() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("SITEPULSE_UI_GRACE")); err == nil { // testing only
		return d
	}
	return 2 * time.Minute
}
