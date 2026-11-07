package main

import (
	"image"
	"image/color"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// ===== 测试素材 =====

// makeNoise 生成带噪声的图（纯色图会让 findImage 全屏命中，测不出真实成本）
func makeNoise(w, h int, seed int64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(seed))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = uint8(rnd.Intn(256))
		img.Pix[i+1] = uint8(rnd.Intn(256))
		img.Pix[i+2] = uint8(rnd.Intn(256))
		img.Pix[i+3] = 255
	}
	return img
}

// paintBlock 在图上画一块纯色（造可命中的目标）
func paintBlock(img *image.RGBA, x0, y0, w, h int, c color.RGBA) {
	for y := y0; y < y0+h; y++ {
		for x := x0; x < x0+w; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 255
		}
	}
}

// cropOf 取子图作为模板
func cropOf(src *image.RGBA, x, y, w, h int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for yy := 0; yy < h; yy++ {
		copy(out.Pix[yy*out.Stride:yy*out.Stride+w*4], src.Pix[src.PixOffset(x, y+yy):src.PixOffset(x, y+yy)+w*4])
	}
	return out
}

// ===== 功能测试 =====

func TestParseColorSpec(t *testing.T) {
	cases := []struct {
		in      string
		r, g, b uint8
		wantErr bool
	}{
		{"#FF8800", 255, 136, 0, false},
		{"#ffffff", 255, 255, 255, false},
		{"255,136,0", 255, 136, 0, false},
		{" 12, 34, 56 ", 12, 34, 56, false},
		{"#FFF", 0, 0, 0, true},
		{"256,0,0", 0, 0, 0, true},
		{"1,2", 0, 0, 0, true},
		{"", 0, 0, 0, true},
	}
	for _, c := range cases {
		r, g, b, err := parseColorSpec(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: 期望报错，实际 r=%d g=%d b=%d", c.in, r, g, b)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: 意外报错 %v", c.in, err)
			continue
		}
		if r != c.r || g != c.g || b != c.b {
			t.Errorf("%q: got (%d,%d,%d) want (%d,%d,%d)", c.in, r, g, b, c.r, c.g, c.b)
		}
	}
}

func TestParseRegion(t *testing.T) {
	x, y, w, h, err := parseRegion("10,20,300,400")
	if err != nil || x != 10 || y != 20 || w != 300 || h != 400 {
		t.Fatalf("got %d %d %d %d err=%v", x, y, w, h, err)
	}
	if _, _, _, _, err := parseRegion("10,20,0,5"); err == nil {
		t.Fatal("宽为 0 应报错")
	}
	if _, _, _, _, err := parseRegion("abc"); err == nil {
		t.Fatal("非法格式应报错")
	}
}

func TestParseRectSpec(t *testing.T) {
	x, y, w, h, err := parseRectSpec("-100,-200,800,600")
	if err != nil || x != -100 || y != -200 || w != 800 || h != 600 {
		t.Fatalf("负原点应当被接受: %d %d %d %d %v", x, y, w, h, err)
	}
	if _, _, _, _, err := parseRectSpec("1,2,3"); err == nil {
		t.Fatal("少一项应报错")
	}
	if _, _, _, _, err := parseRectSpec("1,2,-3,4"); err == nil {
		t.Fatal("负宽应报错")
	}
}

func TestCropRGBA(t *testing.T) {
	img := makeNoise(100, 80, 1)
	c, ox, oy, err := cropRGBA(img, 10, 20, 30, 40)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bounds().Dx() != 30 || c.Bounds().Dy() != 40 || ox != 10 || oy != 20 {
		t.Fatalf("got %dx%d @%d,%d", c.Bounds().Dx(), c.Bounds().Dy(), ox, oy)
	}
	// 边沿收敛：请求超出右/下，应裁剪而不是报错
	c2, _, _, err := cropRGBA(img, 90, 70, 50, 50)
	if err != nil {
		t.Fatalf("边沿应自动收敛: %v", err)
	}
	if c2.Bounds().Dx() != 10 || c2.Bounds().Dy() != 10 {
		t.Fatalf("收敛尺寸错: %dx%d", c2.Bounds().Dx(), c2.Bounds().Dy())
	}
	if _, _, _, err := cropRGBA(img, 200, 200, 10, 10); err == nil {
		t.Fatal("起点在图外应报错")
	}
}

func TestFindColor(t *testing.T) {
	img := makeNoise(400, 300, 7)
	paintBlock(img, 100, 120, 40, 40, color.RGBA{255, 0, 0, 255})
	hits := findColor(img, 255, 0, 0, 10, 10)
	if len(hits) == 0 {
		t.Fatal("应命中红色块")
	}
	h := hits[0]
	// 命中按 24px 网格聚合，返回的是「命中像素最多的那个桶」的中心，
	// 因此精度是网格级（±24px），不是像素级——这是设计取舍（防一次返回上万点）。
	if h.X < 100-colorCell || h.X > 139+colorCell || h.Y < 120-colorCell || h.Y > 159+colorCell {
		t.Fatalf("命中中心落在色块外太远: %+v（色块 100,120 起 40x40）", h)
	}
	if h.Count < 300 {
		t.Fatalf("命中像素数偏少: %d", h.Count)
	}
	// 容差为 0 时噪声里不该命中纯红
	if len(findColor(img, 255, 0, 0, 0, 10)) == 0 {
		t.Fatal("容差 0 也应命中纯红块")
	}
}

func TestFindImage(t *testing.T) {
	img := makeNoise(600, 400, 3)
	paintBlock(img, 200, 150, 60, 30, color.RGBA{10, 200, 60, 255})
	tpl := cropOf(img, 200, 150, 60, 30)
	hits := findImage(img, tpl, 20, 5)
	if len(hits) == 0 {
		t.Fatal("应命中模板位置")
	}
	h := hits[0]
	if absI(h.X-230) > 3 || absI(h.Y-165) > 3 {
		t.Fatalf("命中中心错: %+v（期望 230,165）", h)
	}
	if h.Score <= 0 || h.Score > 100 {
		t.Fatalf("score 越界: %d", h.Score)
	}
	// 大模板套小图：应安全返回 nil
	if got := findImage(makeNoise(10, 10, 1), tpl, 20, 5); got != nil {
		t.Fatalf("模板大于底图应返回 nil，实际 %v", got)
	}
}

func TestFindColorParallelConsistency(t *testing.T) {
	img := makeNoise(320, 240, 11)
	paintBlock(img, 10, 10, 30, 30, color.RGBA{1, 2, 3, 255})
	paintBlock(img, 200, 150, 30, 30, color.RGBA{1, 2, 3, 255})
	t.Setenv("WCPILOT_THREADS", "1")
	serial := findColor(img, 1, 2, 3, 5, 50)
	t.Setenv("WCPILOT_THREADS", "8")
	par := findColor(img, 1, 2, 3, 5, 50)
	if len(serial) != len(par) {
		t.Fatalf("串行 %d 个命中 vs 并行 %d 个，不一致", len(serial), len(par))
	}
	for i := range serial {
		if serial[i] != par[i] {
			t.Fatalf("第 %d 个命中不一致: %+v vs %+v", i, serial[i], par[i])
		}
	}
}

func TestImageHashAndCacheKey(t *testing.T) {
	a := makeNoise(64, 64, 5)
	b := makeNoise(64, 64, 6)
	if ocrCacheKey(a, ocrOpts{Query: "x"}) == ocrCacheKey(b, ocrOpts{Query: "x"}) {
		t.Fatal("不同图像应得到不同缓存键")
	}
	if ocrCacheKey(a, ocrOpts{Query: "x"}) == ocrCacheKey(a, ocrOpts{Query: "y"}) {
		t.Fatal("不同 query 应得到不同缓存键")
	}
	if ocrCacheKey(a, ocrOpts{Query: "x"}) != ocrCacheKey(a, ocrOpts{Query: "x"}) {
		t.Fatal("同一输入键应稳定")
	}
}

func TestBMPRoundTrip(t *testing.T) {
	img := makeNoise(37, 23, 9)
	p := filepath.Join(t.TempDir(), "x.bmp")
	if err := writeBMP(p, img); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(14 + 40 + 37*23*4); st.Size() != want {
		t.Fatalf("BMP 大小 %d，期望 %d", st.Size(), want)
	}
}

func TestPNGRoundTrip(t *testing.T) {
	img := makeNoise(40, 30, 2)
	p := filepath.Join(t.TempDir(), "x.png")
	if err := savePNG(p, img); err != nil {
		t.Fatal(err)
	}
	back, err := loadPNGImage(p)
	if err != nil {
		t.Fatal(err)
	}
	if back.Bounds().Dx() != 40 || back.Bounds().Dy() != 30 {
		t.Fatalf("尺寸丢了: %v", back.Bounds())
	}
	if !imgEqual(img, back) {
		t.Fatal("PNG 往返像素不一致")
	}
}

func TestAnnotateNoPanic(t *testing.T) {
	img := makeNoise(200, 150, 4)
	out := annotateImage(img, 0, 0, annotateOpt{
		Grid: true, GridStep: 25, Cursor: true,
		Panel: []string{"TEST PANEL 123", "SECOND LINE"},
		Boxes: []Hit{{X: 100, Y: 75, W: 40, H: 20, Score: 88}},
	})
	if out.Bounds() != img.Bounds() {
		t.Fatal("标注不应改变尺寸")
	}
	// 网格/面板必须真的画上去了
	if imgEqual(img, out) {
		t.Fatal("标注后应至少有像素变化")
	}
}

func TestLoadBatchJobs(t *testing.T) {
	jobs, err := loadBatchJobs(`[{"op":"click","x":1,"y":2},{"op":"sleep","ms":10}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].Op != "click" || jobs[1].Ms != 10 {
		t.Fatalf("解析结果错: %+v", jobs)
	}
	if _, err := loadBatchJobs(""); err == nil {
		t.Fatal("空 jobs 应报错")
	}
	if _, err := loadBatchJobs("[]"); err == nil {
		t.Fatal("空数组应报错")
	}
	if _, err := loadBatchJobs(`[{"op":`); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestVkOfAndCombo(t *testing.T) {
	if vk, err := vkOf("ctrl"); err != nil || vk != 0x11 {
		t.Fatalf("ctrl -> %x %v", vk, err)
	}
	if vk, err := vkOf("F5"); err != nil || vk != 0x74 {
		t.Fatalf("F5 -> %x %v", vk, err)
	}
	if vk, err := vkOf("A"); err != nil || vk != 0x41 {
		t.Fatalf("A -> %x %v", vk, err)
	}
	if _, err := vkOf("nosuchkey"); err == nil {
		t.Fatal("未知键应报错")
	}
	if normCombo(" Ctrl + C ") != "ctrl+c" {
		t.Fatalf("normCombo: %q", normCombo(" Ctrl + C "))
	}
}

func TestVkExtended(t *testing.T) {
	if !vkExtended(0x25) { // left
		t.Fatal("方向键应为扩展键")
	}
	if vkExtended(0x41) { // A
		t.Fatal("字母键不是扩展键")
	}
	if vkExtended(0x0D) { // 主键盘回车
		t.Fatal("主键盘回车不是扩展键")
	}
}

func TestImgDiffRatio(t *testing.T) {
	a := makeNoise(100, 100, 1)
	if got := imgDiffRatio(a, a); got != 0 {
		t.Fatalf("同图差异应为 0，实际 %v", got)
	}
	b := makeNoise(100, 100, 2)
	if got := imgDiffRatio(a, b); got < 0.9 {
		t.Fatalf("完全不同的噪声图差异应接近 1，实际 %v", got)
	}
	c := image.NewRGBA(image.Rect(0, 0, 50, 50))
	if got := imgDiffRatio(a, c); got != 1 {
		t.Fatalf("尺寸不同应返回 1，实际 %v", got)
	}
}

// ===== 基准（4MP ≈ 2560x1600 全屏）=====

func BenchmarkFindColor4MP(b *testing.B) {
	img := makeNoise(2560, 1600, 1)
	paintBlock(img, 1000, 800, 200, 100, color.RGBA{255, 136, 0, 255})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if hits := findColor(img, 255, 136, 0, 15, 5); len(hits) == 0 {
			b.Fatal("应命中")
		}
	}
}

func BenchmarkFindColor4MPSerial(b *testing.B) {
	b.Setenv("WCPILOT_THREADS", "1")
	img := makeNoise(2560, 1600, 1)
	paintBlock(img, 1000, 800, 200, 100, color.RGBA{255, 136, 0, 255})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		findColor(img, 255, 136, 0, 15, 5)
	}
}

func BenchmarkFindImage4MP(b *testing.B) {
	img := makeNoise(2560, 1600, 2)
	paintBlock(img, 1200, 900, 80, 40, color.RGBA{20, 200, 90, 255})
	tpl := cropOf(img, 1200, 900, 80, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if hits := findImage(img, tpl, 20, 5); len(hits) == 0 {
			b.Fatal("应命中")
		}
	}
}

func BenchmarkFindImage4MPSerial(b *testing.B) {
	b.Setenv("WCPILOT_THREADS", "1")
	img := makeNoise(2560, 1600, 2)
	paintBlock(img, 1200, 900, 80, 40, color.RGBA{20, 200, 90, 255})
	tpl := cropOf(img, 1200, 900, 80, 40)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		findImage(img, tpl, 20, 5)
	}
}

func BenchmarkBGRAtoRGBA4MP(b *testing.B) {
	buf := make([]byte, 2560*1600*4)
	for i := range buf {
		buf[i] = byte(i)
	}
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bgraToRGBAInPlace(buf, 2560*1600)
	}
}

func BenchmarkSavePNGBestSpeed4MP(b *testing.B) {
	b.Setenv("WCPILOT_PNG", "none")
	img := makeNoise(2560, 1600, 3)
	p := filepath.Join(b.TempDir(), "x.png")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := savePNG(p, img); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSavePNGDefault4MP(b *testing.B) {
	b.Setenv("WCPILOT_PNG", "default")
	img := makeNoise(2560, 1600, 3)
	p := filepath.Join(b.TempDir(), "x.png")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := savePNG(p, img); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScaleBilinear2MP(b *testing.B) {
	src := makeNoise(1280, 800, 4)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scaleBilinear(src, 1280, 800, 2560, 1600)
	}
}

func BenchmarkBMP4MP(b *testing.B) {
	img := makeNoise(2560, 1600, 5)
	p := filepath.Join(b.TempDir(), "x.bmp")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := writeBMP(p, img); err != nil {
			b.Fatal(err)
		}
	}
}
