package main

import (
	"fmt"
	"image"
	"math"
	"time"
	"unsafe"
)

const srccopy = 0x00CC0020

// bitmapToRGBA 把内存 DC 中的 32bpp 位图读为 image.RGBA（BGRA->RGBA，top-down）。
// v8：GetDIBits 直接写进 image.RGBA 的 Pix（省一块 w*h*4 中转缓冲），
// 再用 uint32 整字原地换 R/B——旧的逐像素 4 次字节赋值在 4MP 上有 ~60ms 差距。
func bitmapToRGBA(memDC, bm uintptr, w, h int) (*image.RGBA, error) {
	type bitmapInfoHeader struct {
		Size          uint32
		Width         int32
		Height        int32
		Planes        uint16
		BitCount      uint16
		Compression   uint32
		SizeImage     uint32
		XPelsPerMeter int32
		YPelsPerMeter int32
		ClrUsed       uint32
		ClrImportant  uint32
	}
	type bitmapInfo struct {
		Header bitmapInfoHeader
		Colors [1]uint32
	}
	var bi bitmapInfo
	bi.Header.Size = 40
	bi.Header.Width = int32(w)
	bi.Header.Height = -int32(h) // 负高度 = top-down
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = 0 // BI_RGB
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r, _, _ := prGetDIBits.Call(memDC, bm, 0, uintptr(h),
		uintptr(unsafe.Pointer(&img.Pix[0])), uintptr(unsafe.Pointer(&bi)), 0)
	if r == 0 {
		return nil, fmt.Errorf("GetDIBits 失败")
	}
	bgraToRGBAInPlace(img.Pix, w*h)
	return img, nil
}

// grabScreen 截取屏幕任意区域（绝对坐标）
func grabScreen(x, y, w, h int) (*image.RGBA, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("截图区域尺寸非法 %dx%d", w, h)
	}
	hdcScr, _, _ := prGetDC.Call(0)
	if hdcScr == 0 {
		return nil, fmt.Errorf("GetDC 失败")
	}
	defer prReleaseDC.Call(0, hdcScr)
	mem, _, _ := prCreateCompatibleDC.Call(hdcScr)
	defer prDeleteDC.Call(mem)
	bm, _, _ := prCreateCompatibleBitmap.Call(hdcScr, uintptr(w), uintptr(h))
	if bm == 0 {
		return nil, fmt.Errorf("CreateCompatibleBitmap 失败")
	}
	old, _, _ := prSelectObject.Call(mem, bm)
	defer prSelectObject.Call(mem, old)
	defer prDeleteObject.Call(bm)
	r, _, _ := prBitBlt.Call(mem, 0, 0, uintptr(w), uintptr(h), hdcScr, uintptr(x), uintptr(y), srccopy)
	if r == 0 {
		return nil, fmt.Errorf("BitBlt 失败")
	}
	return bitmapToRGBA(mem, bm, w, h)
}

// printWindowFull 用 PrintWindow(PW_RENDERFULLCONTENT, 可穿遮挡) 抓整窗。
// 画布尺寸必须是整窗（GetWindowRect）尺寸——系统按窗口坐标画，含不可见边框区；
// 裁到可见区由 grabWindow 按 DWM 可见框完成。
func printWindowFull(hwnd uintptr, w, h int) *image.RGBA {
	if w <= 0 || h <= 0 {
		return nil
	}
	hdcWin, _, _ := prGetWindowDC.Call(hwnd)
	if hdcWin == 0 {
		return nil
	}
	mem, _, _ := prCreateCompatibleDC.Call(hdcWin)
	bm, _, _ := prCreateCompatibleBitmap.Call(hdcWin, uintptr(w), uintptr(h))
	prReleaseDC.Call(hwnd, hdcWin)
	if mem == 0 || bm == 0 {
		if mem != 0 {
			prDeleteDC.Call(mem)
		}
		if bm != 0 {
			prDeleteObject.Call(bm)
		}
		return nil
	}
	old, _, _ := prSelectObject.Call(mem, bm)
	r, _, _ := prPrintWindow.Call(hwnd, mem, 2) // PW_RENDERFULLCONTENT
	var img *image.RGBA
	if r != 0 {
		img, _ = bitmapToRGBA(mem, bm, w, h)
	}
	prSelectObject.Call(mem, old)
	prDeleteObject.Call(bm)
	prDeleteDC.Call(mem)
	return img
}

// ===== v7 内容分析：空白判定 / DPI 未感知内容归一化 =====

// contentExtent 返回非黑内容的最大 x / y（全黑返回 -1,-1）
func contentExtent(img *image.RGBA) (int, int) {
	if img == nil {
		return -1, -1
	}
	bd := img.Bounds()
	w, h := bd.Dx(), bd.Dy()
	maxX, maxY := -1, -1
	for y := 0; y < h; y++ {
		row := img.PixOffset(0, y)
		for x := 0; x < w; x++ {
			i := row + x*4
			if img.Pix[i] > 8 || img.Pix[i+1] > 8 || img.Pix[i+2] > 8 {
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	return maxX, maxY
}

// isBlankCapture 判断 PrintWindow 是否「什么都没画」。
// v7 判据升级：旧版用「暗像素占比 >97%」——现代暗色 UI / 全黑窗口会被误杀，
// 错误回退到前台裁屏（引入遮挡污染）。新版 = 整图纯黑（抽样点每通道严格 == 0）。
// v8：抽样按行列步进（旧实现 i%w / i/w 在大图上会走出斜线，采样点分布失真）。
func isBlankCapture(img *image.RGBA) bool {
	if img == nil {
		return true
	}
	bd := img.Bounds()
	w, h := bd.Dx(), bd.Dy()
	if w == 0 || h == 0 {
		return true
	}
	// 目标采样约 30 万点：按面积开方得行列步长
	step := 1
	if w*h > 300000 {
		step = int(math.Sqrt(float64(w*h) / 300000))
		if step < 1 {
			step = 1
		}
	}
	n, dark := 0, 0
	for y := 0; y < h; y += step {
		row := img.PixOffset(0, y)
		for x := 0; x < w; x += step {
			o := row + x*4
			n++
			if img.Pix[o] == 0 && img.Pix[o+1] == 0 && img.Pix[o+2] == 0 {
				dark++
			}
		}
	}
	return n > 0 && dark == n
}

// scaleBilinear 把 src 的 [0,0,sw,sh) 双线性放大到 dw x dh
func scaleBilinear(src *image.RGBA, sw, sh, dw, dh int) *image.RGBA {
	if sw == dw && sh == dh {
		return src
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	rx := float64(sw) / float64(dw)
	ry := float64(sh) / float64(dh)
	for y := 0; y < dh; y++ {
		fy := (float64(y)+0.5)*ry - 0.5
		y0 := int(fy)
		if y0 < 0 {
			y0 = 0
		}
		if y0 > sh-1 {
			y0 = sh - 1
		}
		y1 := y0 + 1
		if y1 > sh-1 {
			y1 = sh - 1
		}
		wy := fy - float64(y0)
		if wy < 0 {
			wy = 0
		}
		if wy > 1 {
			wy = 1
		}
		drow := out.PixOffset(0, y)
		r0, r1 := src.PixOffset(0, y0), src.PixOffset(0, y1)
		for x := 0; x < dw; x++ {
			fx := (float64(x)+0.5)*rx - 0.5
			x0 := int(fx)
			if x0 < 0 {
				x0 = 0
			}
			if x0 > sw-1 {
				x0 = sw - 1
			}
			x1 := x0 + 1
			if x1 > sw-1 {
				x1 = sw - 1
			}
			wx := fx - float64(x0)
			if wx < 0 {
				wx = 0
			}
			if wx > 1 {
				wx = 1
			}
			o00, o10 := r0+x0*4, r0+x1*4
			o01, o11 := r1+x0*4, r1+x1*4
			di := drow + x*4
			for c := 0; c < 3; c++ {
				v := float64(src.Pix[o00+c])*(1-wx)*(1-wy) +
					float64(src.Pix[o10+c])*wx*(1-wy) +
					float64(src.Pix[o01+c])*(1-wx)*wy +
					float64(src.Pix[o11+c])*wx*wy
				out.Pix[di+c] = uint8(v + 0.5)
			}
			out.Pix[di+3] = 255
		}
	}
	return out
}

// normalizeUnawareCapture 修正「DPI 未感知窗口在缩放屏上 PrintWindow 只画逻辑尺寸内容」的缺陷。
// 实测（2026-10-11 @150%）：600x450 画布内容只占左上 400x300，右下大片纯黑。
// 触发条件（全部满足，宁缺勿滥）：
//  1. 目标窗口感知级别 = unaware 且系统缩放 > 1.05；
//  2. 右、下两侧都缺 >16px 且缺失比例都 >6%。
//
// 命中则把内容区双线性放大铺满画布。返回 (新图, 是否修正, 原内容宽, 原内容高)。
func normalizeUnawareCapture(img *image.RGBA, hwnd uintptr) (*image.RGBA, bool, int, int) {
	if img == nil {
		return img, false, 0, 0
	}
	info := windowDpiInfo(hwnd)
	aw, _ := info["awareness"].(string)
	// 注意：GetDpiForWindow 对 unaware 窗口恒返回 96（它自认 100%），
	// 真正的渲染缩放来自所在显示器 DPI（GetDpiForMonitor）——实测 150% 屏 unaware 窗口 = 144。
	if aw != "unaware" || monitorDpi(hwnd) <= 100 {
		return img, false, 0, 0
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	maxX, maxY := contentExtent(img)
	cw, ch := maxX+1, maxY+1
	if cw <= 0 || ch <= 0 || cw > w || ch > h {
		return img, false, 0, 0
	}
	gapX, gapY := w-cw, h-ch
	if gapX <= 16 || gapY <= 16 {
		return img, false, cw, ch
	}
	if float64(gapX)/float64(w) < 0.06 || float64(gapY)/float64(h) < 0.06 {
		return img, false, cw, ch
	}
	return scaleBilinear(img, cw, ch, w, h), true, cw, ch
}

// ===== 窗口截图 =====

// windowShot 一次截图的完整结果（v7）
type windowShot struct {
	Img  *image.RGBA
	Rect RECT // 窗口「可见框」（物理像素）；屏幕模式下为零值
	OX   int  // 图像 (0,0) 对应的屏幕物理坐标
	OY   int
	Diag map[string]interface{}
}

// grabWindow 抓窗口内容。
// v7 口径变化：截图尺寸与原点改用 DWM 可见框（EXTENDED_FRAME_BOUNDS，去不可见边框），
// 窗口内坐标换算随之对齐；新增 DPI 未感知归一化与更严的空白判定。
func grabWindow(hwnd uintptr, allowActivate bool) (*windowShot, error) {
	res := &windowShot{Diag: map[string]interface{}{}}
	if isIconic(hwnd) {
		if !allowActivate {
			return nil, fmt.Errorf("窗口已最小化，--bg 无法后台截取（先还原该窗口，或去掉 --bg）")
		}
		restoreFromMinimized(hwnd) // WM_SYSCOMMAND/SC_RESTORE 唯一可靠路径（探针实锤）
		time.Sleep(350 * time.Millisecond)
		if isIconic(hwnd) {
			time.Sleep(400 * time.Millisecond)
		}
		if isIconic(hwnd) {
			return nil, fmt.Errorf("窗口处于最小化停放态且 SC_RESTORE 未能还原（请手动还原该窗口）")
		}
		time.Sleep(150 * time.Millisecond) // 等 DWM 重绘，防止刚还原就截到未渲染帧
	}

	win := windowRect(hwnd)                 // PrintWindow 画布口径（含不可见边框）
	vis, dwmOK := extendedFrameBounds(hwnd) // 可见内容口径
	w, h := int(win.Right-win.Left), int(win.Bottom-win.Top)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("窗口尺寸无效（可能已最小化）")
	}
	res.Diag["dwm_ok"] = dwmOK
	res.Diag["window_size"] = []int{w, h}
	for k, v := range windowDpiInfo(hwnd) {
		res.Diag[k] = v
	}

	img := printWindowFull(hwnd, w, h)
	var perr error
	if img == nil {
		perr = fmt.Errorf("PrintWindow 调用失败")
	} else if fixed, ok, cw, ch := normalizeUnawareCapture(img, hwnd); ok {
		img = fixed
		res.Diag["dpi_scaled"] = true
		res.Diag["content_size"] = []int{cw, ch}
	} else {
		res.Diag["dpi_scaled"] = false
	}
	if img != nil && isBlankCapture(img) {
		perr = fmt.Errorf("PrintWindow 输出全黑（GPU 渲染窗口常见）")
		img = nil
	}

	if img == nil {
		if !allowActivate {
			return nil, fmt.Errorf("%v；--bg 下不激活前台，无法回退裁屏（去掉 --bg 或手动前置窗口）", perr)
		}
		res.Diag["fallback"] = "screen_after_activate"
		activateWindow(hwnd)
		// 激活可能改变窗口几何（最小化还原 / 应用重排）——必须重取，沿用旧尺寸会裁错（v6 遗留 bug）
		win = windowRect(hwnd)
		vis, dwmOK = extendedFrameBounds(hwnd)
		vw, vh := int(vis.Right-vis.Left), int(vis.Bottom-vis.Top)
		if vw <= 0 || vh <= 0 {
			return nil, fmt.Errorf("窗口尺寸无效（可能已最小化）")
		}
		g2, gerr := grabScreen(int(vis.Left), int(vis.Top), vw, vh)
		if gerr != nil {
			return nil, gerr
		}
		res.Img, res.Rect, res.OX, res.OY = g2, vis, int(vis.Left), int(vis.Top)
		res.Diag["dwm_ok"] = dwmOK
		return res, nil
	}

	// 裁到可见框（去掉 DWM 不可见边框）
	if dwmOK {
		dx, dy := int(vis.Left-win.Left), int(vis.Top-win.Top)
		cw, ch := int(vis.Right-vis.Left), int(vis.Bottom-vis.Top)
		if c, cx, cy, cerr := cropRGBA(img, dx, dy, cw, ch); cerr == nil {
			img = c
			res.OX = int(win.Left) + cx
			res.OY = int(win.Top) + cy
			res.Rect = vis
		} else {
			res.Diag["crop_error"] = cerr.Error()
			res.Rect = win // 几何异常 → 退回整窗口径，保持图/坐标自洽
			res.OX, res.OY = int(win.Left), int(win.Top)
		}
	} else {
		res.Rect = win
		res.OX, res.OY = int(win.Left), int(win.Top)
	}
	res.Img = img
	return res, nil
}

// shotSmart 按优先级截取：--region > --hwnd/--title/target > 全屏；统一返回图像原点。
func shotSmart(hwndFlag uint, titleFlag, regionFlag string, allowActivate bool) (*windowShot, error) {
	if regionFlag != "" {
		x, y, w, h, err := parseRegion(regionFlag)
		if err != nil {
			return nil, err
		}
		if hwnd, _, has := resolveTarget(hwndFlag, titleFlag); has {
			ws, err := grabWindow(hwnd, allowActivate)
			if err != nil {
				return nil, err
			}
			crop, cx, cy, err := cropRGBA(ws.Img, x, y, w, h)
			if err != nil {
				return nil, err
			}
			ws.Img = crop
			ws.OX += cx
			ws.OY += cy
			ws.Diag["mode"] = "region+window"
			return ws, nil
		}
		img, err := grabScreen(x, y, w, h)
		if err != nil {
			return nil, err
		}
		return &windowShot{Img: img, OX: x, OY: y,
			Diag: map[string]interface{}{"mode": "region+screen"}}, nil
	}
	if hwnd, _, has := resolveTarget(hwndFlag, titleFlag); has {
		ws, err := grabWindow(hwnd, allowActivate)
		if err != nil {
			return nil, err
		}
		ws.Diag["mode"] = "window"
		return ws, nil
	}
	vx, vy, vw, vh := virtualScreenRect()
	img, err := grabScreen(vx, vy, vw, vh)
	if err != nil {
		return nil, err
	}
	return &windowShot{Img: img, OX: vx, OY: vy,
		Diag: map[string]interface{}{"mode": "screen"}}, nil
}

// virtualScreenRect 全虚拟屏范围（SM_X/Y/CX/CYVIRTUALSCREEN=76/77/78/79）；
// 多屏时覆盖所有显示器，且原点可为负——GetSystemMetrics 返回 int32，必须经 int32 转换收负值。
func virtualScreenRect() (int, int, int, int) {
	sm := func(i uintptr) int { r, _, _ := prGetSystemMetrics.Call(i); return int(int32(r)) }
	return sm(76), sm(77), sm(78), sm(79)
}

// cropRGBA 从图中裁子区域；起点越界报错、边沿自动收敛到图内，返回裁剪图与图内起点
func cropRGBA(img *image.RGBA, x, y, w, h int) (*image.RGBA, int, int, error) {
	if img == nil {
		return nil, 0, 0, fmt.Errorf("源图为空")
	}
	bd := img.Bounds()
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= bd.Dx() || y >= bd.Dy() {
		return nil, 0, 0, fmt.Errorf("region 起点 (%d,%d) 在图外（图 %dx%d）", x, y, bd.Dx(), bd.Dy())
	}
	if x+w > bd.Dx() {
		w = bd.Dx() - x
	}
	if y+h > bd.Dy() {
		h = bd.Dy() - y
	}
	if w <= 0 || h <= 0 {
		return nil, 0, 0, fmt.Errorf("region 尺寸非法")
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for yy := 0; yy < h; yy++ {
		src := img.PixOffset(x, y+yy)
		dst := out.PixOffset(0, yy)
		copy(out.Pix[dst:dst+w*4], img.Pix[src:src+w*4])
	}
	return out, x, y, nil
}

func parseRegion(s string) (int, int, int, int, error) {
	var x, y, w, h int
	n, err := fmt.Sscanf(s, "%d,%d,%d,%d", &x, &y, &w, &h)
	if err != nil || n != 4 || w <= 0 || h <= 0 {
		return 0, 0, 0, 0, fmt.Errorf("region 格式应为 x,y,w,h")
	}
	return x, y, w, h, nil
}
