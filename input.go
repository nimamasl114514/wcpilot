// input.go —— 输入注入（v8）
//
// 两条通道：
//
//	· 前台：SendInput 注入真实鼠标/键盘（被系统视为物理操作，任何程序都认）
//	· 后台（--bg）：PostMessage 投消息，不激活窗口、不动真实光标、不抢物理键盘
//
// v8 修正（都是实测踩出来的）：
//  1. bg 点击先投 WM_MOUSEMOVE——很多控件只在收到移动后才建立 hover/按下状态，
//     直接投 DOWN 会被静默丢弃；同时补齐右键/中键/双击消息。
//  2. bg 键盘 lParam 补扫描码与扩展位——旧实现 lParam 恒 0，靠 lParam 取扫描码的
//     游戏/自绘控件（以及部分输入法）拿不到键值。
//  3. type 默认保存并恢复剪贴板——旧实现直接把用户剪贴板改成待输入文本，属于静默副作用。
//  4. wheel 支持 --bg（WM_MOUSEWHEEL）与 (0,0) 坐标（旧版把 0,0 当「没给坐标」）。
package main

import (
	"fmt"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

type MOUSEINPUT struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// INPUT x64 布局：Type(4) + pad(4) + union(32，取 MOUSEINPUT/<KEYBDINPUT+padding> 最大者)
type INPUT struct {
	Type uint32
	_    uint32
	Mi   MOUSEINPUT
}

type kINPUT struct {
	Type uint32
	_    uint32
	Ki   KEYBDINPUT
	_    [8]byte
}

const (
	mouseMove     = 0x0001
	mouseLeftDown = 0x0002
	mouseLeftUp   = 0x0004
	mouseRightDwn = 0x0008
	mouseRightUp  = 0x0010
	mouseMidDown  = 0x0020
	mouseMidUp    = 0x0040
	mouseWheel    = 0x0800
	keyUp         = 0x0002

	// 后台鼠标消息（PostMessage 投递）
	wmMouseMove     = 0x0200
	wmLButtonDown   = 0x0201
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonDown   = 0x0204
	wmRButtonUp     = 0x0205
	wmRButtonDblClk = 0x0206
	wmMButtonDown   = 0x0207
	wmMButtonUp     = 0x0208
	wmMouseWheel    = 0x020A

	// 后台键盘消息（PostMessage 投递，不激活窗口、不抢物理键盘焦点）
	wmKeyDown = 0x0100
	wmKeyUp   = 0x0101
	wmChar    = 0x0102

	// 编辑控件命令消息：不依赖键盘状态（GetKeyState），是 bg 通道下剪贴板类操作的可靠路径
	wmCut    = 0x0300
	wmCopy   = 0x0301
	wmPaste  = 0x0302
	wmClear  = 0x0303
	emSetSel = 0x00B1
)

func sendMouse(flags, data uint32) {
	var in INPUT
	in.Type = 0
	in.Mi.DwFlags = flags
	in.Mi.MouseData = data
	prSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendKey(vk uint16, down bool) {
	var in kINPUT
	in.Type = 1
	in.Ki.WVk = vk
	if !down {
		in.Ki.DwFlags = keyUp
	}
	prSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func setCursor(x, y int) { prSetCursorPos.Call(uintptr(int32(x)), uintptr(int32(y))) }

func cursorPos() (int, int) {
	var p POINT
	prGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	return int(p.X), int(p.Y)
}

// mouseButtons 把按钮名映射到 (DnSync 标志, 按下消息, 抬起消息, 双击消息, MK 掩码)
func mouseButtons(btn string) (down, up uint32, bgDown, bgUp, bgDbl uintptr, mk uintptr, ok bool) {
	switch strings.ToLower(strings.TrimSpace(btn)) {
	case "right":
		return mouseRightDwn, mouseRightUp, wmRButtonDown, wmRButtonUp, wmRButtonDblClk, 0x0002, true
	case "middle":
		return mouseMidDown, mouseMidUp, wmMButtonDown, wmMButtonUp, 0, 0x0010, true
	case "", "left":
		return mouseLeftDown, mouseLeftUp, wmLButtonDown, wmLButtonUp, wmLButtonDblClk, 0x0001, true
	}
	return 0, 0, 0, 0, 0, 0, false
}

func clickAt(x, y int, btn string, dbl bool) {
	down, up, _, _, _, _, ok := mouseButtons(btn)
	if !ok {
		down, up = mouseLeftDown, mouseLeftUp
	}
	setCursor(x, y)
	time.Sleep(40 * time.Millisecond)
	n := 1
	if dbl {
		n = 2
	}
	for i := 0; i < n; i++ {
		sendMouse(down, 0)
		time.Sleep(15 * time.Millisecond)
		sendMouse(up, 0)
		time.Sleep(15 * time.Millisecond)
	}
}

// doWheel 前台滚轮：hasXY 显式区分「在 (0,0) 滚」与「不指定位置」，hwnd≠0 时先对齐前台。
func doWheel(delta, x, y int, hasXY bool, hwnd uintptr) {
	if delta == 0 {
		delta = 120
	}
	if hasXY {
		if hwnd != 0 {
			activateWindow(hwnd)
		}
		setCursor(x, y)
		time.Sleep(30 * time.Millisecond)
	} else if hwnd != 0 {
		activateWindow(hwnd)
	}
	sendMouse(mouseWheel, uint32(int32(delta)))
}

// postWheel 后台滚轮：WM_MOUSEWHEEL 的 lParam 是屏幕坐标（不是客户区），wParam 高字 = delta。
func postWheel(hwnd uintptr, delta, screenX, screenY int) (uintptr, string) {
	if delta == 0 {
		delta = 120
	}
	rc := visibleRect(hwnd)
	tgt, _, _, _ := childFromPoint(hwnd, screenX-int(rc.Left), screenY-int(rc.Top))
	wp := uintptr(uint32(uint16(int16(delta))) << 16)
	lp := uintptr(uint32(uint16(int16(screenX))) | uint32(uint16(int16(screenY)))<<16)
	prPostMessageW.Call(tgt, wmMouseWheel, wp, lp)
	return tgt, className(tgt)
}

// postClick 后台点击：PostMessage 不激活窗口、不动真实鼠标。
// 顶层窗口往往不是真正接收鼠标的控件（如记事本编辑区是子窗口），
// 故先用 childFromPoint 逐层下钻到最深子控件，并把坐标换算进该控件坐标系再投递。
// v8：先投 WM_MOUSEMOVE（建立 hover/按下态），支持右/中键与双击。
// 返回 (最终接收控件 hwnd, 控件类名, 下钻层数)，供调用方落痕审计。
func postClick(hwnd uintptr, x, y int, btn string, dbl bool) (uintptr, string, int) {
	tgt, tx, ty, depth := childFromPoint(hwnd, x, y)
	lp := uintptr(uint32(uint16(tx)) | uint32(uint16(ty))<<16)
	_, _, bgDown, bgUp, bgDbl, mk, ok := mouseButtons(btn)
	if !ok {
		mk = 0x0001
	}
	prPostMessageW.Call(tgt, wmMouseMove, 0, lp)
	time.Sleep(8 * time.Millisecond)
	prPostMessageW.Call(tgt, bgDown, mk, lp)
	time.Sleep(20 * time.Millisecond)
	prPostMessageW.Call(tgt, bgUp, 0, lp)
	if dbl && bgDbl != 0 {
		// 双击语义：再补一组 DBLCLK + UP（第一条 down/up 已完成第一次点击）
		time.Sleep(20 * time.Millisecond)
		prPostMessageW.Call(tgt, bgDbl, mk, lp)
		time.Sleep(10 * time.Millisecond)
		prPostMessageW.Call(tgt, bgUp, 0, lp)
	}
	return tgt, className(tgt), depth
}

// childFromPoint 从 hwnd 起按窗口内坐标 (x,y) 逐层下钻到最深可见/可用子控件，
// 返回 (子控件 hwnd, 控件内相对 x, 控件内相对 y, 下钻层数)。无子控件时返回自身。
func childFromPoint(hwnd uintptr, x, y int) (uintptr, int, int, int) {
	cur, cx, cy, depth := hwnd, x, y, 0
	for depth < 16 {
		// ChildWindowFromPointEx: POINT 按值传递（x86-64 打包进一个 64 位寄存器）
		lp := uintptr(uint64(uint32(int32(cx))) | uint64(uint32(int32(cy)))<<32)
		r, _, _ := prChildWindowFromPointEx.Call(cur, lp, cwpSkipAll)
		if r == 0 || r == cur {
			break
		}
		// 子控件坐标 = 父坐标 - 子控件左上角（换算到屏幕坐标差值，兼容 DPI/多屏）
		prc := windowRect(cur)
		crc := windowRect(r)
		cx = cx - int(crc.Left-prc.Left)
		cy = cy - int(crc.Top-prc.Top)
		cur = r
		depth++
	}
	return cur, cx, cy, depth
}

// keyboardTarget 取目标窗口所属线程当前的焦点控件（真正接收键盘输入的窗口）；
// 拿不到时退回顶层窗口。GetGUIThreadInfo 可跨进程查询，无需目标在前台。
func keyboardTarget(hwnd uintptr) uintptr {
	tid, _, _ := prGetWindowThreadProcessId.Call(hwnd, 0)
	if tid == 0 {
		return hwnd
	}
	var gti guiThreadInfo
	gti.cbSize = uint32(unsafe.Sizeof(gti))
	r, _, _ := prGetGUIThreadInfo.Call(tid, uintptr(unsafe.Pointer(&gti)))
	if r != 0 && gti.hwndFocus != 0 && isWindow(gti.hwndFocus) {
		return gti.hwndFocus
	}
	return hwnd
}

// postTypeText 后台逐字符输入整段文本（WM_CHAR 投递到焦点控件），不动剪贴板、不抢焦点。
// 返回实际接收控件 hwnd 与类名，供调用方落痕。
func postTypeText(hwnd uintptr, s string) (uintptr, string) {
	tgt := keyboardTarget(hwnd)
	for _, r := range s {
		if r == '\n' { // 换行按 CR 投递（多数编辑控件接受）
			prPostMessageW.Call(tgt, wmChar, uintptr('\r'), 1)
			time.Sleep(4 * time.Millisecond)
			continue
		}
		prPostMessageW.Call(tgt, wmChar, uintptr(r), 1)
		time.Sleep(4 * time.Millisecond)
	}
	return tgt, className(tgt)
}

// bgEditCmd 后台组合键 -> 编辑控件命令消息映射表。
// 原理：投递的 WM_KEYDOWN 不更新键盘状态，编辑控件用 GetKeyState 判定修饰键时会认为
// Ctrl 没按下，故 ctrl+c/x/v/a 在 bg 通道下普遍失效；改为直接投递语义等价的命令消息
// （WM_COPY/WM_CUT/WM_PASTE/EM_SETSEL/WM_CLEAR），不依赖任何键盘状态，稳定可靠。
var bgEditCmd = map[string][3]uintptr{
	"ctrl+c":       {wmCopy, 0, 0},
	"ctrl+insert":  {wmCopy, 0, 0},
	"ctrl+x":       {wmCut, 0, 0},
	"shift+delete": {wmCut, 0, 0},
	"ctrl+v":       {wmPaste, 0, 0},
	"shift+insert": {wmPaste, 0, 0},
	"ctrl+a":       {emSetSel, 0, ^uintptr(0)}, // EM_SETSEL(0,-1) = 全选
	"delete":       {wmClear, 0, 0},
}

// normCombo 归一化组合键（小写、去空格、按 + 连接）
func normCombo(combo string) string {
	parts := strings.Split(strings.ToLower(combo), "+")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, "+")
}

// vkExtended 需要带「扩展键」标志的虚拟键（方向键 / 编辑键组 / 右 Ctrl·Alt / Win / 应用键）
func vkExtended(vk uint16) bool {
	switch vk {
	case 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2D, 0x2E,
		0xA3, 0xA5, 0x5B, 0x5C, 0x5D, 0x90, 0x2C, 0x2F, 0x6F:
		return true
	}
	return false
}

// keyLParam 组装 WM_KEYDOWN/UP 的 lParam：重复次数(1) | 扫描码<<16 | 扩展位<<24 |
// (释放: 1<<30|1<<31)。v8 新增——旧实现 lParam=0，靠 lParam 取扫描码的程序收不到键。
func keyLParam(vk uint16, down bool) uintptr {
	sc, _, _ := prMapVirtualKeyW.Call(uintptr(vk), 0) // MAPVK_VK_TO_VSC
	lp := uintptr(1) | (sc&0xFF)<<16
	if vkExtended(vk) {
		lp |= 1 << 24
	}
	if !down {
		lp |= 1<<30 | 1<<31
	}
	return lp
}

// postKeyCombo 后台组合键。优先命中命令消息映射（可靠路径）；否则逐键投递
// WM_KEYDOWN/WM_KEYUP 到焦点控件。返回实际路径："edit_command" | "key_events"。
// 注意：未映射且依赖加速键表/GetKeyState 的快捷键（如 Ctrl+S）在 bg 下可能无效，
// 应回退前台 pressKeys 或改点菜单——本函数不静默切换通道，由调用方决定。
func postKeyCombo(hwnd uintptr, combo string) (string, error) {
	tgt := keyboardTarget(hwnd)
	if c, ok := bgEditCmd[normCombo(combo)]; ok {
		prPostMessageW.Call(tgt, c[0], c[1], c[2])
		time.Sleep(20 * time.Millisecond)
		return "edit_command", nil
	}
	parts := strings.Split(combo, "+")
	vks := make([]uint16, 0, len(parts))
	for _, p := range parts {
		vk, err := vkOf(p)
		if err != nil {
			return "", err
		}
		vks = append(vks, vk)
	}
	for _, vk := range vks {
		prPostMessageW.Call(tgt, wmKeyDown, uintptr(vk), keyLParam(vk, true))
		time.Sleep(8 * time.Millisecond)
	}
	for i := len(vks) - 1; i >= 0; i-- {
		prPostMessageW.Call(tgt, wmKeyUp, uintptr(vks[i]), keyLParam(vks[i], false))
		time.Sleep(8 * time.Millisecond)
	}
	return "key_events", nil
}

// postKeySingle 后台单键按下/抬起（keydown/keyup 的 bg 通道），投递到焦点控件
func postKeySingle(hwnd uintptr, vk uint16, down bool) {
	m := uintptr(wmKeyUp)
	if down {
		m = wmKeyDown
	}
	prPostMessageW.Call(keyboardTarget(hwnd), m, uintptr(vk), keyLParam(vk, down))
}

var vkTable = map[string]uint16{
	"ctrl": 0x11, "control": 0x11, "shift": 0x10, "alt": 0x12, "win": 0x5B,
	"enter": 0x0D, "return": 0x0D, "tab": 0x09, "esc": 0x1B, "escape": 0x1B,
	"backspace": 0x08, "delete": 0x2E, "del": 0x2E, "space": 0x20, "insert": 0x2D,
	"up": 0x26, "down": 0x28, "left": 0x25, "right": 0x27,
	"home": 0x24, "end": 0x23, "pageup": 0x21, "pagedown": 0x22, "pgup": 0x21, "pgdn": 0x22,
	"capslock": 0x14, "numlock": 0x90, "scrolllock": 0x91, "printscreen": 0x2C,
	"f1": 0x70, "f2": 0x71, "f3": 0x72, "f4": 0x73, "f5": 0x74, "f6": 0x75,
	"f7": 0x76, "f8": 0x77, "f9": 0x78, "f10": 0x79, "f11": 0x7A, "f12": 0x7B,
}

func vkOf(token string) (uint16, error) {
	t := strings.ToLower(strings.TrimSpace(token))
	if v, ok := vkTable[t]; ok {
		return v, nil
	}
	if len([]rune(token)) == 1 {
		r := []rune(strings.ToUpper(token))[0]
		ret, _, _ := prVkKeyScanW.Call(uintptr(r))
		vk := uint16(ret & 0xFF)
		if int16(ret) != -1 && vk != 0xFF {
			return vk, nil
		}
	}
	return 0, fmt.Errorf("无法识别的键 %q", token)
}

// pressKeys 执行 "ctrl+shift+s" 形式组合键
func pressKeys(combo string) error {
	parts := strings.Split(combo, "+")
	var vks []uint16
	for _, p := range parts {
		vk, err := vkOf(p)
		if err != nil {
			return err
		}
		vks = append(vks, vk)
	}
	for _, vk := range vks {
		sendKey(vk, true)
		time.Sleep(12 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	for i := len(vks) - 1; i >= 0; i-- {
		sendKey(vks[i], false)
		time.Sleep(12 * time.Millisecond)
	}
	return nil
}

func isAllASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// ===== 剪贴板 =====

func setClipboardText(s string) error {
	var lastErr error
	for try := 0; try < 5; try++ {
		if r, _, _ := prOpenClipboard.Call(0); r != 0 {
			u16 := utf16.Encode([]rune(s))
			size := uintptr((len(u16) + 1) * 2)
			hMem, _, _ := prGlobalAlloc.Call(0x0002, size) // GMEM_MOVEABLE
			if hMem == 0 {
				prCloseClipboard.Call()
				return fmt.Errorf("GlobalAlloc 失败")
			}
			ptr, _, _ := prGlobalLock.Call(hMem)
			dst := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(u16)+1)
			copy(dst, u16)
			dst[len(u16)] = 0
			prGlobalUnlock.Call(hMem)
			prEmptyClipboard.Call()
			if r2, _, _ := prSetClipboardData.Call(13, hMem); r2 == 0 { // CF_UNICODETEXT
				prCloseClipboard.Call()
				return fmt.Errorf("SetClipboardData 失败")
			}
			prCloseClipboard.Call()
			return nil
		}
		lastErr = fmt.Errorf("OpenClipboard 被占用")
		time.Sleep(80 * time.Millisecond)
	}
	return lastErr
}

// clipboardText 读剪贴板文本；ok=false 表示「没有 CF_UNICODETEXT」或打不开（区别于空串）
func clipboardText() (string, bool) {
	r, _, _ := prOpenClipboard.Call(0)
	if r == 0 {
		return "", false
	}
	defer prCloseClipboard.Call()
	h, _, _ := prGetClipboardData.Call(13)
	if h == 0 {
		return "", false
	}
	ptr, _, _ := prGlobalLock.Call(h)
	if ptr == 0 {
		return "", false
	}
	defer prGlobalUnlock.Call(h)
	var u16 []uint16
	for i := 0; i < 1<<20; i++ {
		c := *(*uint16)(unsafe.Pointer(ptr + uintptr(i*2)))
		if c == 0 {
			break
		}
		u16 = append(u16, c)
	}
	return syscall_UTF16ToString(u16), true
}

func getClipboardText() string {
	s, _ := clipboardText()
	return s
}

// typeText 输入文本：纯 ASCII 走虚拟键码逐字；其余走剪贴板 + Ctrl+V（支持中文）。
// v8：restoreClipboard=true 时把剪贴板恢复原样（旧实现静默改掉用户剪贴板）。
// 返回 (viaClipboard, viaVK)。
func typeText(s string, restoreClipboard bool) (bool, bool) {
	if isAllASCII(s) {
		ok := true
		for _, r := range s {
			ret, _, _ := prVkKeyScanW.Call(uintptr(r))
			if int16(ret) == -1 {
				ok = false
				break
			}
			vk := uint16(ret & 0xFF)
			shift := uint16(ret >> 8 & 0xFF)
			if shift&1 != 0 {
				sendKey(0x10, true)
			}
			sendKey(vk, true)
			time.Sleep(8 * time.Millisecond)
			sendKey(vk, false)
			if shift&1 != 0 {
				sendKey(0x10, false)
			}
			time.Sleep(8 * time.Millisecond)
		}
		if ok {
			return false, true
		}
	}
	prev, hadPrev := clipboardText()
	if err := setClipboardText(s); err != nil {
		return false, false
	}
	time.Sleep(60 * time.Millisecond)
	pressKeys("ctrl+v")
	time.Sleep(60 * time.Millisecond)
	if restoreClipboard && hadPrev {
		_ = setClipboardText(prev) // 尽力恢复；失败不影响已完成的输入
	}
	return true, false
}

func syscall_UTF16ToString(s []uint16) string {
	buf := make([]uint16, len(s)+1)
	copy(buf, s)
	return syscall.UTF16ToString(buf)
}
