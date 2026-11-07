// batch.go —— 复合动作编排引擎（v8）
//
// 一次调用跑完「点击-输入-等待-拖拽-判定-剪贴板」整条链，逐 job 回报耗时与结果。
// 坐标语义与单动作子命令完全一致（窗口锁定/本 job 指定窗口时 = 窗口内相对坐标，越界该项失败）。
//
// v8 修复与新增：
//
//	· 【真 bug】失败短路时 os.Exit 不会执行 defer → keydown 按住的键不会被释放（会卡键）。
//	  现在退出前显式 releaseAll()。
//	· 每个 job 可以带 hwnd/title，覆盖全局 target 锁定（一条流程里操作多个窗口）。
//	· 新增 wait_stable op（等画面真的不动了，替代拍脑袋的 sleep）。
//	· wheel 支持 bg（WM_MOUSEWHEEL）。
//	· type 恢复剪贴板。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BatchJob 复合动作清单中的一项；字段按需取用（未用的保持零值忽略）
type BatchJob struct {
	Op       string  `json:"op"`          // 见 runBatch 文档
	X        int     `json:"x"`           // 主坐标 x（窗口锁定=窗口内相对坐标，越界该项失败）
	Y        int     `json:"y"`           // 主坐标 y（同语义）
	X2       int     `json:"x2"`          // drag 终点 x
	Y2       int     `json:"y2"`          // drag 终点 y
	Hwnd     uint    `json:"hwnd"`        // 本 job 临时指定窗口（0 = 用全局 target）
	Title    string  `json:"title"`       // 本 job 临时指定窗口标题关键字
	Btn      string  `json:"btn"`         // left|right|middle
	Double   bool    `json:"double"`      // 双击
	Text     string  `json:"text"`        // type 文本 / OCR 查找词
	Key      string  `json:"key"`         // 组合键 "ctrl+c" / keydown 键名
	Delta    int     `json:"delta"`       // wheel 增量
	Color    string  `json:"color"`       // #RRGGBB
	Tpl      string  `json:"tpl"`         // 模板图路径
	Tol      int     `json:"tol"`         // 容差
	Delay    int     `json:"delay"`       // 动作前延迟 ms
	Ms       int     `json:"ms"`          // sleep 时长 ms
	Steps    int     `json:"steps"`       // drag 分段数（默认 20）
	StepMs   int     `json:"step_ms"`     // drag 每段间隔 ms（默认 10）
	Timeout  int     `json:"timeout_ms"`  // wait_* 超时 ms（默认 30000）
	Interval int     `json:"interval_ms"` // wait_* 轮询间隔 ms（默认 300；text/gone 默认 800）
	StableMs int     `json:"stable_ms"`   // wait_stable 需要保持稳定的时长
	Ratio    float64 `json:"ratio"`       // wait_stable 差异阈值（默认 0.001）
	Click    bool    `json:"click"`       // wait_* 命中后点击首个命中点
	All      bool    `json:"all"`         // OCR 返回全部命中（关闭 early-stop）
	Angle    bool    `json:"angle"`       // OCR 方向分类
	Bg       bool    `json:"bg"`          // click/key/type/keydown/keyup/wheel 走后台消息注入
	Rect     string  `json:"rect"`        // winctl 目标矩形 "x,y,w,h"（屏幕绝对坐标）
	State    string  `json:"state"`       // winctl 状态 max|min|normal|top|notop
	OnError  string  `json:"on_error"`    // stop|continue（默认 stop；batch --continue-on-error 全局宽松）
	Comment  string  `json:"comment"`     // 备注，只进结果 JSON，便于读长流程日志
}

type batchItem struct {
	I       int                    `json:"i"`
	Op      string                 `json:"op"`
	OK      bool                   `json:"ok"`
	Ms      int64                  `json:"ms"`
	Info    map[string]interface{} `json:"info,omitempty"`
	Err     string                 `json:"error,omitempty"`
	Comment string                 `json:"comment,omitempty"`
}

// runBatch 编排执行全部动作；任一失败默认短路（on_error=continue 或全局 --continue-on-error 可跳）。
// 无论正常结束还是短路退出，都会先释放所有被 keydown 按住的键（防卡键）。
func runBatch(jobs []BatchJob, contOnErr bool, exeDir string, ev *evidenceOpt) {
	var reports []batchItem
	var steps []map[string]interface{}
	var pressed []uint16
	releaseAll := func() {
		for i := len(pressed) - 1; i >= 0; i-- {
			sendKey(pressed[i], false)
			time.Sleep(10 * time.Millisecond)
		}
		pressed = nil
	}
	defer releaseAll()

	for i := range jobs {
		j := &jobs[i]
		step := map[string]interface{}{"i": i, "op": j.Op}
		if j.Comment != "" {
			step["comment"] = j.Comment
		}
		if ev != nil && ev.Shots { // 时间线：动作前状态
			step["before"] = captureInto(ev.Dir, fmt.Sprintf("step%03d_before", i))
		}
		st := time.Now()
		info, err := execJob(j, exeDir, &pressed)
		rep := batchItem{I: i, Op: j.Op, OK: err == nil, Ms: time.Since(st).Milliseconds(), Info: info, Comment: j.Comment}
		if err != nil {
			rep.Err = err.Error()
		}
		reports = append(reports, rep)
		step["ok"] = err == nil
		step["ms"] = rep.Ms
		if err != nil {
			step["error"] = err.Error()
		}
		if ev != nil && ev.Shots { // 时间线：动作后状态
			step["after"] = captureInto(ev.Dir, fmt.Sprintf("step%03d_after", i))
		}
		if err != nil && ev != nil { // 失败现场（截图 + 上下文）
			if sc := saveScene(ev, i, j, err.Error(), info); sc != nil {
				step["scene"] = sc
			}
		}
		steps = append(steps, step)

		if err != nil && !(contOnErr || strings.EqualFold(j.OnError, "continue")) {
			releaseAll() // 关键：os.Exit 不会跑 defer，必须显式释放
			if ev != nil {
				writeJSON(filepath.Join(ev.Dir, "timeline.json"), map[string]interface{}{
					"total": len(jobs), "done": i + 1, "steps": steps, "finished": false,
				})
			}
			out := map[string]interface{}{
				"ok": false, "done": i + 1, "total": len(jobs), "results": reports,
				"error": fmt.Sprintf("job[%d] %q 失败，流程短路（已自动释放全部按住的键）", i, j.Op),
			}
			if ev != nil {
				out["evidence_dir"] = ev.Dir
			}
			jout(out)
			os.Exit(exitFail)
		}
	}
	if reports == nil {
		reports = []batchItem{}
	}
	anyFail := false
	for _, r := range reports {
		if !r.OK {
			anyFail = true
			break
		}
	}
	if ev != nil {
		writeJSON(filepath.Join(ev.Dir, "timeline.json"), map[string]interface{}{
			"total": len(jobs), "done": len(reports), "steps": steps, "finished": true,
		})
	}
	out := map[string]interface{}{"ok": !anyFail, "done": len(reports), "total": len(jobs), "results": reports}
	if ev != nil {
		out["evidence_dir"] = ev.Dir
	}
	jout(out)
	if anyFail {
		os.Exit(exitFail)
	}
}

// jobCoords 本 job 的坐标换算（job 自带的 hwnd/title 优先，否则回落到全局 target）
func jobCoords(j *BatchJob, x, y int) (int, int, uintptr, error) {
	return tryCoords(j.Hwnd, j.Title, x, y)
}

// jobTarget 本 job 的目标窗口（0 = 屏幕模式）
func jobTarget(j *BatchJob) (uintptr, RECT, bool) {
	return resolveTargetSafe(j.Hwnd, j.Title)
}

// execJob 执行单个复合动作，返回 (附加信息, 错误)
func execJob(j *BatchJob, exeDir string, pressed *[]uint16) (map[string]interface{}, error) {
	if j.Delay > 0 {
		time.Sleep(time.Duration(j.Delay) * time.Millisecond)
	}
	switch j.Op {
	case "sleep":
		if j.Ms > 0 {
			time.Sleep(time.Duration(j.Ms) * time.Millisecond)
		}
		return map[string]interface{}{"slept_ms": j.Ms}, nil

	case "click":
		sx, sy, hwnd, err := jobCoords(j, j.X, j.Y)
		if err != nil {
			return nil, err
		}
		if j.Bg {
			if hwnd == 0 {
				return nil, fmt.Errorf("click bg 需要目标窗口（target 锁定或本 job 的 hwnd/title）")
			}
			tgt, cls, depth := postClick(hwnd, j.X, j.Y, defaultStr(j.Btn, "left"), j.Double)
			return map[string]interface{}{"mode": "bg_postmessage", "win_x": j.X, "win_y": j.Y,
				"target_hwnd": tgt, "target_class": cls, "child_depth": depth}, nil
		}
		if hwnd != 0 {
			activateWindow(hwnd)
		}
		clickAt(sx, sy, defaultStr(j.Btn, "left"), j.Double)
		return map[string]interface{}{"screen_x": sx, "screen_y": sy}, nil

	case "move":
		sx, sy, _, err := jobCoords(j, j.X, j.Y)
		if err != nil {
			return nil, err
		}
		setCursor(sx, sy)
		return map[string]interface{}{"screen_x": sx, "screen_y": sy}, nil

	case "wheel":
		hwnd, _, has := jobTarget(j)
		sx, sy, _, err := jobCoords(j, j.X, j.Y)
		if err != nil {
			return nil, err
		}
		if j.Bg {
			if !has {
				return nil, fmt.Errorf("wheel bg 需要目标窗口")
			}
			tgt, cls := postWheel(hwnd, j.Delta, sx, sy)
			return map[string]interface{}{"delta": j.Delta, "mode": "bg_postmessage",
				"target_hwnd": tgt, "target_class": cls}, nil
		}
		doWheel(j.Delta, sx, sy, true, hwnd)
		return map[string]interface{}{"delta": j.Delta, "screen_x": sx, "screen_y": sy}, nil

	case "key":
		if j.Key == "" {
			return nil, fmt.Errorf("key op 需要 key 字段")
		}
		if j.Bg {
			hwnd, _, has := jobTarget(j)
			if !has {
				return nil, fmt.Errorf("key bg 需要目标窗口")
			}
			via, err := postKeyCombo(hwnd, j.Key)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"key": j.Key, "mode": "bg_postmessage", "via": via}, nil
		}
		activateIfTarget(j)
		if err := pressKeys(j.Key); err != nil {
			return nil, err
		}
		return map[string]interface{}{"key": j.Key}, nil

	case "keydown", "keyup":
		if j.Key == "" {
			return nil, fmt.Errorf("%s op 需要 key 字段", j.Op)
		}
		vk, err := vkOf(j.Key)
		if err != nil {
			return nil, err
		}
		down := j.Op == "keydown"
		if j.Bg {
			hwnd, _, has := jobTarget(j)
			if !has {
				return nil, fmt.Errorf("%s bg 需要目标窗口", j.Op)
			}
			postKeySingle(hwnd, vk, down)
			return map[string]interface{}{"key": j.Key, "down": down, "mode": "bg_postmessage"}, nil
		}
		sendKey(vk, down)
		if down {
			*pressed = append(*pressed, vk)
		} else {
			*pressed = removeVk(*pressed, vk)
		}
		return map[string]interface{}{"key": j.Key, "down": down}, nil

	case "type":
		if j.Text == "" {
			return nil, fmt.Errorf("type op 需要 text 字段")
		}
		if j.Bg {
			hwnd, _, has := jobTarget(j)
			if !has {
				return nil, fmt.Errorf("type bg 需要目标窗口")
			}
			tgt, cls := postTypeText(hwnd, j.Text)
			return map[string]interface{}{"typed": j.Text, "mode": "bg_postmessage",
				"target_hwnd": tgt, "target_class": cls}, nil
		}
		activateIfTarget(j)
		clip, plain := typeText(j.Text, true)
		return map[string]interface{}{"typed": j.Text, "via_clipboard": clip, "via_vk": plain}, nil

	case "drag":
		if err := dragJob(j); err != nil {
			return nil, err
		}
		return map[string]interface{}{"from": [2]int{j.X, j.Y}, "to": [2]int{j.X2, j.Y2}}, nil

	case "click_text":
		hits, ox, oy, rc, windowed, err := probeTextHits(j, exeDir, j.Text, false)
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 {
			return nil, fmt.Errorf("OCR 未命中 %q", j.Text)
		}
		clickHit(hits[0], ox, oy, rc, windowed, j.Bg, defaultStr(j.Btn, "left"), j.Double)
		return map[string]interface{}{"hit": hits[0]}, nil

	case "click_color":
		hits, ox, oy, rc, windowed, err := probeColorHits(j, j.Color, j.Tol)
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 {
			return nil, fmt.Errorf("未找到颜色 %q", j.Color)
		}
		clickHit(hits[0], ox, oy, rc, windowed, j.Bg, defaultStr(j.Btn, "left"), j.Double)
		return map[string]interface{}{"hit": hits[0]}, nil

	case "click_image":
		hits, ox, oy, rc, windowed, err := probeImageHits(j, j.Tpl, j.Tol)
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 {
			return nil, fmt.Errorf("未找到模板图 %q", j.Tpl)
		}
		clickHit(hits[0], ox, oy, rc, windowed, j.Bg, defaultStr(j.Btn, "left"), j.Double)
		return map[string]interface{}{"hit": hits[0]}, nil

	case "winctl":
		hwnd, _, has := jobTarget(j)
		if !has {
			return nil, fmt.Errorf("winctl op 需要目标窗口（target 锁定或本 job 的 hwnd/title）")
		}
		return winctlCore(hwnd, j.Rect, j.State)

	case "get_pixel":
		sx, sy, _, err := jobCoords(j, j.X, j.Y)
		if err != nil {
			return nil, err
		}
		return getPixelCore(sx, sy)

	case "wait_stable":
		info, err := waitStableCore(j.Hwnd, j.Title, "", j.Timeout, j.Interval, j.StableMs, j.Ratio)
		return info, err

	case "wait_text", "wait_color", "wait_image", "wait_gone":
		return waitGeneric(j, exeDir)
	}
	return nil, fmt.Errorf("未知 batch op %q", j.Op)
}

// activateIfTarget 输入类 op 前台对齐：目标窗口存在时先激活再发键鼠，
// 防止前序动作（OCR 探测/弹窗/失焦）改变前台焦点导致 key/wheel 打偏到别的窗口
func activateIfTarget(j *BatchJob) {
	if hwnd, _, has := jobTarget(j); has {
		activateWindow(hwnd)
	}
}

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func removeVk(list []uint16, vk uint16) []uint16 {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == vk {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// dragJob 平滑拖拽：两端点按统一防误触规则换算（窗口模式双端越界强校验），按下→分段移动→抬起
func dragJob(j *BatchJob) error {
	sx1, sy1, hwnd, err := jobCoords(j, j.X, j.Y)
	if err != nil {
		return err
	}
	sx2, sy2, _, err := jobCoords(j, j.X2, j.Y2)
	if err != nil {
		return err
	}
	steps := j.Steps
	if steps < 2 {
		steps = 20
	}
	stepMs := j.StepMs
	if stepMs <= 0 {
		stepMs = 10
	}
	if hwnd != 0 {
		activateWindow(hwnd)
	}
	setCursor(sx1, sy1)
	time.Sleep(60 * time.Millisecond)
	down, up, _, _, _, _, ok := mouseButtons(j.Btn)
	if !ok {
		down, up = mouseLeftDown, mouseLeftUp
	}
	sendMouse(down, 0)
	time.Sleep(60 * time.Millisecond)
	for i := 1; i <= steps; i++ {
		x := sx1 + (sx2-sx1)*i/steps
		y := sy1 + (sy2-sy1)*i/steps
		setCursor(x, y)
		time.Sleep(time.Duration(stepMs) * time.Millisecond)
	}
	sendMouse(up, 0)
	time.Sleep(30 * time.Millisecond)
	return nil
}

// probeTextHits 截屏 OCR 并过滤 query；返回屏幕坐标已换算的命中点（点击可直接用）
func probeTextHits(j *BatchJob, exeDir, query string, all bool) ([]Hit, int, int, RECT, bool, error) {
	ws, err := shotSmart(j.Hwnd, j.Title, "", true)
	if err != nil {
		return nil, 0, 0, RECT{}, false, err
	}
	boxes, _, err := ocrRecognize(exeDir, ws.Img, ocrOpts{Query: query, All: all, Angle: j.Angle, Early: !all})
	if err != nil {
		return nil, 0, 0, ws.Rect, false, err
	}
	_, _, has := jobTarget(j)
	var hits []Hit
	for _, b := range boxes {
		if all || strings.Contains(b.Text, query) {
			h := Hit{X: b.Cx, Y: b.Cy, W: b.W, H: b.H, Score: int(b.Score * 100), Count: 1, Text: b.Text}
			hits = append(hits, toScreenHits([]Hit{h}, ws.OX, ws.OY)[0])
		}
	}
	return hits, ws.OX, ws.OY, ws.Rect, has, nil
}

func probeColorHits(j *BatchJob, colorStr string, tol int) ([]Hit, int, int, RECT, bool, error) {
	r, g, b, err := parseColorSpec(colorStr)
	if err != nil {
		return nil, 0, 0, RECT{}, false, err
	}
	if tol <= 0 {
		tol = 10
	}
	ws, err := shotSmart(j.Hwnd, j.Title, "", true)
	if err != nil {
		return nil, 0, 0, RECT{}, false, err
	}
	_, _, has := jobTarget(j)
	hits := findColor(ws.Img, r, g, b, tol, 5)
	return toScreenHits(hits, ws.OX, ws.OY), ws.OX, ws.OY, ws.Rect, has, nil
}

func probeImageHits(j *BatchJob, tplPath string, tol int) ([]Hit, int, int, RECT, bool, error) {
	if tplPath == "" {
		return nil, 0, 0, RECT{}, false, fmt.Errorf("需要 tpl 模板图路径")
	}
	tpl, err := loadPNGImage(tplPath)
	if err != nil {
		return nil, 0, 0, RECT{}, false, fmt.Errorf("模板读取失败: %v", err)
	}
	if tol <= 0 {
		tol = 20
	}
	ws, err := shotSmart(j.Hwnd, j.Title, "", true)
	if err != nil {
		return nil, 0, 0, RECT{}, false, err
	}
	_, _, has := jobTarget(j)
	hits := findImage(ws.Img, tpl, tol, 5)
	return toScreenHits(hits, ws.OX, ws.OY), ws.OX, ws.OY, ws.Rect, has, nil
}

// probeFor wait_* 统一探测（探测本身失败视为未命中，继续轮询到超时）
func probeFor(op string, j *BatchJob, exeDir string) ([]Hit, int, int, RECT, bool) {
	var hits []Hit
	var ox, oy int
	var rc RECT
	var w bool
	var err error
	switch op {
	case "wait_text", "wait_gone":
		hits, ox, oy, rc, w, err = probeTextHits(j, exeDir, j.Text, j.All)
	case "wait_color":
		hits, ox, oy, rc, w, err = probeColorHits(j, j.Color, j.Tol)
	case "wait_image":
		hits, ox, oy, rc, w, err = probeImageHits(j, j.Tpl, j.Tol)
	}
	if err != nil {
		return nil, 0, 0, rc, w
	}
	return hits, ox, oy, rc, w
}

// waitGeneric wait_* 引擎：轮询直到条件满足或超时；wait_gone = 等文字/颜色/模板消失（加载完成判定）
func waitGeneric(j *BatchJob, exeDir string) (map[string]interface{}, error) {
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = 30000
	}
	interval := j.Interval
	if interval <= 0 {
		interval = 300
		if j.Op == "wait_text" || j.Op == "wait_gone" {
			interval = 800 // OCR 每轮要跑推理，间隔放大
		}
	}
	gone := j.Op == "wait_gone"
	deadline := time.Now().Add(time.Duration(timeout) * time.Millisecond)
	st := time.Now()
	var hits []Hit
	var ox, oy int
	var rc RECT
	var windowed bool
	satisfied := false
	rounds := 0
	for {
		rounds++
		hits, ox, oy, rc, windowed = probeFor(j.Op, j, exeDir)
		satisfied = (len(hits) > 0) != gone
		if satisfied {
			break
		}
		rest := time.Until(deadline)
		if rest <= 0 {
			break
		}
		d := time.Duration(interval) * time.Millisecond
		if rest < d {
			d = rest
		}
		time.Sleep(d)
	}
	elapsed := time.Since(st).Milliseconds()
	info := map[string]interface{}{
		"op": j.Op, "found": len(hits) > 0, "gone_mode": gone,
		"elapsed_ms": elapsed, "rounds": rounds, "timeout_ms": timeout,
	}
	if !satisfied {
		return info, fmt.Errorf("%s 在 %dms 内未满足（轮询 %d 轮）", j.Op, timeout, rounds)
	}
	if hits == nil {
		hits = []Hit{}
	}
	info["hits"] = hits
	if j.Click && len(hits) > 0 {
		clickHit(hits[0], ox, oy, rc, windowed, j.Bg, defaultStr(j.Btn, "left"), j.Double)
		info["clicked"] = true
	}
	return info, nil
}

// loadBatchJobs 三种数据源：文件路径 / '-' (stdin) / '[...]' JSON 字面量
func loadBatchJobs(src string) ([]BatchJob, error) {
	s := strings.TrimSpace(src)
	var data []byte
	var err error
	switch {
	case s == "":
		return nil, fmt.Errorf("batch 需要 --jobs（文件路径 / '-' stdin / '[...]' 字面）")
	case s == "-":
		data, err = io.ReadAll(os.Stdin)
	case strings.HasPrefix(s, "["):
		data = []byte(src)
	default:
		data, err = os.ReadFile(src)
	}
	if err != nil {
		return nil, err
	}
	var jobs []BatchJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("jobs JSON 解析失败: %v", err)
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("jobs 为空")
	}
	return jobs, nil
}
