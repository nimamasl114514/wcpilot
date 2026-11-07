package main

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	prEnumWindows              = user32.NewProc("EnumWindows")
	prGetWindowTextW           = user32.NewProc("GetWindowTextW")
	prGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
	prGetClassNameW            = user32.NewProc("GetClassNameW")
	prIsWindowVisible          = user32.NewProc("IsWindowVisible")
	prIsWindow                 = user32.NewProc("IsWindow")
	prIsIconic                 = user32.NewProc("IsIconic")
	prGetWindowRect            = user32.NewProc("GetWindowRect")
	prGetClientRect            = user32.NewProc("GetClientRect")
	prSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	prGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	prShowWindow               = user32.NewProc("ShowWindow")
	prBringWindowToTop         = user32.NewProc("BringWindowToTop")
	prGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	prAttachThreadInput        = user32.NewProc("AttachThreadInput")
	prGetCurrentThreadId       = kernel32.NewProc("GetCurrentThreadId")
	prSetCursorPos             = user32.NewProc("SetCursorPos")
	prGetCursorPos             = user32.NewProc("GetCursorPos")
	prSendInput                = user32.NewProc("SendInput")
	prGetDC                    = user32.NewProc("GetDC")
	prGetWindowDC              = user32.NewProc("GetWindowDC")
	prReleaseDC                = user32.NewProc("ReleaseDC")
	prPrintWindow              = user32.NewProc("PrintWindow")
	prGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	prCreateCompatibleDC       = gdi32.NewProc("CreateCompatibleDC")
	prCreateCompatibleBitmap   = gdi32.NewProc("CreateCompatibleBitmap")
	prSelectObject             = gdi32.NewProc("SelectObject")
	prDeleteObject             = gdi32.NewProc("DeleteObject")
	prDeleteDC                 = gdi32.NewProc("DeleteDC")
	prBitBlt                   = gdi32.NewProc("BitBlt")
	prGetDIBits                = gdi32.NewProc("GetDIBits")
	prOpenClipboard            = user32.NewProc("OpenClipboard")
	prCloseClipboard           = user32.NewProc("CloseClipboard")
	prEmptyClipboard           = user32.NewProc("EmptyClipboard")
	prSetClipboardData         = user32.NewProc("SetClipboardData")
	prGetClipboardData         = user32.NewProc("GetClipboardData")
	prGlobalAlloc              = kernel32.NewProc("GlobalAlloc")
	prGlobalLock               = kernel32.NewProc("GlobalLock")
	prGlobalUnlock             = kernel32.NewProc("GlobalUnlock")
	prVkKeyScanW               = user32.NewProc("VkKeyScanW")
	prMapVirtualKeyW           = user32.NewProc("MapVirtualKeyW")
	prPostMessageW             = user32.NewProc("PostMessageW")
	prGetKeyState              = user32.NewProc("GetKeyState")
	prChildWindowFromPointEx   = user32.NewProc("ChildWindowFromPointEx")
	prGetGUIThreadInfo         = user32.NewProc("GetGUIThreadInfo")
	prGetWindowPlacement       = user32.NewProc("GetWindowPlacement")
	prSetWindowPlacement       = user32.NewProc("SetWindowPlacement")
	prEnumChildWindows         = user32.NewProc("EnumChildWindows")
	prGetParent                = user32.NewProc("GetParent")
	prGetDlgCtrlID             = user32.NewProc("GetDlgCtrlID")
	prIsWindowEnabled          = user32.NewProc("IsWindowEnabled")
	prGetWindowLongPtrW        = user32.NewProc("GetWindowLongPtrW")
)

// cwpSkipAll = CWP_SKIPINVISIBLE|CWP_SKIPDISABLED|CWP_SKIPTRANSPARENT
const cwpSkipAll = 0x0001 | 0x0002 | 0x0008

// GUITHREADINFO（x64 布局：cbSize/flags 各 4B + 6 个 HWND + RECT）
type guiThreadInfo struct {
	cbSize      uint32
	flags       uint32
	hwndActive  uintptr
	hwndFocus   uintptr
	hwndCapture uintptr
	hwndMenuOw  uintptr
	hwndMoveSz  uintptr
	hwndCaret   uintptr
	rcCaret     RECT
}

type RECT struct{ Left, Top, Right, Bottom int32 }
type POINT struct{ X, Y int32 }

func rectMap(r RECT) map[string]interface{} {
	return map[string]interface{}{
		"left": r.Left, "top": r.Top, "right": r.Right, "bottom": r.Bottom,
		"width": r.Right - r.Left, "height": r.Bottom - r.Top,
	}
}

type WinInfo struct {
	Hwnd  uintptr `json:"hwnd"`
	Title string  `json:"title"`
	Class string  `json:"class"`
	Pid   uint32  `json:"pid"`
	Rect  RECT    `json:"rect"`
}

func isVisible(hwnd uintptr) bool {
	r, _, _ := prIsWindowVisible.Call(hwnd)
	return r != 0
}

func isIconic(hwnd uintptr) bool {
	r, _, _ := prIsIconic.Call(hwnd)
	return r != 0
}

func isWindow(hwnd uintptr) bool {
	r, _, _ := prIsWindow.Call(hwnd)
	return r != 0
}

func isEnabled(hwnd uintptr) bool {
	r, _, _ := prIsWindowEnabled.Call(hwnd)
	return r != 0
}

func windowText(hwnd uintptr) string {
	n, _, _ := prGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	prGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return syscall.UTF16ToString(buf)
}

func className(hwnd uintptr) string {
	buf := make([]uint16, 256)
	prGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
	return syscall.UTF16ToString(buf)
}

func windowRect(hwnd uintptr) RECT {
	var rc RECT
	prGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	return rc
}

// windowPlacement 对应 Win32 WINDOWPLACEMENT（x64 44 字节）
type windowPlacement struct {
	Length   uint32
	Flags    uint32
	ShowCmd  uint32
	PtMin    POINT
	PtMax    POINT
	RcNormal RECT
}

// restoreFromMinimized 恢复最小化窗口（ctypes 四变体探针实锤）：
// SW_RESTORE / SetWindowPlacement(r=0 直接失败) / SW_SHOWNORMAL 全对 Avalonia 类自窗口状态管理应用无效，
// 唯一有效路径 = SendMessage/PostMessage(WM_SYSCOMMAND, SC_RESTORE)——走应用自己的「还原」入口，
// 应用内部窗口状态才会同步（本机 Avalonia 例：ShowWindow(1) 甚至会被应用立即改回最小化）。
// 用 PostMessage 防火方/卡顿线程挂住 wcpilot；是否真的还原由调用方等待后复查 IsIconic 判定。
func restoreFromMinimized(hwnd uintptr) {
	prPostMessageW.Call(hwnd, 0x112, 0xF120, 0) // WM_SYSCOMMAND / SC_RESTORE
}

func enumWindows() []WinInfo {
	var list []WinInfo
	cb := syscall.NewCallback(func(hwnd, lparam uintptr) uintptr {
		if !isVisible(hwnd) {
			return 1
		}
		t := windowText(hwnd)
		if t == "" {
			return 1
		}
		var pid uint32
		prGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		list = append(list, WinInfo{hwnd, t, className(hwnd), pid, windowRect(hwnd)})
		return 1
	})
	prEnumWindows.Call(cb, 0)
	return list
}

func findWindowByTitle(keyword string) (uintptr, bool) {
	for _, w := range enumWindows() {
		if containsFold(w.Title, keyword) || containsFold(w.Class, keyword) {
			return w.Hwnd, true
		}
	}
	return 0, false
}

func activateWindow(hwnd uintptr) {
	if isIconic(hwnd) {
		prShowWindow.Call(hwnd, 9) // SW_RESTORE
		time.Sleep(150 * time.Millisecond)
	}
	fg, _, _ := prGetForegroundWindow.Call()
	var fgThread uint32
	if fg != 0 {
		tid, _, _ := prGetWindowThreadProcessId.Call(fg, uintptr(unsafe.Pointer(&fgThread)))
		fgThread = uint32(tid)
	}
	cur, _, _ := prGetCurrentThreadId.Call()
	attached := false
	if fgThread != 0 && fgThread != uint32(cur) {
		r, _, _ := prAttachThreadInput.Call(cur, uintptr(fgThread), 1)
		attached = r != 0
	}
	prBringWindowToTop.Call(hwnd)
	prSetForegroundWindow.Call(hwnd)
	if attached {
		prAttachThreadInput.Call(cur, uintptr(fgThread), 0)
	}
	time.Sleep(120 * time.Millisecond)
}

func cmdWindows(keyword string) {
	var out []WinInfo
	for _, w := range enumWindows() {
		if keyword == "" || containsFold(w.Title, keyword) || containsFold(w.Class, keyword) {
			out = append(out, w)
		}
	}
	if out == nil {
		out = []WinInfo{}
	}
	jout(map[string]interface{}{"ok": true, "count": len(out), "windows": out})
}
