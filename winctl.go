// winctl.go —— wcpilot 窗口控制扩展（v3）：winctl 移动/缩放/最大最小化/置顶 + get_pixel 读像素
// winctl 作用于「窗口本身」（位置/尺寸/显示状态/Z 序），不触碰屏幕内容，天然无坐标误触面；
// get_pixel 复用统一坐标防误触语义（target/--hwnd 锁定时 = 窗口内坐标，越界强校验拒绝）。
package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	prSetWindowPos   = user32.NewProc("SetWindowPos")
	prGetPixel       = gdi32.NewProc("GetPixel")
	prIsZoomed       = user32.NewProc("IsZoomed")
	prGetWindowLongW = user32.NewProc("GetWindowLongW")

	hwndTopmost   = ^uintptr(0)     // HWND_TOPMOST   = (HWND)-1
	hwndNotopmost = ^uintptr(0) - 1 // HWND_NOTOPMOST = (HWND)-2
)

const (
	swMaximize = 3
	swMinimize = 6
	swRestore  = 9

	swpNosize     = 0x0001
	swpNomove     = 0x0002
	swpNozorder   = 0x0004
	swpNoactivate = 0x0010

	wsExTopmost = 0x00000008
	clrInvalid  = 0xFFFFFFFF
)

// parseRectSpec 解析 "x,y,w,h"；w/h 必须为正，x/y 允许多显示器负坐标
func parseRectSpec(s string) (int, int, int, int, error) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	if len(parts) != 4 {
		return 0, 0, 0, 0, fmt.Errorf("rect 格式应为 x,y,w,h（收到 %q）", s)
	}
	vals := make([]int, 4)
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0, 0, 0, 0, fmt.Errorf("rect 分量 %q 不是整数", p)
		}
		vals[i] = v
	}
	if vals[2] <= 0 || vals[3] <= 0 {
		return 0, 0, 0, 0, fmt.Errorf("rect 宽高必须为正（收到 %dx%d）", vals[2], vals[3])
	}
	return vals[0], vals[1], vals[2], vals[3], nil
}

// winStateInfo 查询窗口显示状态（最大化/最小化/置顶/前台）
func winStateInfo(hwnd uintptr) map[string]interface{} {
	neg20 := int32(-20) // GWL_EXSTYLE；常量负值无法直接转 uintptr，经变量符号扩展
	exstyle, _, _ := prGetWindowLongW.Call(hwnd, uintptr(neg20))
	zoomed, _, _ := prIsZoomed.Call(hwnd)
	fg, _, _ := prGetForegroundWindow.Call()
	return map[string]interface{}{
		"is_max":        zoomed != 0,
		"is_min":        isIconic(hwnd),
		"is_top":        exstyle&wsExTopmost != 0,
		"is_foreground": fg == hwnd,
	}
}

// winctlCore 容错版窗口控制（CLI 与 batch 共用）；rectStr/state 同时为空 = 仅查询
func winctlCore(hwnd uintptr, rectStr, state string) (map[string]interface{}, error) {
	before := windowRect(hwnd)
	var actions []string
	if rectStr != "" {
		x, y, w, h, err := parseRectSpec(rectStr)
		if err != nil {
			return nil, err
		}
		if zoomed, _, _ := prIsZoomed.Call(hwnd); zoomed != 0 {
			prShowWindow.Call(hwnd, swRestore) // 最大化下 SetWindowPos 移动无效，先还原
			time.Sleep(80 * time.Millisecond)
		}
		r, _, e := prSetWindowPos.Call(hwnd, 0, uintptr(int32(x)), uintptr(int32(y)), uintptr(w), uintptr(h), swpNozorder|swpNoactivate)
		if r == 0 {
			return nil, fmt.Errorf("SetWindowPos 移动/缩放失败: %v", e)
		}
		actions = append(actions, fmt.Sprintf("rect=%d,%d,%d,%d", x, y, w, h))
	}
	if state != "" {
		st := strings.ToLower(strings.TrimSpace(state))
		switch st {
		case "max":
			prShowWindow.Call(hwnd, swMaximize)
		case "min":
			prShowWindow.Call(hwnd, swMinimize)
		case "normal":
			prShowWindow.Call(hwnd, swRestore)
		case "top", "notop":
			z := hwndTopmost
			if st == "notop" {
				z = hwndNotopmost
			}
			r, _, e := prSetWindowPos.Call(hwnd, z, 0, 0, 0, 0, swpNomove|swpNosize|swpNoactivate)
			if r == 0 {
				return nil, fmt.Errorf("SetWindowPos 置顶切换失败: %v", e)
			}
		default:
			return nil, fmt.Errorf("未知 state %q（支持 max|min|normal|top|notop）", state)
		}
		actions = append(actions, "state="+st)
	}
	time.Sleep(60 * time.Millisecond)
	info := map[string]interface{}{
		"hwnd":   hwnd,
		"title":  windowText(hwnd),
		"before": rectMap(before),
		"after":  rectMap(windowRect(hwnd)),
	}
	for k, v := range winStateInfo(hwnd) {
		info[k] = v
	}
	if len(actions) > 0 {
		info["actions"] = actions
	}
	return info, nil
}

// getPixelCore 读屏幕实际显示像素（屏幕 DC）；被遮挡窗口读到的是遮挡物颜色，
// 需读窗口自身内容像素请用 shot/find_color 通道（PrintWindow 可穿遮挡）
func getPixelCore(x, y int) (map[string]interface{}, error) {
	dc, _, _ := prGetDC.Call(0)
	if dc == 0 {
		return nil, fmt.Errorf("GetDC(0) 失败")
	}
	cr, _, _ := prGetPixel.Call(dc, uintptr(int32(x)), uintptr(int32(y)))
	prReleaseDC.Call(0, dc)
	if cr == clrInvalid {
		return nil, fmt.Errorf("GetPixel(%d,%d) 失败（坐标可能超出屏幕）", x, y)
	}
	r := int(cr & 0xFF)
	g := int((cr >> 8) & 0xFF)
	b := int((cr >> 16) & 0xFF)
	return map[string]interface{}{
		"screen_x": x, "screen_y": y,
		"r": r, "g": g, "b": b,
		"hex": fmt.Sprintf("#%02X%02X%02X", r, g, b),
	}, nil
}

// cmdWinctl CLI 层（die 化包装）：必须解析到窗口（--hwnd / --title / target 锁定）
func cmdWinctl(hwndFlag uint, titleFlag, rectStr, state string) {
	hwnd, _, has := resolveTarget(hwndFlag, titleFlag)
	if !has {
		die("winctl 需要目标窗口：--hwnd / --title / 先 target 锁定")
	}
	info, err := winctlCore(hwnd, rectStr, state)
	if err != nil {
		die("%v", err)
	}
	info["ok"] = true
	jout(info)
}

// cmdGetPixel CLI 层：复用统一坐标防误触语义（窗口锁定=窗口内坐标越界强校验）
func cmdGetPixel(hwndFlag uint, titleFlag string, x, y int) {
	sx, sy, hwnd, err := tryCoords(hwndFlag, titleFlag, x, y)
	if err != nil {
		die("%v", err)
	}
	info, err := getPixelCore(sx, sy)
	if err != nil {
		die("%v", err)
	}
	info["ok"] = true
	if hwnd != 0 {
		info["hwnd"] = hwnd
		info["win_x"] = x
		info["win_y"] = y
	}
	jout(info)
}
