// target.go —— 目标窗口锁定（状态文件）与统一解析
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type TargetState struct {
	Hwnd  uintptr `json:"hwnd"`
	Title string  `json:"title"`
}

func targetPath() string {
	return filepath.Join(os.TempDir(), "wcpilot_target.json")
}

func saveTarget(t TargetState) error {
	b, _ := json.Marshal(t)
	return os.WriteFile(targetPath(), b, 0644)
}

func loadTarget() (TargetState, bool) {
	var t TargetState
	b, err := os.ReadFile(targetPath())
	if err != nil {
		return t, false
	}
	if json.Unmarshal(b, &t) != nil || t.Hwnd == 0 {
		return t, false
	}
	return t, true
}

func clearTarget() { os.Remove(targetPath()) }

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// resolveTargetSafe 不退出进程的窗口解析：
// --hwnd 显式 > --title 关键字 > target 锁定状态；has=false 表示「屏幕模式」。
// v8：batch 里每个 job 可以各自指定窗口，解析失败必须是返回值而不是直接退出进程。
func resolveTargetSafe(hwndFlag uint, titleFlag string) (uintptr, RECT, bool) {
	if hwndFlag != 0 {
		hwnd := uintptr(hwndFlag)
		if !isWindow(hwnd) {
			return 0, RECT{}, false
		}
		return hwnd, visibleRect(hwnd), true
	}
	if titleFlag != "" {
		if hwnd, ok := findWindowByTitle(titleFlag); ok {
			return hwnd, visibleRect(hwnd), true
		}
		return 0, RECT{}, false
	}
	if t, ok := loadTarget(); ok {
		if isWindow(t.Hwnd) {
			return t.Hwnd, visibleRect(t.Hwnd), true
		}
		clearTarget()
	}
	return 0, RECT{}, false
}

// resolveTarget 统一窗口解析（CLI 层）：显式指定的窗口找不到 → 直接报错退出。
// 返回的 rect 为 DWM 可见框（EXTENDED_FRAME_BOUNDS）——窗口内坐标 (0,0) = 用户看得见的左上角，
// 不含 DWM 不可见 resize 边框（旧口径实测偏 7-11px）。
func resolveTarget(hwndFlag uint, titleFlag string) (uintptr, RECT, bool) {
	hwnd, rc, has := resolveTargetSafe(hwndFlag, titleFlag)
	if has {
		return hwnd, rc, true
	}
	if hwndFlag != 0 {
		die("句柄 %d 不是有效窗口（可能已关闭）", hwndFlag)
	}
	if titleFlag != "" {
		die("找不到标题含 %q 的窗口，先用 windows 子命令查看", titleFlag)
	}
	return 0, RECT{}, false
}

func mustResolve(hwndFlag uint, titleFlag string) (uintptr, RECT, bool) {
	return resolveTarget(hwndFlag, titleFlag)
}

func cmdTarget(hwndFlag uint, titleFlag string, clear bool) {
	if clear {
		clearTarget()
		jout(map[string]interface{}{"ok": true, "cleared": true})
		return
	}
	if hwndFlag == 0 && titleFlag == "" {
		if t, ok := loadTarget(); ok && isWindow(t.Hwnd) {
			jout(map[string]interface{}{"ok": true, "target": t, "rect": rectMap(visibleRect(t.Hwnd)),
				"state": winStateInfo(t.Hwnd)})
		} else {
			jout(map[string]interface{}{"ok": true, "target": nil})
		}
		return
	}
	hwnd, rc, _ := mustResolve(hwndFlag, titleFlag)
	t := TargetState{hwnd, windowText(hwnd)}
	if err := saveTarget(t); err != nil {
		die("保存目标失败: %v", err)
	}
	rec := map[string]interface{}{"ok": true, "target": t, "rect": rectMap(rc),
		"state": winStateInfo(hwnd),
		"note":  fmt.Sprintf("后续 click/shot/find_* 不带 --hwnd 时将默认锁定窗口 %q", t.Title)}
	if isIconic(hwnd) {
		// 最小化窗口的矩形是系统的「停放坐标」（通常是大负数），不对应任何可见内容
		rec["warning"] = "目标窗口当前已最小化，rect 是停放坐标；首次 shot/click 会自动还原，或先 winctl --state normal"
	}
	jout(rec)
}
