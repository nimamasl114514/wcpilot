// annotate.go —— 截图可视化标注（找 bug 用）：坐标网格 / 鼠标位置 / 命中框 / 信息面板
//
// 设计约束：wcpilot 零第三方依赖 → 文字用内置 5x7 点阵字模自绘（数字 / 大写字母 / 常用符号），
// 不引入 x/image/font。标注只作用于「输出给人和 AI 看的 PNG」，不影响 find_*/OCR 内部图源。
package main

import (
	"fmt"
	"image"
	"image/color"
	"path/filepath"
	"strconv"
	"unicode"
)

// ===== 5x7 点阵字模（每字符 5 列，每列 1 字节，bit0 = 顶行） =====

var glyph5x7 = map[rune][5]byte{
	'0':  {0x3E, 0x51, 0x49, 0x45, 0x3E},
	'1':  {0x00, 0x42, 0x7F, 0x40, 0x00},
	'2':  {0x42, 0x61, 0x51, 0x49, 0x46},
	'3':  {0x21, 0x41, 0x45, 0x4B, 0x31},
	'4':  {0x18, 0x14, 0x12, 0x7F, 0x10},
	'5':  {0x27, 0x45, 0x45, 0x45, 0x39},
	'6':  {0x3C, 0x4A, 0x49, 0x49, 0x30},
	'7':  {0x01, 0x71, 0x09, 0x05, 0x03},
	'8':  {0x36, 0x49, 0x49, 0x49, 0x36},
	'9':  {0x06, 0x49, 0x49, 0x29, 0x1E},
	'A':  {0x7E, 0x11, 0x11, 0x11, 0x7E},
	'B':  {0x7F, 0x49, 0x49, 0x49, 0x36},
	'C':  {0x3E, 0x41, 0x41, 0x41, 0x22},
	'D':  {0x7F, 0x41, 0x41, 0x22, 0x1C},
	'E':  {0x7F, 0x49, 0x49, 0x49, 0x41},
	'F':  {0x7F, 0x09, 0x09, 0x01, 0x01},
	'G':  {0x3E, 0x41, 0x41, 0x51, 0x32},
	'H':  {0x7F, 0x08, 0x08, 0x08, 0x7F},
	'I':  {0x00, 0x41, 0x7F, 0x41, 0x00},
	'J':  {0x20, 0x40, 0x41, 0x3F, 0x01},
	'K':  {0x7F, 0x08, 0x14, 0x22, 0x41},
	'L':  {0x7F, 0x40, 0x40, 0x40, 0x40},
	'M':  {0x7F, 0x02, 0x04, 0x02, 0x7F},
	'N':  {0x7F, 0x04, 0x08, 0x10, 0x7F},
	'O':  {0x3E, 0x41, 0x41, 0x41, 0x3E},
	'P':  {0x7F, 0x09, 0x09, 0x09, 0x06},
	'Q':  {0x3E, 0x41, 0x51, 0x21, 0x5E},
	'R':  {0x7F, 0x09, 0x19, 0x29, 0x46},
	'S':  {0x46, 0x49, 0x49, 0x49, 0x31},
	'T':  {0x01, 0x01, 0x7F, 0x01, 0x01},
	'U':  {0x3F, 0x40, 0x40, 0x40, 0x3F},
	'V':  {0x1F, 0x20, 0x40, 0x20, 0x1F},
	'W':  {0x7F, 0x20, 0x18, 0x20, 0x7F},
	'X':  {0x63, 0x14, 0x08, 0x14, 0x63},
	'Y':  {0x03, 0x04, 0x78, 0x04, 0x03},
	'Z':  {0x61, 0x51, 0x49, 0x45, 0x43},
	' ':  {0x00, 0x00, 0x00, 0x00, 0x00},
	'.':  {0x00, 0x60, 0x60, 0x00, 0x00},
	',':  {0x00, 0x50, 0x30, 0x00, 0x00},
	':':  {0x00, 0x36, 0x36, 0x00, 0x00},
	'-':  {0x08, 0x08, 0x08, 0x08, 0x08},
	'_':  {0x40, 0x40, 0x40, 0x40, 0x40},
	'+':  {0x08, 0x08, 0x3E, 0x08, 0x08},
	'=':  {0x14, 0x14, 0x14, 0x14, 0x14},
	'/':  {0x20, 0x10, 0x08, 0x04, 0x02},
	'\\': {0x02, 0x04, 0x08, 0x10, 0x20},
	'#':  {0x14, 0x7F, 0x14, 0x7F, 0x14},
	'(':  {0x00, 0x1C, 0x22, 0x41, 0x00},
	')':  {0x00, 0x41, 0x22, 0x1C, 0x00},
	'%':  {0x23, 0x13, 0x08, 0x64, 0x62},
	'*':  {0x14, 0x08, 0x3E, 0x08, 0x14},
	'@':  {0x3E, 0x41, 0x5D, 0x55, 0x3E},
	'?':  {0x02, 0x01, 0x51, 0x09, 0x06},
}

// ===== 基础绘制 =====

func setPx(img *image.RGBA, x, y int, c color.RGBA) {
	bd := img.Bounds()
	if x < 0 || y < 0 || x >= bd.Dx() || y >= bd.Dy() {
		return
	}
	i := img.PixOffset(x, y)
	img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
}

// fillRect 实心矩形（裁剪一次，然后按行整段写；旧版逐像素 setPx 每点做 4 次边界判断）
func fillRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	bd := img.Bounds()
	wImg, hImg := bd.Dx(), bd.Dy()
	x0, y0 := x, y
	x1, y1 := x+w, y+h
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > wImg {
		x1 = wImg
	}
	if y1 > hImg {
		y1 = hImg
	}
	if x0 >= x1 || y0 >= y1 {
		return
	}
	var px [4]byte
	px[0], px[1], px[2], px[3] = c.R, c.G, c.B, 255
	for yy := y0; yy < y1; yy++ {
		i := img.PixOffset(x0, yy)
		for xx := x0; xx < x1; xx++ {
			copy(img.Pix[i:i+4], px[:])
			i += 4
		}
	}
}

// blendRect 半透明混合（alpha = 前景占比）
func blendRect(img *image.RGBA, x, y, w, h int, c color.RGBA, alpha float64) {
	bd := img.Bounds()
	for yy := y; yy < y+h; yy++ {
		if yy < 0 || yy >= bd.Dy() {
			continue
		}
		for xx := x; xx < x+w; xx++ {
			if xx < 0 || xx >= bd.Dx() {
				continue
			}
			i := img.PixOffset(xx, yy)
			img.Pix[i] = uint8(float64(img.Pix[i])*(1-alpha) + float64(c.R)*alpha)
			img.Pix[i+1] = uint8(float64(img.Pix[i+1])*(1-alpha) + float64(c.G)*alpha)
			img.Pix[i+2] = uint8(float64(img.Pix[i+2])*(1-alpha) + float64(c.B)*alpha)
			img.Pix[i+3] = 255
		}
	}
}

func strokeRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	for i := 0; i < w; i++ {
		setPx(img, x+i, y, c)
		setPx(img, x+i, y+h-1, c)
	}
	for i := 0; i < h; i++ {
		setPx(img, x, y+i, c)
		setPx(img, x+w-1, y+i, c)
	}
}

// drawText 逐字符绘制；scale 为像素放大倍数（1 → 字符 5x7，步进 6）
func drawText(img *image.RGBA, x, y int, s string, c color.RGBA, scale int) int {
	if scale < 1 {
		scale = 1
	}
	cx := x
	for _, ch := range s {
		g, ok := glyph5x7[unicode.ToUpper(ch)]
		if !ok {
			g = glyph5x7['?']
		}
		for col := 0; col < 5; col++ {
			b := g[col]
			for row := 0; row < 7; row++ {
				if b&(1<<uint(row)) != 0 {
					fillRect(img, cx+col*scale, y+row*scale, scale, scale, c)
				}
			}
		}
		cx += 6 * scale
	}
	return cx - x
}

func textWidth(s string, scale int) int {
	if scale < 1 {
		scale = 1
	}
	return len([]rune(s)) * 6 * scale
}

// ===== 标注要素 =====

// drawGrid 坐标网格：每 step 像素一条淡线，每 5 条加重并在交点附近标数值（图内坐标）
func drawGrid(img *image.RGBA, step int, line, major color.RGBA) {
	if step < 5 {
		step = 5
	}
	bd := img.Bounds()
	w, h := bd.Dx(), bd.Dy()
	for x := 0; x < w; x += step {
		c := line
		majorLine := (x/step)%5 == 0
		if majorLine {
			c = major
		}
		fillRect(img, x, 0, 1, h, c) // 整列一次写完
		if majorLine {
			drawText(img, x+2, 2, strconv.Itoa(x), major, 1)
		}
	}
	for y := 0; y < h; y += step {
		c := line
		majorLine := (y/step)%5 == 0
		if majorLine {
			c = major
		}
		fillRect(img, 0, y, w, 1, c)
		if majorLine && y > 0 {
			drawText(img, 2, y+2, strconv.Itoa(y), major, 1)
		}
	}
}

// drawCrosshair 鼠标位置十字标记 + 坐标标签
func drawCrosshair(img *image.RGBA, x, y, r int, c color.RGBA) {
	for i := -r; i <= r; i++ {
		setPx(img, x+i, y, c)
		setPx(img, x, y+i, c)
	}
	label := "@" + strconv.Itoa(x) + "," + strconv.Itoa(y)
	lx, ly := x+6, y+6
	pad := 2
	tw := textWidth(label, 1) + pad*2
	blendRect(img, lx-1, ly-1, tw, 7+pad*2, color.RGBA{0, 0, 0, 255}, 0.72)
	drawText(img, lx+pad-1, ly+pad-1, label, c, 1)
}

// drawHits 命中框（find_text/color/image 的可视化）
func drawHits(img *image.RGBA, hits []Hit, c color.RGBA) {
	for i, h := range hits {
		w, hh := h.W, h.H
		if w <= 0 {
			w = 14
		}
		if hh <= 0 {
			hh = 14
		}
		strokeRect(img, h.X-w/2, h.Y-hh/2, w, hh, c)
		// 角上点一个实心小方块强化定位
		fillRect(img, h.X-2, h.Y-2, 5, 5, c)
		if i < 20 {
			tag := "#" + strconv.Itoa(i) + " " + strconv.Itoa(h.X) + "," + strconv.Itoa(h.Y)
			if h.Score > 0 {
				tag += " S" + strconv.Itoa(h.Score)
			}
			lx, ly := h.X+w/2+3, h.Y-hh/2
			blendRect(img, lx-1, ly-1, textWidth(tag, 1)+4, 11, color.RGBA{0, 0, 0, 255}, 0.72)
			drawText(img, lx+1, ly+1, tag, c, 1)
		}
	}
}

// drawPanel 左上角信息面板（半透明底 + 多行文字）
func drawPanel(img *image.RGBA, lines []string, c color.RGBA) {
	if len(lines) == 0 {
		return
	}
	maxW := 0
	for _, l := range lines {
		if w := textWidth(l, 1); w > maxW {
			maxW = w
		}
	}
	pad := 4
	blendRect(img, 4, 4, maxW+pad*2, len(lines)*9+pad*2-2, color.RGBA{0, 0, 0, 255}, 0.72)
	for i, l := range lines {
		drawText(img, 4+pad, 4+pad+i*9, l, c, 1)
	}
}

// ===== 高层入口 =====

// saveHitAnnotation 把命中框画到截图副本上另存（out 为空则不存），返回绝对路径。
// find_text / find_color / find_image 共用：一眼看出「识别到了什么 / 框在哪」。
func saveHitAnnotation(out string, img *image.RGBA, ox, oy int, hits []Hit, title string) string {
	if out == "" || img == nil {
		return ""
	}
	opt := annotateOpt{Grid: true, GridStep: 50, Cursor: true, Boxes: hits,
		Panel: []string{title + " HITS " + strconv.Itoa(len(hits)),
			fmt.Sprintf("ORIGIN %d,%d", ox, oy)}}
	res := annotateImage(img, ox, oy, opt)
	if err := savePNG(out, res); err != nil {
		return ""
	}
	abs, _ := filepath.Abs(out)
	return abs
}

// annotateOpt 标注配置
type annotateOpt struct {
	Grid     bool
	GridStep int
	Cursor   bool
	Panel    []string
	Boxes    []Hit
	BoxColor color.RGBA
}

// annotateImage 在副本上绘制标注（不改原图）
func annotateImage(src *image.RGBA, ox, oy int, opt annotateOpt) *image.RGBA {
	if src == nil {
		return src
	}
	bd := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bd.Dx(), bd.Dy()))
	copy(out.Pix, src.Pix)

	gridLine := color.RGBA{70, 110, 70, 255}
	gridMajor := color.RGBA{120, 190, 120, 255}
	markC := color.RGBA{255, 80, 80, 255}

	if opt.Grid {
		step := opt.GridStep
		if step <= 0 {
			step = 50
		}
		drawGrid(out, step, gridLine, gridMajor)
	}
	if len(opt.Boxes) > 0 {
		bc := opt.BoxColor
		if bc.A == 0 {
			bc = color.RGBA{255, 200, 0, 255}
		}
		drawHits(out, opt.Boxes, bc)
	}
	if opt.Cursor {
		x, y := cursorPos()
		gx, gy := int(x)-ox, int(y)-oy
		if gx >= 0 && gy >= 0 && gx < bd.Dx() && gy < bd.Dy() {
			drawCrosshair(out, gx, gy, 14, markC)
		}
	}
	if len(opt.Panel) > 0 {
		drawPanel(out, opt.Panel, color.RGBA{235, 235, 235, 255})
	}
	return out
}
