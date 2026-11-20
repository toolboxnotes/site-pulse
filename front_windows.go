//go:build windows

package main

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procIsIconic                 = user32.NewProc("IsIconic")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procMonitorFromWindow        = user32.NewProc("MonitorFromWindow")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procSystemParametersInfoW    = user32.NewProc("SystemParametersInfoW")
	procGetCurrentThreadId       = syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentThreadId")
)

type winRect struct{ Left, Top, Right, Bottom int32 }

// appWindows lists Chromium windows (visible or not) whose title starts with "Site Pulse".
func appWindows() []uintptr {
	var out []uintptr
	cb := syscall.NewCallback(func(h, _ uintptr) uintptr {
		buf := make([]uint16, 256)
		procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
		if syscall.UTF16ToString(buf) != "Chrome_WidgetWin_1" {
			return 1
		}
		procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
		if strings.HasPrefix(syscall.UTF16ToString(buf), "Site Pulse") {
			out = append(out, h)
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return out
}

// bringWindowToFront: Windows sometimes opens the first window behind other windows (the
// "foreground lock"), minimized, or off-screen (window position saved for a monitor that is
// gone). Find our window once its page has connected and make sure it is visible and in front.
// Returns a short description for the log.
func bringWindowToFront() string {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	time.Sleep(700 * time.Millisecond) // let a replaced window close itself first
	var hs []uintptr
	for i := 0; i < 16; i++ {
		hs = appWindows()
		if len(hs) == 1 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(hs) == 0 {
		return "window not found"
	}
	h := hs[len(hs)-1] // lowest in the stacking order = the one hidden behind
	var notes []string
	if v, _, _ := procIsWindowVisible.Call(h); v == 0 {
		procShowWindow.Call(h, 1) // SW_SHOWNORMAL
		notes = append(notes, "was hidden")
	}
	if v, _, _ := procIsIconic.Call(h); v != 0 {
		procShowWindow.Call(h, 9) // SW_RESTORE
		notes = append(notes, "was minimized")
	}
	if m, _, _ := procMonitorFromWindow.Call(h, 0 /*MONITOR_DEFAULTTONULL*/); m == 0 {
		var wa, r winRect
		procSystemParametersInfoW.Call(0x30 /*SPI_GETWORKAREA*/, 0, uintptr(unsafe.Pointer(&wa)), 0)
		procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&r)))
		w, ht := r.Right-r.Left, r.Bottom-r.Top
		if w <= 0 || w > wa.Right-wa.Left {
			w = (wa.Right - wa.Left) * 4 / 5
		}
		if ht <= 0 || ht > wa.Bottom-wa.Top {
			ht = (wa.Bottom - wa.Top) * 4 / 5
		}
		x, y := wa.Left+(wa.Right-wa.Left-w)/2, wa.Top+(wa.Bottom-wa.Top-ht)/2
		procSetWindowPos.Call(h, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(ht), 0x0004 /*SWP_NOZORDER*/)
		notes = append(notes, "was off-screen")
	}
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == h {
		notes = append(notes, "already in front")
		return strings.Join(notes, ", ")
	}
	// SetForegroundWindow is refused while another app holds the foreground lock, unless our
	// thread shares input with the foreground window's thread.
	me, _, _ := procGetCurrentThreadId.Call()
	other, _, _ := procGetWindowThreadProcessId.Call(fg, 0)
	if other != 0 && other != me {
		procAttachThreadInput.Call(me, other, 1)
	}
	procShowWindow.Call(h, 5) // SW_SHOW
	procBringWindowToTop.Call(h)
	procSetForegroundWindow.Call(h)
	if other != 0 && other != me {
		procAttachThreadInput.Call(me, other, 0)
	}
	if now, _, _ := procGetForegroundWindow.Call(); now != h {
		// last resort: minimizing and restoring a window activates it
		procShowWindow.Call(h, 6) // SW_MINIMIZE
		procShowWindow.Call(h, 9) // SW_RESTORE
		notes = append(notes, "used minimize/restore")
	}
	now, _, _ := procGetForegroundWindow.Call()
	notes = append(notes, fmt.Sprintf("in front: %v", now == h))
	return strings.Join(notes, ", ")
}
