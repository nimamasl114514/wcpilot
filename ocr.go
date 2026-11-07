// ocr.go —— OCR 通道（v8）：常驻守护进程 + 内容寻址缓存 + 一次性 spawn 兜底
//
// 为什么要守护进程：wcpilot 是「一次调用一个进程」的 CLI，而 rapidocr 的
// import + 模型加载每次要 ~2.5s（本机实测 1.75s init + 0.79s import）。AI 代理一次
// 任务里 find_text / wait_text 会调用几十次，这笔开销是纯浪费。
// 方案：第一个调用者拉起一个 detached 的 python 服务（监听 127.0.0.1 随机端口，
// 令牌鉴权、空闲自退），之后所有 wcpilot 进程复用它 → 每次 OCR 只付推理时间。
//
// 为什么还要缓存：wait_text 轮询时同一帧会被反复识别。缓存键 = 图像像素内容哈希
// （不是文件 mtime，因为临时文件每次重写），命中即 0ms 返回。
//
// 兜底原则：守护进程起不来（没装 python / 端口被占 / 版本不兼容）一律静默降级到
// 一次性 spawn；OCR 本身失败才报错，且错误信息必须能指向修复动作。
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type OcrBox struct {
	Text  string  `json:"text"`
	Score float64 `json:"score"`
	X     int     `json:"x"`
	Y     int     `json:"y"`
	W     int     `json:"w"`
	H     int     `json:"h"`
	Cx    int     `json:"cx"`
	Cy    int     `json:"cy"`
}

// ocrOpts OCR 请求参数（Early = 只找一个词时命中即停）
type ocrOpts struct {
	Query string
	All   bool
	Angle bool
	Early bool
}

// ocrMeta OCR 诊断信息：走了哪条通道、花了多久、扫了多少块、是否提前停
type ocrMeta struct {
	Mode    string `json:"mode"` // daemon | spawn | cache
	MS      int    `json:"ms"`
	Total   int    `json:"total"`
	Scanned int    `json:"scanned"`
	Partial bool   `json:"partial,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Engine  string `json:"engine,omitempty"`
}

// ===== 对外统一入口 =====

// ocrRecognize 识别整张图（内部：缓存 → 守护进程 → 一次性 spawn）
func ocrRecognize(exeDir string, img *image.RGBA, o ocrOpts) ([]OcrBox, ocrMeta, error) {
	if o.Query == "" {
		o.Early = false
	}
	key := ocrCacheKey(img, o)
	if boxes, meta, ok := ocrCacheGet(key); ok {
		meta.Mode = "cache"
		return boxes, meta, nil
	}

	// 中转文件用 BMP：无压缩 → 写盘 ~20ms、cv2 解码 ~30ms（PNG 编+解要一个量级更多）
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("wcpilot_ocr_%d.bmp", os.Getpid()))
	if err := writeBMP(tmp, img); err != nil {
		return nil, ocrMeta{}, err
	}
	defer os.Remove(tmp)

	boxes, meta, err := ocrDispatch(exeDir, tmp, o)
	if err != nil {
		return nil, meta, err
	}
	ocrCachePut(key, boxes, meta)
	return boxes, meta, nil
}

func ocrDispatch(exeDir, imgPath string, o ocrOpts) ([]OcrBox, ocrMeta, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("WCPILOT_OCR")))
	if mode != "spawn" {
		boxes, meta, err := ocrViaDaemon(exeDir, imgPath, o)
		if err == nil {
			return boxes, meta, nil
		}
		if mode == "daemon" {
			return nil, meta, fmt.Errorf("OCR 守护进程不可用: %v", err)
		}
		// auto：守护进程失败 → 静默降级 spawn，但在 meta 里留痕
		boxes, meta2, err2 := ocrViaSpawn(exeDir, imgPath, o)
		if err2 == nil {
			meta2.Engine = "spawn_fallback: " + err.Error()
		}
		return boxes, meta2, err2
	}
	return ocrViaSpawn(exeDir, imgPath, o)
}

// ===== 一次性 spawn（旧通道 / 兜底）=====

func ocrViaSpawn(exeDir, imgPath string, o ocrOpts) ([]OcrBox, ocrMeta, error) {
	py, err := findPython()
	if err != nil {
		return nil, ocrMeta{Mode: "spawn"}, err
	}
	helper, err := ocrHelperPath(exeDir)
	if err != nil {
		return nil, ocrMeta{Mode: "spawn"}, err
	}
	args := []string{helper, imgPath}
	if o.Query != "" {
		args = append(args, "--query", o.Query)
	}
	if o.All {
		args = append(args, "--all")
	}
	if o.Angle {
		args = append(args, "--angle")
	}
	st := time.Now()
	out, err := exec.Command(py, args...).CombinedOutput()
	if err != nil {
		return nil, ocrMeta{Mode: "spawn"}, fmt.Errorf("OCR 执行失败: %v 输出: %s", err, trimStr(string(out), 400))
	}
	var boxes []OcrBox
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &boxes); err != nil {
		// 辅助脚本用 {"error": "..."} 报错
		var e struct{ Error string }
		if json.Unmarshal([]byte(strings.TrimSpace(string(out))), &e) == nil && e.Error != "" {
			return nil, ocrMeta{Mode: "spawn"}, fmt.Errorf("%s", e.Error)
		}
		return nil, ocrMeta{Mode: "spawn"}, fmt.Errorf("OCR 输出解析失败: %s", trimStr(string(out), 400))
	}
	return boxes, ocrMeta{Mode: "spawn", MS: int(time.Since(st).Milliseconds()), Total: len(boxes)}, nil
}

func ocrHelperPath(exeDir string) (string, error) {
	for _, dir := range []string{exeDir, must(os.Getwd())} {
		p := filepath.Join(dir, "ocr_helper.py")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("缺少 OCR 辅助脚本 ocr_helper.py（应与 wcpilot.exe 同目录）")
}

func must(s string, _ error) string { return s }

func trimStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// findPython 找可用 python 解释器：先 $WCPILOT_PYTHON，再 exec.LookPath，
// 最后按 PATH 逐项做 POSIX→Windows 路径转换后再探测。
// 背景：从 Git Bash 启动 wcpilot 时继承的 PATH 是 /c/... POSIX 形式，
// Windows 的 exec.LookPath 解析不了 → OCR 一律报「未找到 python 解释器」。
func findPython() (string, error) {
	if p := os.Getenv("WCPILOT_PYTHON"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p, nil
	}
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		win := posixToWindows(entry)
		if win == "" || strings.Contains(strings.ToLower(win), "windowsapps") {
			continue // WindowsApps 下是应用商店占位符，不可用
		}
		for _, cand := range []string{win, win + ".exe",
			filepath.Join(win, "python.exe"), filepath.Join(win, "python3.exe"), filepath.Join(win, "py.exe")} {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("未找到 python 解释器（PATH 无可解析的 python；可用 WCPILOT_PYTHON 指定绝对路径，并 pip install rapidocr-onnxruntime）")
}

// posixToWindows 把 MSYS/Cygwin 风格路径（/c/Users/x）转成 Windows 路径（C:\Users\x）
func posixToWindows(p string) string {
	if len(p) >= 3 && p[0] == '/' && p[2] == '/' &&
		((p[1] >= 'a' && p[1] <= 'z') || (p[1] >= 'A' && p[1] <= 'Z')) {
		return strings.ToUpper(p[1:2]) + ":\\" + strings.ReplaceAll(p[3:], "/", "\\")
	}
	if strings.ContainsAny(p, `:\`) {
		return p
	}
	return ""
}

// ===== 守护进程 =====

var (
	daemonMu   sync.Mutex // 保护「拉起/复用」竞态
	reqSeq     int
	daemonInfo *daemonState
)

type daemonState struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func ocrStatePath() string {
	return filepath.Join(os.TempDir(), "wcpilot_ocr.daemon.json")
}

func daemonIdleSeconds() int {
	if v := os.Getenv("WCPILOT_OCR_IDLE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 600
}

// daemonRequest 发一条请求并读一行响应（含超时，避免卡死调用方）
func daemonRequest(st *daemonState, req map[string]interface{}) (map[string]interface{}, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", st.Port), 1200*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(180 * time.Second))

	req["token"] = st.Token
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var rsp map[string]interface{}
	if err := json.Unmarshal(line, &rsp); err != nil {
		return nil, fmt.Errorf("守护进程响应无法解析: %s", trimStr(string(line), 200))
	}
	return rsp, nil
}

// pingDaemon 检查状态文件里的守护进程还活着
func pingDaemon(st *daemonState) bool {
	if st == nil || st.Port <= 0 {
		return false
	}
	rsp, err := daemonRequest(st, map[string]interface{}{"id": 0, "op": "ping"})
	if err != nil {
		return false
	}
	ok, _ := rsp["pong"].(bool)
	return ok
}

func loadDaemonState() *daemonState {
	b, err := os.ReadFile(ocrStatePath())
	if err != nil {
		return nil
	}
	var st daemonState
	if json.Unmarshal(b, &st) != nil || st.Port <= 0 {
		return nil
	}
	return &st
}

func randomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// startDaemon 拉起 detached 的 python 服务并等它就绪
func startDaemon(exeDir string) (*daemonState, error) {
	py, err := findPython()
	if err != nil {
		return nil, err
	}
	helper, err := ocrHelperPath(exeDir)
	if err != nil {
		return nil, err
	}
	token := randomToken()
	register := filepath.Join(os.TempDir(), "wcpilot_ocr.ready."+token[:8]+".json")
	os.Remove(register)

	cmd := exec.Command(py, helper, "--serve",
		"--register", register, "--token", token,
		"--idle", strconv.Itoa(daemonIdleSeconds()), "--port", "0")
	base := uint32(0x00000008 | 0x00000200)                                                    // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: base | 0x01000000} // + CREATE_BREAKAWAY_FROM_JOB
	if err := cmd.Start(); err != nil {
		// 某些沙箱/作业对象不允许 breakaway → 退回不带该标志再试一次
		cmd = exec.Command(py, helper, "--serve",
			"--register", register, "--token", token,
			"--idle", strconv.Itoa(daemonIdleSeconds()), "--port", "0")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: base}
		if err2 := cmd.Start(); err2 != nil {
			return nil, fmt.Errorf("拉起 OCR 守护进程失败: %v", err2)
		}
	}
	go cmd.Wait() // 回收句柄（子进程独立于本进程存活）

	// 首次要 import + 加载模型，给足 30s
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(register); err == nil && len(b) > 0 {
			var st daemonState
			if json.Unmarshal(b, &st) == nil && st.Port > 0 {
				st.Token = token
				os.Remove(register)
				if pingDaemon(&st) {
					persistDaemonState(&st)
					return &st, nil
				}
				return nil, fmt.Errorf("守护进程已就绪但 ping 失败")
			}
		}
		time.Sleep(80 * time.Millisecond)
	}
	os.Remove(register)
	return nil, fmt.Errorf("OCR 守护进程 30s 内未就绪（首次启动要加载模型；若反复失败请改用 WCPILOT_OCR=spawn 排查）")
}

func persistDaemonState(st *daemonState) {
	b, _ := json.Marshal(st)
	_ = os.WriteFile(ocrStatePath(), b, 0644)
}

// ensureDaemon 复用已有守护进程，没有就拉一个（并发调用只拉一次）
func ensureDaemon(exeDir string) (*daemonState, error) {
	daemonMu.Lock()
	defer daemonMu.Unlock()
	if daemonInfo != nil && pingDaemon(daemonInfo) {
		return daemonInfo, nil
	}
	if st := loadDaemonState(); st != nil {
		if pingDaemon(st) {
			daemonInfo = st
			return st, nil
		}
		os.Remove(ocrStatePath()) // 陈旧状态文件：进程已死
	}
	st, err := startDaemon(exeDir)
	if err != nil {
		return nil, err
	}
	daemonInfo = st
	return st, nil
}

// ocrViaDaemon 走常驻服务
func ocrViaDaemon(exeDir, imgPath string, o ocrOpts) ([]OcrBox, ocrMeta, error) {
	st, err := ensureDaemon(exeDir)
	if err != nil {
		return nil, ocrMeta{Mode: "daemon"}, err
	}
	daemonMu.Lock()
	reqSeq++
	id := reqSeq
	daemonMu.Unlock()

	rsp, err := daemonRequest(st, map[string]interface{}{
		"id": id, "op": "ocr", "image": imgPath,
		"query": o.Query, "all": o.All, "angle": o.Angle, "early": o.Early,
	})
	if err != nil {
		daemonInfo = nil
		os.Remove(ocrStatePath())
		return nil, ocrMeta{Mode: "daemon"}, err
	}
	if ok, _ := rsp["ok"].(bool); !ok {
		msg, _ := rsp["error"].(string)
		if msg == "" {
			msg = "未知错误"
		}
		return nil, ocrMeta{Mode: "daemon"}, fmt.Errorf("%s", msg)
	}
	raw, _ := json.Marshal(rsp["boxes"])
	var boxes []OcrBox
	_ = json.Unmarshal(raw, &boxes)

	meta := ocrMeta{Mode: "daemon"}
	meta.MS = intOf(rsp["ms"])
	meta.Total = intOf(rsp["total"])
	meta.Scanned = intOf(rsp["scanned"])
	meta.Partial, _ = rsp["partial"].(bool)
	meta.Stage, _ = rsp["stage"].(string)
	return boxes, meta, nil
}

func intOf(v interface{}) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	}
	return 0
}

// ocrDaemonStatus 供 doctor / ocr_status 使用
func ocrDaemonStatus() (map[string]interface{}, bool) {
	st := loadDaemonState()
	info := map[string]interface{}{}
	if st == nil {
		return info, false
	}
	info["pid"] = st.PID
	info["port"] = st.Port
	alive := pingDaemon(st)
	info["alive"] = alive
	return info, alive
}

// stopOcrDaemon 主动优雅退出守护进程（ocr_stop 子命令）
func stopOcrDaemon() bool {
	st := loadDaemonState()
	if st == nil {
		return false
	}
	_, err := daemonRequest(st, map[string]interface{}{"id": 0, "op": "quit"})
	os.Remove(ocrStatePath())
	daemonInfo = nil
	return err == nil
}

// ===== 内容寻址缓存 =====

func ocrCacheDir() string {
	return filepath.Join(os.TempDir(), "wcpilot_ocr_cache")
}

// hashPix 64 位 FNV-1a（按 8 字节字），16MB 图 ~3ms；只用于缓存键，不要求抗碰撞
func hashPix(p []byte, seed uint64) uint64 {
	const prime = 1099511628211
	h := seed
	n := len(p) / 8
	for i := 0; i < n; i++ {
		h = (h ^ binary.LittleEndian.Uint64(p[i*8:])) * prime
	}
	for _, b := range p[n*8:] {
		h = (h ^ uint64(b)) * prime
	}
	return h
}

func ocrCacheKey(img *image.RGBA, o ocrOpts) string {
	bd := img.Bounds()
	h := hashPix(img.Pix, 1469598103934665603)
	h = hashPix([]byte(fmt.Sprintf("%dx%d|%s|%v|%v", bd.Dx(), bd.Dy(), o.Query, o.All, o.Angle)), h)
	return fmt.Sprintf("%016x", h)
}

func ocrCacheGet(key string) ([]OcrBox, ocrMeta, bool) {
	if os.Getenv("WCPILOT_OCR_CACHE") == "off" {
		return nil, ocrMeta{}, false
	}
	b, err := os.ReadFile(filepath.Join(ocrCacheDir(), key+".json"))
	if err != nil {
		return nil, ocrMeta{}, false
	}
	var payload struct {
		Boxes []OcrBox `json:"boxes"`
		Meta  ocrMeta  `json:"meta"`
	}
	if json.Unmarshal(b, &payload) != nil {
		return nil, ocrMeta{}, false
	}
	// 缓存命中算 0ms：真正需要计时的是本次调用
	payload.Meta.MS = 0
	return payload.Boxes, payload.Meta, true
}

func ocrCachePut(key string, boxes []OcrBox, meta ocrMeta) {
	if os.Getenv("WCPILOT_OCR_CACHE") == "off" {
		return
	}
	dir := ocrCacheDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	b, err := json.Marshal(map[string]interface{}{"boxes": boxes, "meta": meta})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, key+".json"), b, 0644)
	pruneCache(dir, 300)
}

// pruneCache 缓存上限：超过 n 个文件按 mtime 删最旧的（尽力而为，失败静默）
func pruneCache(dir string, n int) {
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) <= n {
		return
	}
	type fi struct {
		name string
		mod  time.Time
	}
	list := make([]fi, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, err := e.Info(); err == nil {
			list = append(list, fi{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.Before(list[j].mod) })
	drop := len(list) - n
	for i := 0; i < drop; i++ {
		os.Remove(filepath.Join(dir, list[i].name))
	}
}

// ===== CLI 层：find_text =====

func cmdFindText(exeDir string, hwndFlag uint, titleFlag, regionFlag, query string, all, click, bg bool, out string) {
	ws, err := shotSmart(hwndFlag, titleFlag, regionFlag, true)
	if err != nil {
		die("%v", err)
	}
	img, rc, ox, oy := ws.Img, ws.Rect, ws.OX, ws.OY
	_, _, hasTarget := resolveTarget(hwndFlag, titleFlag)

	boxes, meta, err := ocrRecognize(exeDir, img, ocrOpts{Query: query, All: all, Early: !all})
	if err != nil {
		die("%v", err)
	}
	var hits []Hit
	for _, b := range boxes {
		if all || (query != "" && strings.Contains(b.Text, query)) {
			h := Hit{X: b.Cx, Y: b.Cy, W: b.W, H: b.H, Score: int(b.Score * 100), Count: 1, Text: b.Text}
			hits = append(hits, toScreenHits([]Hit{h}, ox, oy)[0])
		}
	}
	if hits == nil {
		hits = []Hit{}
	}
	if click && len(hits) > 0 {
		clickHit(hits[0], ox, oy, rc, hasTarget, bg, "left", false)
	}
	rec := map[string]interface{}{"ok": true, "count": len(hits), "hits": hits,
		"query": query, "windowed": hasTarget, "clicked": click && len(hits) > 0,
		"ocr":  meta,
		"note": "hits 的 x/y 为截图内坐标；screen_x/screen_y 为屏幕绝对坐标"}
	if p := saveHitAnnotation(out, img, ox, oy, hits, "FIND_TEXT"); p != "" {
		rec["annotated_path"] = p
	}
	jout(rec)
}
