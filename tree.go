// tree.go —— 控件树（v8 新增）：不靠 OCR 就能定位「哪个子控件、什么类名、在哪」
//
// 为什么需要它：find_text 要 OCR（慢），find_color 受主题色干扰；而绝大多数原生控件
// （Edit / Button / SysListView32 / RichEdit）本身就有类名、文本和矩形。
// 拿到控件树后可以：先按类名+文本缩小范围 → 只对那一小块做 OCR/截图，
// 或者直接用控件相对坐标点击——又快又稳。
//
// 局限（写在输出里）：这是 Win32 子窗口树，不是 UI Automation。
// WPF / UWP / Electron / Chrome 的内部元素树拿不到（它们是自绘），
// 这类程序只能看到顶层窗口 + 少数宿主子窗口，仍需 OCR/找图兜底。
package main

import (
	"sort"
	"strings"
	"syscall"
)

// WinNode 控件树节点
type WinNode struct {
	Hwnd    uintptr `json:"hwnd"`
	Class   string  `json:"class"`
	Text    string  `json:"text"`
	Depth   int     `json:"depth"`
	CtrlID  int     `json:"ctrl_id,omitempty"`
	X       int     `json:"x"` // 屏幕绝对坐标（相对整个虚拟屏）
	Y       int     `json:"y"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Visible bool    `json:"visible"`
	Enabled bool    `json:"enabled"`
}

// depthOf 从 root 往下数到 hwnd 的层数（EnumChildWindows 给的是全部后代，层数要自己算）
func depthOf(root, hwnd uintptr) int {
	d := 0
	cur := hwnd
	for i := 0; i < 32 && cur != 0 && cur != root; i++ {
		p, _, _ := prGetParent.Call(cur)
		if p == 0 {
			break
		}
		cur = p
		d++
	}
	return d
}

func ctrlID(hwnd uintptr) int {
	r, _, _ := prGetDlgCtrlID.Call(hwnd)
	return int(int32(r))
}

// collectChildren 枚举 root 的全部后代，按 depth 截断
func collectChildren(root uintptr, maxDepth int) []WinNode {
	var out []WinNode
	cb := syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		d := depthOf(root, hwnd)
		if maxDepth > 0 && d > maxDepth {
			return 1
		}
		rc := windowRect(hwnd)
		n := WinNode{
			Hwnd:    hwnd,
			Class:   className(hwnd),
			Text:    windowText(hwnd),
			Depth:   d,
			CtrlID:  ctrlID(hwnd),
			X:       int(rc.Left),
			Y:       int(rc.Top),
			W:       int(rc.Right - rc.Left),
			H:       int(rc.Bottom - rc.Top),
			Visible: isVisible(hwnd),
			Enabled: isEnabled(hwnd),
		}
		out = append(out, n)
		return 1
	})
	prEnumChildWindows.Call(root, cb, 0)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Y != out[j].Y {
			return out[i].Y < out[j].Y
		}
		return out[i].X < out[j].X
	})
	return out
}

// cmdWinTree 打印控件树；--text/--class 过滤（命中节点保留，但深度照常标注）
func cmdWinTree(hwndFlag uint, titleFlag, textFilter, classFilter string, maxDepth int) {
	hwnd, rc, has := resolveTarget(hwndFlag, titleFlag)
	if !has {
		die("win_tree 需要目标窗口：--hwnd / --title / 先 target 锁定")
	}
	root := WinNode{
		Hwnd: hwnd, Class: className(hwnd), Text: windowText(hwnd), Depth: 0,
		CtrlID: ctrlID(hwnd),
		X:      int(rc.Left), Y: int(rc.Top),
		W: int(rc.Right - rc.Left), H: int(rc.Bottom - rc.Top),
		Visible: isVisible(hwnd), Enabled: isEnabled(hwnd),
	}
	nodes := collectChildren(hwnd, maxDepth)
	var kept []WinNode
	for _, n := range nodes {
		if textFilter != "" && !containsFold(n.Text, textFilter) && !containsFold(n.Class, textFilter) {
			continue
		}
		if classFilter != "" && !containsFold(n.Class, classFilter) {
			continue
		}
		kept = append(kept, n)
	}
	if kept == nil {
		kept = []WinNode{}
	}
	jout(map[string]interface{}{
		"ok": true, "root": root, "count": len(kept), "total_children": len(nodes),
		"nodes": kept,
		"hint": "坐标为屏幕绝对坐标；要点击某控件，用 click --x (x-left) --y (y-top) 配合 --hwnd 目标窗口的可见框原点，" +
			"或直接 click --hwnd <窗口> 并传入相对窗口可见框的坐标",
		"note": "Win32 子窗口树；WPF/UWP/Electron 自绘界面内部的元素不在此列",
	})
}

// findChildByText 供 --bg 点击前的辅助定位（暂未接入 CLI，保留给 batch 扩展）
func findChildByText(root uintptr, needle string, maxDepth int) (WinNode, bool) {
	for _, n := range collectChildren(root, maxDepth) {
		if n.Visible && n.W > 0 && n.H > 0 && strings.Contains(strings.ToLower(n.Text), strings.ToLower(needle)) {
			return n, true
		}
	}
	return WinNode{}, false
}
