// dpi.go —— 启动即声明 per-monitor DPI 感知（v5 修复：高分屏下截不到整屏）
//
// 现象：150% 缩放屏（物理 2560x1600）上 `wcpilot shot` 只出 1707x1067 的图，
// 内容是屏幕左上角一块，右侧约 33% 与底部（含任务栏）整块丢失。
//
// 根因：Go 可执行文件默认无 manifest → 进程被判为 DPI 未感知（dpi unaware），
// GetSystemMetrics(SM_CXSCREEN=0 / SM_CYSCREEN=1) 返回的是「DPI 虚拟化」后的逻辑尺寸，
// 而 GetDC(0) + BitBlt 取到的仍是物理像素；按逻辑尺寸从 (0,0) 拷贝物理像素，
// 等于只截了屏幕左上角 1707x1067 / 2560x1600 ≈ 44% 的面积。
//
// 修法：进程启动最早期声明 per-monitor aware v2，坐标/尺寸/截图统一到物理像素，
// 全屏截图、窗口矩形、find_*/get_pixel 坐标全部自洽（同为物理像素）。
// 三级降级：SetProcessDpiAwarenessContext(v2, Win10 1703+) → SetProcessDpiAwareness(2, Win8.1+)
// → SetProcessDPIAware(Vista+)。

package main

import "syscall"

var (
	user32Dpi   = syscall.NewLazyDLL("user32.dll")
	shcoreDpi   = syscall.NewLazyDLL("shcore.dll")
	prSetDpiCtx = user32Dpi.NewProc("SetProcessDpiAwarenessContext")
	prSetDpiShc = shcoreDpi.NewProc("SetProcessDpiAwareness")
	prSetDpiOld = user32Dpi.NewProc("SetProcessDPIAware")
)

// DPI_AWARENESS_CONTEXT 是负值伪句柄，必须按补码传入（负数参数陷阱）
const (
	dpiCtxUnaware      = ^uintptr(0) // -1
	dpiCtxSystemAware  = ^uintptr(1) // -2
	dpiCtxPerMonitor   = ^uintptr(2) // -3
	dpiCtxPerMonitorV2 = ^uintptr(3) // -4
)

// dpiMode 记录本次实际生效的感知级别（none / system / per_monitor / per_monitor_v2）
var dpiMode string

// enableDpiAwareness 必须在任何窗口/DC/坐标操作之前调用（wcpilot 无窗口，main 首行即可）
func enableDpiAwareness() string {
	if r, _, _ := prSetDpiCtx.Call(dpiCtxPerMonitorV2); r != 0 {
		return "per_monitor_v2"
	}
	if r, _, _ := prSetDpiShc.Call(2); r == 0 { // PROCESS_PER_MONITOR_DPI_AWARE
		return "per_monitor"
	}
	if r, _, _ := prSetDpiOld.Call(); r != 0 {
		return "system"
	}
	return "none"
}
