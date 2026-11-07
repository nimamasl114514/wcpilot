// dwm.go —— 窗口几何三口径统一（DWM 可见框 / 整窗 / 客户区）+ DPI 感知诊断
//
// 背景（2026-10-11 实测 @150% 缩放屏，wcpilot v7）：
//
//	· GetWindowRect 含 DWM 不可见 resize 边框——记事本实测左/右/下各多 7px、无边框 tkinter 窗多 11px。
//	  拿它当"窗口"口径 → 截图多黑边、窗口内坐标整体偏左上，find_*/OCR 命中随之偏移。
//	· DwmGetWindowAttribute(DWMWA_EXTENDED_FRAME_BOUNDS) 返回用户真正看得见的范围（物理像素），
//	  才是"窗口"该有的口径。
//	· DPI 未感知（unaware）进程的窗口在 150% 屏上，PrintWindow 只绘制逻辑尺寸内容，
//	  物理画布右下留大片纯黑（实测 600x450 画布内容仅 400x300）——见 shot.go normalizeDpiCapture。
package main

import (
	"syscall"
	"unsafe"
)

var (
	dwmapi = syscall.NewLazyDLL("dwmapi.dll")

	prDwmGetWindowAttribute  = dwmapi.NewProc("DwmGetWindowAttribute")
	prMonitorFromWindow      = user32.NewProc("MonitorFromWindow")
	prGetDpiForMonitor       = shcoreDpi.NewProc("GetDpiForMonitor")
	prGetWindowDpiCtx        = user32.NewProc("GetWindowDpiAwarenessContext")
	prGetAwarenessFromDpiCtx = user32.NewProc("GetAwarenessFromDpiAwarenessContext")
	prGetDpiForWindow        = user32.NewProc("GetDpiForWindow")
)

const dwmwaExtendedFrameBounds = 9

// DPI_AWARENESS 枚举（GetAwarenessFromDpiAwarenessContext 返回值）
const (
	dpiAwarenessUnaware     = 0
	dpiAwarenessSystemAware = 1
	dpiAwarenessPerMonitor  = 2
)

// extendedFrameBounds 取窗口「可见内容」矩形（物理像素）。
// 失败（DWM 关闭 / 老系统 / 无效 hwnd）回退 GetWindowRect；第二返回值 = 是否命中 DWM。
func extendedFrameBounds(hwnd uintptr) (RECT, bool) {
	var rc RECT
	r, _, _ := prDwmGetWindowAttribute.Call(hwnd, uintptr(dwmwaExtendedFrameBounds),
		uintptr(unsafe.Pointer(&rc)), unsafe.Sizeof(rc))
	if r != 0 || rc.Right <= rc.Left || rc.Bottom <= rc.Top {
		return windowRect(hwnd), false
	}
	return rc, true
}

// visibleRect 窗口口径统一入口：优先 DWM 可见框，失败回退整窗矩形。
func visibleRect(hwnd uintptr) RECT {
	rc, _ := extendedFrameBounds(hwnd)
	return rc
}

// clientRect 客户区（窗口自身坐标系，不做屏幕换算）
func clientRect(hwnd uintptr) RECT {
	var rc RECT
	prGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	return rc
}

// windowDpiInfo 目标窗口 DPI 诊断：感知级别 + DPI + 缩放比（wininfo 与截图诊断共用）
func windowDpiInfo(hwnd uintptr) map[string]interface{} {
	names := map[uintptr]string{
		dpiAwarenessUnaware:     "unaware",
		dpiAwarenessSystemAware: "system_aware",
		dpiAwarenessPerMonitor:  "per_monitor_aware",
	}
	ctx, _, _ := prGetWindowDpiCtx.Call(hwnd)
	aw, _, _ := prGetAwarenessFromDpiCtx.Call(ctx)
	dpi, _, _ := prGetDpiForWindow.Call(hwnd)
	out := map[string]interface{}{"awareness": names[aw]}
	if dpi == 0 { // 老系统无 GetDpiForWindow
		out["awareness"] = "unknown"
		return out
	}
	out["dpi"] = int(dpi)
	out["scale"] = float64(dpi) / 96.0
	return out
}

// monitorDpi 目标窗口所在显示器的实际 DPI（MDT_EFFECTIVE_DPI）。
// 对 DPI 未感知窗口而言，这才是它被系统放大渲染的真实倍率来源（GetDpiForWindow 会恒返回 96）。
func monitorDpi(hwnd uintptr) int {
	hmon, _, _ := prMonitorFromWindow.Call(hwnd, 2) // MONITOR_DEFAULTTONEAREST
	if hmon == 0 {
		return 96
	}
	var x, y uint32
	r, _, _ := prGetDpiForMonitor.Call(hmon, 0, // MDT_EFFECTIVE_DPI
		uintptr(unsafe.Pointer(&x)), uintptr(unsafe.Pointer(&y)))
	if r != 0 || x == 0 {
		return 96
	}
	return int(x)
}

// cmdWinInfo 几何诊断子命令：一次给出全部口径，用于排障「截图/点击偏了多少」
func cmdWinInfo(hwndFlag uint, titleFlag string) {
	hwnd, _, has := resolveTarget(hwndFlag, titleFlag)
	if !has {
		die("wininfo 需要 --hwnd / --title / 先 target 锁定")
	}
	win := windowRect(hwnd)
	vis, dwmOK := extendedFrameBounds(hwnd)
	cli := clientRect(hwnd)
	info := map[string]interface{}{
		"ok": true, "hwnd": hwnd, "title": windowText(hwnd), "class": className(hwnd),
		"window_rect":  rectMap(win), // GetWindowRect：含不可见边框
		"frame_bounds": rectMap(vis), // DWM 可见框：截图/坐标应用口径
		"dwm_ok":       dwmOK,
		"client_rect":  rectMap(cli),
		"monitor_dpi":  monitorDpi(hwnd),
		"inset": map[string]int{ // 整窗相对可见框多出来的不可见边框（像素）
			"left":   int(vis.Left - win.Left),
			"top":    int(vis.Top - win.Top),
			"right":  int(win.Right - vis.Right),
			"bottom": int(win.Bottom - vis.Bottom),
		},
	}
	for k, v := range windowDpiInfo(hwnd) {
		info[k] = v
	}
	for k, v := range winStateInfo(hwnd) {
		info[k] = v
	}
	if isIconic(hwnd) {
		info["warning"] = "窗口已最小化：window_rect/frame_bounds 是停放坐标（大负数），不代表可见位置"
	}
	jout(info)
}
