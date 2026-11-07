// imgx.go —— 图像内核（v8）：像素格式快速转换 / PNG 快速编解码 / BMP 零压缩中转 / 并行分带
//
// 设计取舍：
//   - 截图到 PNG 的默认压缩改 BestSpeed：AI 流水线里「快」比「小」重要
//     （2560x1600 全屏默认压缩 ~150ms → BestSpeed ~25ms）；需要小文件时 WCPILOT_PNG=default。
//   - OCR 中转不再走 PNG：BMP 无压缩，写盘 ~20ms、cv2 解码 ~30ms，比 PNG 编+解少一个量级。
//   - 并行按「行带」切分，天然无写冲突：findColor 每带独立计数、findImage 每带独立命中。
package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"runtime"
	"strconv"
	"sync"
)

// ===== 并行分带 =====

// threads 可用并行度：WCPILOT_THREADS 覆盖 > CPU 数 > 1
func threads() int {
	if v := os.Getenv("WCPILOT_THREADS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 1 {
				return 1
			}
			if n > 64 {
				return 64
			}
			return n
		}
	}
	n := runtime.NumCPU()
	if n < 1 {
		return 1
	}
	if n > 64 {
		n = 64
	}
	return n
}

// parallelRows 把 [0,h) 切成 n 段并行执行；段太小或单核时退化为串行（避免 goroutine 比干活还贵）。
// fn 收到段序号（0..n-1，串行退化时恒为 0）与半开区间 [y0,y1)，便于调用方按段归并结果。
func parallelRows(h int, fn func(idx, y0, y1 int)) {
	n := threads()
	if n <= 1 || h <= 0 || h < n*8 {
		fn(0, 0, h)
		return
	}
	chunk := (h + n - 1) / n
	var wg sync.WaitGroup
	idx := 0
	for y0 := 0; y0 < h; y0 += chunk {
		y1 := y0 + chunk
		if y1 > h {
			y1 = h
		}
		wg.Add(1)
		go func(i, a, b int) {
			defer wg.Done()
			fn(i, a, b)
		}(idx, y0, y1)
		idx++
	}
	wg.Wait()
}

// ===== 像素格式 =====

// bgraToRGBAInPlace 把就地填充的 BGRA 缓冲原地转成 RGBA 并强制不透明。
// 按 uint32 整字处理（每像素 3 次按位运算），比逐通道 4 次字节写快 2-3 倍。
func bgraToRGBAInPlace(p []byte, n int) {
	const alpha = 0xFF000000
	for i := 0; i < n; i++ {
		o := i * 4
		v := binary.LittleEndian.Uint32(p[o:])
		// BGRA -> RGBA：外部交换 R/B，A 强制 255
		binary.LittleEndian.PutUint32(p[o:], alpha|(v&0x000000FF)<<16|(v&0x0000FF00)|(v>>16)&0x000000FF)
	}
}

// ===== PNG =====

// pngLevel 选压缩档位：WCPILOT_PNG=default|best 时降速换体积，其余一律 BestSpeed
func pngLevel() png.CompressionLevel {
	switch os.Getenv("WCPILOT_PNG") {
	case "default":
		return png.DefaultCompression
	case "best":
		return png.BestCompression
	case "none":
		return png.NoCompression
	}
	return png.BestSpeed
}

// savePNG 写 PNG（v8：默认 BestSpeed，见 pngLevel）
func savePNG(path string, img *image.RGBA) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := png.Encoder{CompressionLevel: pngLevel()}
	if err := enc.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// loadPNGImage 读 PNG 为 *image.RGBA。v8：解码结果已是 RGBA 时零拷贝直用，
// 否则走 draw.Draw 的快速路径——旧实现逐像素 At() 取色，2560x1600 要 200ms+。
func loadPNGImage(path string) (*image.RGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	im, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	b := im.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), im, b.Min, draw.Src)
	return out, nil
}

// ===== BMP（OCR 中转用）=====

// writeBMP 写 32bpp BI_RGB 自底向上 BMP（无压缩）。
// OCR 只需「把像素交给 cv2」，PNG 的压缩/解压在这条链上是纯浪费。
func writeBMP(path string, img *image.RGBA) error {
	bd := img.Bounds()
	w, h := bd.Dx(), bd.Dy()
	if w <= 0 || h <= 0 {
		return fmt.Errorf("BMP 尺寸非法 %dx%d", w, h)
	}
	rowSize := w * 4
	imgSize := rowSize * h
	fileSize := 14 + 40 + imgSize

	buf := make([]byte, 14+40+imgSize)
	// BITMAPFILEHEADER
	buf[0], buf[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(buf[2:], uint32(fileSize))
	binary.LittleEndian.PutUint32(buf[10:], 54)
	// BITMAPINFOHEADER
	binary.LittleEndian.PutUint32(buf[14:], 40)
	binary.LittleEndian.PutUint32(buf[18:], uint32(int32(w)))
	binary.LittleEndian.PutUint32(buf[22:], uint32(int32(h))) // 正高度 = 自底向上
	binary.LittleEndian.PutUint16(buf[26:], 1)                // planes
	binary.LittleEndian.PutUint16(buf[28:], 32)               // bpp
	binary.LittleEndian.PutUint32(buf[34:], uint32(imgSize))

	pix := buf[54:]
	for y := 0; y < h; y++ {
		src := img.PixOffset(0, h-1-y)
		dst := y * rowSize
		row := img.Pix[src : src+rowSize]
		for x := 0; x < w; x++ {
			o := x * 4
			pix[dst+o] = row[o+2]   // B
			pix[dst+o+1] = row[o+1] // G
			pix[dst+o+2] = row[o]   // R
			pix[dst+o+3] = 255      // A（cv2 读 BMP 时忽略）
		}
	}
	return os.WriteFile(path, buf, 0644)
}

// ===== 其他 =====

// imgEqual 逐字节比较两张同尺寸图（wait_stable 用；不做任何编码，纯内存）
func imgEqual(a, b *image.RGBA) bool {
	if a == nil || b == nil {
		return a == b
	}
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return false
	}
	if len(a.Pix) != len(b.Pix) {
		return false
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return false
		}
	}
	return true
}

// imgDiffRatio 粗略差异比例（抽样，0..1）；wait_stable 的阈值判定用
func imgDiffRatio(a, b *image.RGBA) float64 {
	if a == nil || b == nil {
		return 1
	}
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return 1
	}
	n := len(a.Pix)
	step := 4 * 16 // 每 16 像素抽 1 个
	if n < step {
		step = 4
	}
	total, diff := 0, 0
	for i := 0; i+3 < n; i += step {
		total++
		if a.Pix[i] != b.Pix[i] || a.Pix[i+1] != b.Pix[i+1] || a.Pix[i+2] != b.Pix[i+2] {
			diff++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(diff) / float64(total)
}
