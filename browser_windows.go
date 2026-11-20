//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// appPath reads HKCU/HKLM ...\App Paths\<exe> (where installers register the real location).
func appPath(exe string) []string {
	var out []string
	key := `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\` + exe
	for _, root := range []syscall.Handle{syscall.HKEY_CURRENT_USER, syscall.HKEY_LOCAL_MACHINE} {
		var h syscall.Handle
		kp, _ := syscall.UTF16PtrFromString(key)
		if syscall.RegOpenKeyEx(root, kp, 0, syscall.KEY_READ|0x0100 /*WOW64_64KEY*/, &h) != nil {
			continue
		}
		var typ, n uint32
		buf := make([]uint16, 1024)
		n = uint32(len(buf) * 2)
		if syscall.RegQueryValueEx(h, nil, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) == nil {
			p := strings.Trim(syscall.UTF16ToString(buf), `"`)
			if p != "" {
				out = append(out, os.ExpandEnv(p))
			}
		}
		syscall.RegCloseKey(h)
	}
	return out
}

func findBrowser() string {
	var cands []string
	// Chrome first: registered location, then the usual install folders, then PATH
	cands = append(cands, appPath("chrome.exe")...)
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData", "ProgramW6432"} {
		if d := os.Getenv(env); d != "" {
			cands = append(cands, filepath.Join(d, `Google\Chrome\Application\chrome.exe`))
		}
	}
	if p, err := exec.LookPath("chrome.exe"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, appPath("msedge.exe")...)
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		if d := os.Getenv(env); d != "" {
			cands = append(cands, filepath.Join(d, `Microsoft\Edge\Application\msedge.exe`))
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func openDefault(url string) error {
	// no hideConsole: the hide flag would be passed on to a browser started fresh
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

var procAllowSetForegroundWindow = syscall.NewLazyDLL("user32.dll").NewProc("AllowSetForegroundWindow")

// allowForeground lets the browser we start bring its window to the front. Without it, a slow
// Chrome start can open the window behind Explorer and the first click looks like nothing happened.
func allowForeground() {
	const asfwAny = 0xFFFFFFFF // ASFW_ANY
	procAllowSetForegroundWindow.Call(uintptr(asfwAny))
}
