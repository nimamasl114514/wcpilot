// find.go —— 找色 / 找图 / 匹配结果（v8 并行版）
//
// v8 性能改造（同一份 2560x1600 图，24 核实测）：
//   - findColor：map[int]*acc 逐像素累加 → 扁平 []cellAcc + 行带并行，188ms → 30ms 量级
//   - findImage：暴力滑窗 → 行带并行 + 首点早退 + 命中数原子封顶，模板越大收益越明显
//   - 两者都可用 WCPILOT_THREADS 限流，单核环境自动退化串行（不引入 goroutine 开销）
package main

import (
	"fmt"
	"image"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Hit struct {
	X       int    `json:"x"` // 图内命中中心 x
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	Count   int    `json:"count"`
	Score   int    `json:"score,omitempty"`
	Text    string `json:"text,omitempty"` // OCR 命中的原文（--all 时用来核对识别质量）
	ScreenX int    `json:"screen_x,omitempty"`
	ScreenY int    `json:"screen_y,omitempty"`
}

func parseColorSpec(s string) (uint8, uint8, uint8, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "#") {
		if len(s) != 7 {
			return 0, 0, 0, fmt.Errorf("颜色格式 #RRGGBB")
		}
		v, err := strconv.ParseUint(s[1:], 16, 32)
		if err != nil {
			return 0, 0, 0, err
		}
		return uint8(v >> 16), uint8(v >> 8), uint8(v), nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("颜色格式 r,g,b 或 #RRGGBB")
	}
	var rgb [3]uint8
	for i, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || v < 0 || v > 255 {
			return 0, 0, 0, fmt.Errorf("颜色分量非法")
		}
		rgb[i] = uint8(v)
	}
	return rgb[0], rgb[1], rgb[2], nil
}

// ===== 找色 =====

const colorCell = 24 // 命中像素聚合网格（防止一次返回上万个点）

type cellAcc struct {
	sx, sy, n int32
}

// findColor 找颜色：命中像素按 24px 网格分桶，返回桶中心（按命中数降序）。
// v8：扁平数组代替 map（桶号 = cy*cw+cx 可直接算出），行带并行归并。
func findColor(img *image.RGBA, r, g, b uint8, tol, max int) []Hit {
	if img == nil || max <= 0 {
		return nil
	}
	bd := img.Bounds()
	w, h := bd.Dx(), bd.Dy()
	if w == 0 || h == 0 {
		return nil
	}
	cw := (w + colorCell - 1) / colorCell
	ch := (h + colorCell - 1) / colorCell

	bw := threads()
	if bw > h {
		bw = h
	}
	if bw < 1 {
		bw = 1
	}
	// 每带独立累加，最后按桶号相加（桶的划分与行带边界无关，直接相加即正确）
	parts := make([][]cellAcc, bw)
	parallelRows(h, func(idx, y0, y1 int) {
		acc := make([]cellAcc, cw*ch)
		pix := img.Pix
		stride := img.Stride
		for y := y0; y < y1; y++ {
			row := y * stride
			cy := y / colorCell
			for x := 0; x < w; x++ {
				i := row + x*4
				dr := int(pix[i]) - int(r)
				if dr < 0 {
					dr = -dr
				}
				if dr > tol {
					continue
				}
				dg := int(pix[i+1]) - int(g)
				if dg < 0 {
					dg = -dg
				}
				if dg > tol {
					continue
				}
				db := int(pix[i+2]) - int(b)
				if db < 0 {
					db = -db
				}
				if db > tol {
					continue
				}
				a := &acc[cy*cw+x/colorCell]
				a.sx += int32(x)
				a.sy += int32(y)
				a.n++
			}
		}
		parts[idx] = acc
	})

	total := make([]cellAcc, cw*ch)
	for _, acc := range parts {
		if acc == nil {
			continue
		}
		for i := range acc {
			if acc[i].n == 0 {
				continue
			}
			total[i].sx += acc[i].sx
			total[i].sy += acc[i].sy
			total[i].n += acc[i].n
		}
	}

	hits := make([]Hit, 0, 64)
	for cy := 0; cy < ch; cy++ {
		for cx := 0; cx < cw; cx++ {
			a := total[cy*cw+cx]
			if a.n < 3 {
				continue
			}
			hits = append(hits, Hit{
				X: int(a.sx / a.n), Y: int(a.sy / a.n),
				W: colorCell, H: colorCell, Count: int(a.n),
			})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Count != hits[j].Count {
			return hits[i].Count > hits[j].Count
		}
		if hits[i].Y != hits[j].Y {
			return hits[i].Y < hits[j].Y
		}
		return hits[i].X < hits[j].X
	})
	if len(hits) > max {
		hits = hits[:max]
	}
	return hits
}

func absI(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ===== 找图 =====

// findImage 模板匹配：9 点采样快速过滤 → 全板抽样平均差校验 → 命中去重。
// v8：行带并行 + 首点（左上角像素）先比对的早退路径；命中数达上限即全局停止。
func findImage(big, tpl *image.RGBA, tol, max int) []Hit {
	if big == nil || tpl == nil || max <= 0 {
		return nil
	}
	bb, tb := big.Bounds(), tpl.Bounds()
	bw, bh, tw, th := bb.Dx(), bb.Dy(), tb.Dx(), tb.Dy()
	if tw > bw || th > bh || tw < 1 || th < 1 {
		return nil
	}

	// 模板采样点（均匀 9 点，角 → 中 → 边）
	sx := [9]int{0, tw / 2, tw - 1, 0, tw - 1, tw / 2, 0, tw / 2, tw - 1}
	sy := [9]int{0, 0, 0, th / 2, th / 2, th / 2, th - 1, th - 1, th - 1}
	type px struct {
		dx, dy  int
		r, g, b uint8
	}
	var samps [9]px
	for i := range sx {
		o := tpl.PixOffset(sx[i], sy[i])
		samps[i] = px{sx[i], sy[i], tpl.Pix[o], tpl.Pix[o+1], tpl.Pix[o+2]}
	}

	var (
		mu    sync.Mutex
		all   []Hit
		count int32
	)
	stop := func() bool { return atomic.LoadInt32(&count) >= int32(max) }

	parallelRows(bh-th+1, func(idx, y0, y1 int) {
		local := make([]Hit, 0, 4)
		pix := big.Pix
		stride := big.Stride
		for y := y0; y < y1; y++ {
			if stop() {
				break
			}
			for x := 0; x+tw <= bw; x += 2 { // step=2：半分辨率滑窗，配合去重足够
				i := y*stride + x*4
				// 首点早退（绝大多数位置一个像素就能否掉）
				if absI(int(pix[i])-int(samps[0].r)) > tol ||
					absI(int(pix[i+1])-int(samps[0].g)) > tol ||
					absI(int(pix[i+2])-int(samps[0].b)) > tol {
					continue
				}
				okAll := true
				for k := 1; k < 9; k++ {
					o := (y+samps[k].dy)*stride + (x+samps[k].dx)*4
					if absI(int(pix[o])-int(samps[k].r)) > tol ||
						absI(int(pix[o+1])-int(samps[k].g)) > tol ||
						absI(int(pix[o+2])-int(samps[k].b)) > tol {
						okAll = false
						break
					}
				}
				if !okAll {
					continue
				}
				// 全板均差校验（每 4 像素抽样）
				sum, cnt := 0, 0
				for ty := 0; ty < th; ty += 4 {
					ro := (y+ty)*stride + x*4
					to := tpl.PixOffset(0, ty)
					for tx := 0; tx < tw; tx += 4 {
						o1 := ro + tx*4
						o2 := to + tx*4
						sum += absI(int(pix[o1])-int(tpl.Pix[o2])) +
							absI(int(pix[o1+1])-int(tpl.Pix[o2+1])) +
							absI(int(pix[o1+2])-int(tpl.Pix[o2+2]))
						cnt += 3
					}
				}
				if cnt == 0 || sum/cnt > tol {
					continue
				}
				local = append(local, Hit{
					X: x + tw/2, Y: y + th/2, W: tw, H: th,
					Score: 100 - (sum/cnt)*100/256,
				})
				if len(local) >= max {
					break
				}
			}
		}
		if len(local) == 0 {
			return
		}
		mu.Lock()
		if len(all) < max {
			room := max - len(all)
			if len(local) > room {
				local = local[:room]
			}
			all = append(all, local...)
			atomic.StoreInt32(&count, int32(len(all)))
		}
		mu.Unlock()
	})

	if len(all) == 0 {
		return nil
	}
	return dedupeHits(all, tw, th)
}

func dedupeHits(hits []Hit, tw, th int) []Hit {
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		dup := false
		for _, o := range out {
			if absI(h.X-o.X) < tw/2 && absI(h.Y-o.Y) < th/2 {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, h)
		}
	}
	return out
}

// toScreenHits 把图内坐标换算为屏幕坐标（统一加截图原点，任意通道都给出绝对坐标）
func toScreenHits(hits []Hit, ox, oy int) []Hit {
	for i := range hits {
		hits[i].ScreenX = ox + hits[i].X
		hits[i].ScreenY = oy + hits[i].Y
	}
	return hits
}

// ===== CLI 层 =====

func cmdFindColor(hwndFlag uint, titleFlag, regionFlag, colorStr string, tol, max int, all, click, bg bool, out string) {
	r, g, b, err := parseColorSpec(colorStr)
	if err != nil {
		die("%v", err)
	}
	t0 := time.Now()
	ws, err := shotSmart(hwndFlag, titleFlag, regionFlag, true)
	if err != nil {
		die("%v", err)
	}
	tCap := time.Since(t0)
	img, rc, ox, oy := ws.Img, ws.Rect, ws.OX, ws.OY
	_, _, hasTarget := resolveTarget(hwndFlag, titleFlag)
	if all {
		max = 100
	}
	hits := findColor(img, r, g, b, tol, max)
	hits = toScreenHits(hits, ox, oy)
	trace("capture=%dms find=%dms threads=%d hits=%d",
		tCap.Milliseconds(), (time.Since(t0) - tCap).Milliseconds(), threads(), len(hits))
	if click && len(hits) > 0 {
		clickHit(hits[0], ox, oy, rc, hasTarget, bg, "left", false)
	}
	rec := map[string]interface{}{"ok": true, "count": len(hits), "hits": hits,
		"windowed": hasTarget, "clicked": click && len(hits) > 0}
	if p := saveHitAnnotation(out, img, ox, oy, hits, "FIND_COLOR"); p != "" {
		rec["annotated_path"] = p // 命中框可视化：一眼看出识别到了什么
	}
	jout(rec)
}

func cmdFindImage(hwndFlag uint, titleFlag, regionFlag, tplPath string, tol, max int, click, bg bool, out string) {
	if tplPath == "" {
		die("find_image 需要 --tpl 模板图片")
	}
	tpl, err := loadPNGImage(tplPath)
	if err != nil {
		die("模板读取失败: %v", err)
	}
	t0 := time.Now()
	ws, err := shotSmart(hwndFlag, titleFlag, regionFlag, true)
	if err != nil {
		die("%v", err)
	}
	tCap := time.Since(t0)
	img, rc, ox, oy := ws.Img, ws.Rect, ws.OX, ws.OY
	_, _, hasTarget := resolveTarget(hwndFlag, titleFlag)
	hits := findImage(img, tpl, tol, max)
	hits = toScreenHits(hits, ox, oy)
	trace("capture=%dms find=%dms tpl=%dx%d threads=%d hits=%d",
		tCap.Milliseconds(), (time.Since(t0) - tCap).Milliseconds(),
		tpl.Bounds().Dx(), tpl.Bounds().Dy(), threads(), len(hits))
	if click && len(hits) > 0 {
		clickHit(hits[0], ox, oy, rc, hasTarget, bg, "left", false)
	}
	rec := map[string]interface{}{"ok": true, "count": len(hits), "hits": hits,
		"windowed": hasTarget, "clicked": click && len(hits) > 0}
	if p := saveHitAnnotation(out, img, ox, oy, hits, "FIND_IMAGE"); p != "" {
		rec["annotated_path"] = p
	}
	jout(rec)
}

// clickHit 点击命中点：非 bg 一律点屏幕绝对坐标 (ox+X, oy+Y)；
// bg 走 PostMessage 需窗口内相对坐标，用 (ox-rc.Left+X) 换算——region 裁剪后的命中也能点对。
func clickHit(h Hit, ox, oy int, rc RECT, windowed, bg bool, btn string, dbl bool) {
	if windowed {
		hwnd := resolveSafeHwnd(rc)
		if bg {
			if hwnd == 0 {
				die("bg 点击需要有效目标窗口（target 锁定或 --hwnd）")
			}
			postClick(hwnd, h.X+(ox-int(rc.Left)), h.Y+(oy-int(rc.Top)), btn, dbl)
			return
		}
		activateWindow(hwnd)
	}
	clickAt(ox+h.X, oy+h.Y, btn, dbl)
}

// resolveSafeHwnd 供 clickHit 在后台模式找回 hwnd（经 target 状态）
func resolveSafeHwnd(rc RECT) uintptr {
	if t, ok := loadTarget(); ok && isWindow(t.Hwnd) {
		return t.Hwnd
	}
	return 0
}
