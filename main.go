// wcpilot —— AI 驱动的 Windows 电脑操作工具（截图/点击/输入/找字/找色/找图，支持窗口限定防误触）
//
// 设计原则：
//  1. 一次调用一个动作，输出单行 JSON——方便 AI 代理用 shell 串起来，也方便人肉调试。
//  2. 纯 Go 标准库，syscall 直调 user32/gdi32/dwmapi，零第三方依赖，单 exe 可拷走。
//  3. 防误触优先：只要锁定了窗口，坐标一律按「窗口内相对坐标」解释，越界直接拒绝执行。
//  4. 参数错了就明确报错（exit 2），运行失败给可执行的原因（exit 1），绝不含糊其辞。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 退出码约定（写脚本时可直接判断）
const (
	exitOK    = 0
	exitFail  = 1 // 运行失败（找不到窗口 / 越界 / OCR 超时 …）
	exitUsage = 2 // 参数或用法错误
)

func jout(v map[string]interface{}) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func die(format string, a ...interface{}) {
	jout(map[string]interface{}{"ok": false, "error": fmt.Sprintf(format, a...)})
	os.Exit(exitFail)
}

func usageErr(format string, a ...interface{}) {
	jout(map[string]interface{}{"ok": false, "error": fmt.Sprintf(format, a...), "usage": "wcpilot help"})
	os.Exit(exitUsage)
}

func osExit(code int) { os.Exit(code) }

// traceOn / trace 性能追踪开关：WCPILOT_TRACE=1 时把各阶段耗时打到 stderr，
// stdout 仍只有一行结果 JSON（不破坏调用方解析）。
var traceOn = os.Getenv("WCPILOT_TRACE") != ""

func trace(format string, a ...interface{}) {
	if !traceOn {
		return
	}
	fmt.Fprintf(os.Stderr, "[trace] "+format+"\n", a...)
}

func usage() {
	fmt.Println(banner)
	fmt.Println(versionString())
	fmt.Print(`
用法: wcpilot <子命令> [选项]        （所有输出均为单行 JSON）

窗口与画面
  windows       列出可见窗口 [--keyword 关键字]（过滤标题/类名）
  target        锁定后续操作的默认窗口 [--hwnd N | --title 关键字 | --clear]
  activate      激活窗口到前台 [--hwnd N | --title 关键字]
  wininfo       窗口几何诊断（整窗/可见框/客户区/DPI 三口径对照）
  win_tree      控件树（Win32 子窗口：class/text/rect/可见/可用）[--depth 3] [--text 过滤] [--class 过滤]
  winctl        移动/缩放/最大化/最小化/置顶 [--rect x,y,w,h] [--state max|min|normal|top|notop]
  shot          截图 [-o out.png] [--region x,y,w,h] [--bg] [--grid 50] [--no-annotate]
  annotate      给已有 PNG 叠加坐标网格/十字/信息面板 --in x.png [-o out.png]
  get_pixel     读屏幕像素 --x N --y N（被遮挡时读到遮挡物；读窗口内容用 find_color/shot）

鼠标与键盘
  click         点击 --x N --y N [--btn left|right|middle] [--double] [--bg]
  dblclick      双击（= click --double）
  move          移动鼠标 --x N --y N
  wheel         滚轮 --delta N（正上负下，120 一档）[--x --y] [--bg]
  drag          平滑拖拽 --x --y --x2 --y2 [--steps 20] [--step_ms 10] [--btn]
  key           组合键 --key "ctrl+c" [--bg]
  keydown       按住键 --key ctrl（配合 move/click 做多选/框选）
  keyup         释放键 --key ctrl
  type          输入文本 --text "内容" [--bg] [--no-restore-clipboard]
  get_clipboard 读剪贴板文本
  set_clipboard 写剪贴板文本 --text "..."

查找与等待（定位目标）
  find_color    找颜色 --color "#FF8800" [--tol 15] [--region ..] [--click] [-o found.png]
  find_image    找图   --tpl 模板.png [--tol 20] [--region ..] [--click] [-o found.png]
  find_text     OCR 找字 --text "确定" [--region ..] [--click] [--all] [--angle] [-o found.png]
  wait_text     等文字出现（同含 --click/--timeout/--interval）
  wait_color    等颜色出现
  wait_image    等模板图出现
  wait_gone     等文字/颜色/图像消失（加载完成判定）
  wait_stable   等画面稳定（连续两帧差异 <= --ratio 且持续 --stable_ms）

批量编排
  batch         复合动作编排 --jobs ops.json（或 '-' stdin / '[...]' 字面）
                [--continue-on-error] [--evidence DIR] [--shots]
                op: click|move|wheel|key|keydown|keyup|type|drag|sleep|winctl|get_pixel|
                    click_text|click_color|click_image|wait_text|wait_color|wait_image|wait_gone|wait_stable

环境
  doctor        环境自检（DPI/python/OCR 守护进程/缓存/并行度/目标锁定）
  ocr_status    查看 OCR 守护进程状态
  ocr_stop      停止 OCR 守护进程
  version       版本信息
  help          本帮助（wcpilot help <子命令> 看该命令详解）

坐标与防误触（重要）
  · 带 --hwnd / --title / 已 target 锁定时：--x/--y 一律是「窗口可见区内的相对坐标」，
    越界直接拒绝执行——这是防误触的核心，不会误点别的窗口。
  · 不带任何窗口时：--x/--y 是整块虚拟屏的绝对坐标（多屏原点可为负）。
  · 窗口口径 = DWM 可见框（不含 Windows 的不可见 resize 边框），与截图图像素一一对应。

后台模式 --bg（不激活窗口 / 不动真实鼠标 / 不抢物理键盘）
  · click   投 WM_MOUSEMOVE + 按下/抬起（自动下钻到最深的子控件）
  · key/type 投 WM_KEYDOWN·UP / WM_CHAR（lParam 带扫描码与扩展位）
  · wheel   投 WM_MOUSEWHEEL
  · 消息注入的兼容性因程序而异（自绘/游戏/部分 Electron 不认），务必用截图验证状态变化。

环境变量
  WCPILOT_PYTHON       指定 python 解释器（装了 rapidocr-onnxruntime 的那个）
  WCPILOT_OCR          auto(默认) | daemon(强制守护进程) | spawn(每次起进程)
  WCPILOT_OCR_IDLE     守护进程空闲退出秒数（默认 600，0 = 不退出）
  WCPILOT_OCR_CACHE    off 关闭 OCR 结果缓存
  WCPILOT_THREADS      并行查找线程数（默认 CPU 核数）
  WCPILOT_PNG          default | best | none：PNG 压缩档位（默认 BestSpeed）
`)
	os.Exit(exitOK)
}

// cmdHelp 子命令详解
func cmdHelp(topic string) {
	topic = strings.ToLower(strings.TrimSpace(topic))
	docs := map[string]string{
		"batch": `batch —— 复合动作编排

  wcpilot batch --jobs ops.json [--continue-on-error] [--evidence DIR] [--shots]

jobs 可以是文件路径、'-'（读 stdin）或直接写成 JSON 数组字面量。
每个 job 是对象，字段按 op 取用：

  op         click|move|wheel|key|keydown|keyup|type|drag|sleep|winctl|get_pixel|
             click_text|click_color|click_image|wait_text|wait_color|wait_image|wait_gone|wait_stable
  x,y        主坐标（窗口锁定时 = 窗口内相对坐标，越界该项失败）
  x2,y2      drag 终点
  btn        left|right|middle（默认 left）
  double     双击
  text       type 的文本 / OCR 查找词
  key        组合键 "ctrl+c" 或单键名
  delta      滚轮增量（120 = 一格）
  color      #RRGGBB
  tpl        模板图路径
  tol        容差
  delay      本动作前的延迟 ms
  ms         sleep 时长 ms
  steps      拖拽分段数（默认 20）
  step_ms    拖拽每段间隔
  timeout_ms / interval_ms   wait_* 的超时与轮询间隔
  ratio      wait_stable 的差异阈值（0~1，默认 0.001）
  stable_ms  wait_stable 需要保持稳定的时长
  click      wait_* 命中后点击首个命中点
  bg         本动作走后台消息注入
  hwnd/title 本动作临时指定窗口（覆盖全局 target 锁定）
  rect/state winctl 参数
  on_error   stop(默认) | continue
  comment    备注，只进结果 JSON

行为：默认「失败即短路」；成功/失败都会返回逐 job 的 {i,op,ok,ms,info}。
结束时自动释放所有 keydown 按住的键（防卡键）。`,
		"find_text": `find_text —— OCR 找文字

  wcpilot find_text --text "确定" [--region x,y,w,h] [--click] [--all] [--angle] [-o out.png]

· 默认命中即停（early-stop）：只找一个词时不必识别整屏，全屏 168 个文本块从 13s 降到 1~3s
· 需要同名词的全部命中时加 --all（会完整识别，慢）
· --angle 打开方向分类（竖排/旋转文字），会更慢
· 结果里的 x/y 是截图内坐标，screen_x/screen_y 是屏幕绝对坐标
· 截图范围越小越快：优先配合 --hwnd / target 锁定 / --region 使用`,
		"shot": `shot —— 截图

  wcpilot shot [-o out.png] [--hwnd N | --title 词] [--region x,y,w,h] [--bg]
               [--grid 50] [--no-annotate] [--cursor=false]

· 窗口截图走 PrintWindow(PW_RENDERFULLCONTENT)，被遮挡也能截到窗口自身内容
· 截图尺寸按 DWM 可见框裁剪（不含 Windows 不可见边框），与窗口内坐标严格对齐
· 默认叠加坐标网格 + 鼠标十字 + 左上信息面板，--no-annotate 关闭
· 输出 JSON 的 origin_x/origin_y 是图像 (0,0) 对应的屏幕坐标`,
		"wait_stable": `wait_stable —— 等画面稳定

  wcpilot wait_stable [--timeout 5000] [--interval 200] [--stable_ms 400] [--ratio 0.001]
                      [--hwnd N | --region x,y,w,h]

比 sleep 可靠：连续两帧像素差异比例 <= --ratio 且已持续 --stable_ms 才返回。
超时会给出最后的差异比例，便于判断是「还在渲染」还是「有别的东西在动」。`,
	}
	if topic == "" || topic == "help" {
		usage()
	}
	if d, ok := docs[topic]; ok {
		fmt.Println(d)
		os.Exit(exitOK)
	}
	// 没有详解的子命令：给出该行用法
	fmt.Printf("该子命令暂无详解，见总览：wcpilot help  （命令：%s）\n", topic)
	os.Exit(exitOK)
}

func main() {
	dpiMode = enableDpiAwareness() // 必须最先执行：否则高分屏截图只截到左上角
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	switch cmd {
	case "help", "--help", "-h":
		if len(os.Args) > 2 {
			cmdHelp(os.Args[2])
		}
		usage()
	case "version", "--version", "-v":
		fmt.Println(versionString())
		os.Exit(exitOK)
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		pHwnd     = fs.Uint("hwnd", 0, "窗口句柄(十进制)")
		pTitle    = fs.String("title", "", "窗口标题关键字")
		pKeyword  = fs.String("keyword", "", "窗口过滤关键字")
		pOut      = fs.String("o", "", "输出文件")
		pRegion   = fs.String("region", "", "区域 x,y,w,h")
		pX        = fs.Int("x", 0, "x 坐标")
		pY        = fs.Int("y", 0, "y 坐标")
		pBtn      = fs.String("btn", "left", "按键 left|right|middle")
		pDouble   = fs.Bool("double", false, "双击")
		pBg       = fs.Bool("bg", false, "后台模式(不激活/不动真实鼠标)")
		pClear    = fs.Bool("clear", false, "清除目标锁定")
		pKey      = fs.String("key", "", "组合键")
		pText     = fs.String("text", "", "文本 / 查找词 / 控件过滤词")
		pClass    = fs.String("class", "", "win_tree 类名过滤")
		pDepth    = fs.Int("depth", 3, "win_tree 最大深度（0=不限）")
		pColor    = fs.String("color", "", "颜色 #RRGGBB 或 r,g,b")
		pTpl      = fs.String("tpl", "", "模板图片路径")
		pTol      = fs.Int("tol", 10, "容差")
		pAll      = fs.Bool("all", false, "返回全部命中")
		pClick    = fs.Bool("click", false, "找到后点击首个命中")
		pAngle    = fs.Bool("angle", false, "OCR 方向分类（慢，竖排文字才需要）")
		pDelta    = fs.Int("delta", 0, "滚轮增量")
		pMax      = fs.Int("max", 10, "最多返回命中数")
		pJobs     = fs.String("jobs", "", "batch 任务清单 JSON（路径 / '-' stdin / '[...]' 字面）")
		pTimeout  = fs.Int("timeout", 30000, "wait_* 超时毫秒")
		pInterval = fs.Int("interval", 0, "wait_* 轮询间隔毫秒(0=默认)")
		pX2       = fs.Int("x2", 0, "drag 终点 x")
		pY2       = fs.Int("y2", 0, "drag 终点 y")
		pSteps    = fs.Int("steps", 20, "drag 分段数")
		pStepMs   = fs.Int("step_ms", 10, "drag 每段间隔毫秒")
		pRect     = fs.String("rect", "", "winctl 目标矩形 x,y,w,h（屏幕绝对坐标）")
		pState    = fs.String("state", "", "winctl 状态 max|min|normal|top|notop")
		pCoE      = fs.Bool("continue-on-error", false, "batch 某步失败继续执行")
		pGrid     = fs.Int("grid", 50, "shot/annotate 坐标网格步长（0=不带网格）")
		pNoAnnot  = fs.Bool("no-annotate", false, "shot 关闭全部可视化标注")
		pCursor   = fs.Bool("cursor", true, "shot/annotate 标注鼠标位置十字")
		pIn       = fs.String("in", "", "annotate 输入 PNG 路径")
		pEvidence = fs.String("evidence", "", "batch 证据链目录（失败自动留现场截图+上下文）")
		pShots    = fs.Bool("shots", false, "batch 每个动作前后截图存证（时间线）")
		pStableMs = fs.Int("stable_ms", 0, "wait_stable 需要保持稳定的时长毫秒")
		pRatio    = fs.Float64("ratio", 0.001, "wait_stable 连续帧差异阈值 0~1")
		pNoRest   = fs.Bool("no-restore-clipboard", false, "type 不恢复剪贴板（默认恢复）")
	)
	if err := fs.Parse(os.Args[2:]); err != nil {
		usageErr("%v", err)
	}
	exeDir := exeDirOf()

	switch cmd {
	case "windows":
		cmdWindows(*pKeyword)
	case "activate":
		hwnd, _, has := resolveTarget(*pHwnd, *pTitle)
		if !has {
			usageErr("activate 需要 --hwnd 或 --title")
		}
		activateWindow(hwnd)
		jout(map[string]interface{}{"ok": true, "hwnd": hwnd, "title": windowText(hwnd)})
	case "target":
		cmdTarget(*pHwnd, *pTitle, *pClear)
	case "shot":
		var opt annotateOpt
		if !*pNoAnnot {
			opt.Grid = *pGrid > 0
			opt.GridStep = *pGrid
			opt.Cursor = *pCursor
		}
		cmdShot(*pHwnd, *pTitle, *pRegion, *pOut, *pBg, opt)
	case "annotate":
		cmdAnnotate(*pIn, *pOut, *pGrid, *pCursor)
	case "wininfo":
		cmdWinInfo(*pHwnd, *pTitle)
	case "win_tree":
		cmdWinTree(*pHwnd, *pTitle, *pText, *pClass, *pDepth)
	case "click", "dblclick":
		if cmd == "dblclick" {
			*pDouble = true
		}
		cmdClick(*pHwnd, *pTitle, *pX, *pY, *pBtn, *pDouble, *pBg)
	case "move":
		x, y, hwnd := absCoords(*pHwnd, *pTitle, *pX, *pY)
		_ = hwnd
		setCursor(x, y)
		jout(map[string]interface{}{"ok": true, "screen_x": x, "screen_y": y})
	case "wheel":
		hwnd, _, has := resolveTarget(*pHwnd, *pTitle)
		hasXY := isFlagSet(fs, "x") || isFlagSet(fs, "y")
		if *pBg {
			if !has {
				usageErr("wheel --bg 需要目标窗口：--hwnd / --title / 先 target 锁定")
			}
			sx, sy := 0, 0
			if hasXY {
				sx, sy, _, _ = tryCoords(*pHwnd, *pTitle, *pX, *pY)
			}
			tgt, cls := postWheel(hwnd, *pDelta, sx, sy)
			jout(map[string]interface{}{"ok": true, "delta": *pDelta, "mode": "bg_postmessage",
				"target_hwnd": tgt, "target_class": cls})
			return
		}
		if hasXY {
			sx, sy, h := absCoords(*pHwnd, *pTitle, *pX, *pY)
			doWheel(*pDelta, sx, sy, true, h)
			jout(map[string]interface{}{"ok": true, "delta": *pDelta, "screen_x": sx, "screen_y": sy})
			return
		}
		doWheel(*pDelta, 0, 0, false, hwnd)
		jout(map[string]interface{}{"ok": true, "delta": *pDelta})
	case "key":
		if *pKey == "" {
			usageErr("key 需要 --key 参数")
		}
		if *pBg {
			hwnd, _, has := resolveTarget(*pHwnd, *pTitle)
			if !has {
				usageErr("key --bg 需要目标窗口：--hwnd / --title / 先 target 锁定")
			}
			via, err := postKeyCombo(hwnd, *pKey)
			if err != nil {
				die("%v", err)
			}
			tgt := keyboardTarget(hwnd)
			jout(map[string]interface{}{"ok": true, "key": *pKey, "mode": "bg_postmessage",
				"via": via, "target_hwnd": tgt, "target_class": className(tgt)})
			return
		}
		if err := pressKeys(*pKey); err != nil {
			die("%v", err)
		}
		jout(map[string]interface{}{"ok": true, "key": *pKey})
	case "type":
		if *pText == "" {
			usageErr("type 需要 --text 参数")
		}
		hwnd, _, has := resolveTarget(*pHwnd, *pTitle)
		if *pBg {
			if !has {
				usageErr("type --bg 需要目标窗口：--hwnd / --title / 先 target 锁定")
			}
			tgt, cls := postTypeText(hwnd, *pText)
			jout(map[string]interface{}{"ok": true, "typed": *pText, "mode": "bg_postmessage",
				"target_hwnd": tgt, "target_class": cls})
			return
		}
		if has {
			activateWindow(hwnd)
		}
		clip, plain := typeText(*pText, !*pNoRest)
		jout(map[string]interface{}{"ok": true, "typed": *pText, "via_clipboard": clip, "via_vk": plain})
	case "find_color":
		cmdFindColor(*pHwnd, *pTitle, *pRegion, *pColor, *pTol, *pMax, *pAll, *pClick, *pBg, *pOut)
	case "find_image":
		cmdFindImage(*pHwnd, *pTitle, *pRegion, *pTpl, *pTol, *pMax, *pClick, *pBg, *pOut)
	case "find_text":
		cmdFindText(exeDir, *pHwnd, *pTitle, *pRegion, *pText, *pAll, *pClick, *pBg, *pOut)
	case "batch":
		jobs, err := loadBatchJobs(*pJobs)
		if err != nil {
			usageErr("%v", err)
		}
		runBatch(jobs, *pCoE, exeDir, buildEvidence(*pEvidence, *pShots))
	case "drag":
		if err := dragJob(&BatchJob{X: *pX, Y: *pY, X2: *pX2, Y2: *pY2, Btn: *pBtn, Steps: *pSteps, StepMs: *pStepMs}); err != nil {
			die("%v", err)
		}
		jout(map[string]interface{}{"ok": true, "drag": true, "from": [2]int{*pX, *pY}, "to": [2]int{*pX2, *pY2}})
	case "keydown", "keyup":
		if *pKey == "" {
			usageErr("%s 需要 --key 参数", cmd)
		}
		vk, err := vkOf(*pKey)
		if err != nil {
			die("%v", err)
		}
		if *pBg {
			hwnd, _, has := resolveTarget(*pHwnd, *pTitle)
			if !has {
				usageErr("%s --bg 需要目标窗口", cmd)
			}
			postKeySingle(hwnd, vk, cmd == "keydown")
			jout(map[string]interface{}{"ok": true, "key": *pKey, "down": cmd == "keydown", "mode": "bg_postmessage"})
			return
		}
		sendKey(vk, cmd == "keydown")
		jout(map[string]interface{}{"ok": true, "key": *pKey, "down": cmd == "keydown",
			"warn": "keydown 后请记得 keyup；batch 编排内会自动全释放，裸用不会"})
	case "wait_text", "wait_color", "wait_image", "wait_gone":
		job := &BatchJob{Op: cmd, Text: *pText, Color: *pColor, Tpl: *pTpl, Tol: *pTol,
			Timeout: *pTimeout, Interval: *pInterval, Click: *pClick, Angle: *pAngle}
		info, err := waitGeneric(job, exeDir)
		if err != nil {
			jout(map[string]interface{}{"ok": false, "result": info, "error": err.Error()})
			os.Exit(exitFail)
		}
		jout(map[string]interface{}{"ok": true, "result": info})
	case "wait_stable":
		if *pTimeout == 30000 { // 未显式指定时给更合理的 5s
			*pTimeout = 5000
		}
		cmdWaitStable(*pHwnd, *pTitle, *pRegion, *pTimeout, *pInterval, *pStableMs, *pRatio)
	case "get_clipboard":
		jout(map[string]interface{}{"ok": true, "text": getClipboardText()})
	case "set_clipboard":
		if err := setClipboardText(*pText); err != nil {
			die("%v", err)
		}
		jout(map[string]interface{}{"ok": true, "text_len": len([]rune(*pText))})
	case "winctl":
		cmdWinctl(*pHwnd, *pTitle, *pRect, *pState)
	case "get_pixel":
		cmdGetPixel(*pHwnd, *pTitle, *pX, *pY)
	case "doctor":
		cmdDoctor(exeDir)
	case "ocr_status":
		cmdOcrStatus()
	case "ocr_stop":
		cmdOcrStop()
	default:
		usageErr("未知子命令: %s", cmd)
	}
}

// isFlagSet 判断某个 flag 是否被显式指定（区分「--x 0」与「没给 --x」）
func isFlagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// tryCoords 非 die 版坐标换算（供 batch/wait 引擎逐动作容错使用）
func tryCoords(hwndFlag uint, titleFlag string, x, y int) (int, int, uintptr, error) {
	hwnd, rc, has := resolveTargetSafe(hwndFlag, titleFlag)
	if !has {
		return x, y, 0, nil
	}
	w, h := int(rc.Right-rc.Left), int(rc.Bottom-rc.Top)
	if x < 0 || y < 0 || x >= w || y >= h {
		return 0, 0, hwnd, fmt.Errorf("坐标 (%d,%d) 超出窗口 [%dx%d]，拒绝执行（防误触）", x, y, w, h)
	}
	return int(rc.Left) + x, int(rc.Top) + y, hwnd, nil
}

// absCoords 统一坐标换算：有窗口时 (x,y)=窗口内相对坐标 -> 屏幕绝对坐标，并做强校验
func absCoords(hwndFlag uint, titleFlag string, x, y int) (int, int, uintptr) {
	sx, sy, hwnd, err := tryCoords(hwndFlag, titleFlag, x, y)
	if err != nil {
		die("%v", err)
	}
	return sx, sy, hwnd
}

func cmdClick(hwndFlag uint, titleFlag string, x, y int, btn string, dbl, bg bool) {
	hwnd, rc, has := resolveTarget(hwndFlag, titleFlag)
	var sx, sy int
	if has {
		w, h := int(rc.Right-rc.Left), int(rc.Bottom-rc.Top)
		if x < 0 || y < 0 || x >= w || y >= h {
			die("坐标 (%d,%d) 超出窗口 [%dx%d]，拒绝执行（防误触）", x, y, w, h)
		}
		sx, sy = int(rc.Left)+x, int(rc.Top)+y
	} else {
		sx, sy = x, y
	}
	if bg && has {
		tgt, cls, depth := postClick(hwnd, x, y, btn, dbl)
		jout(map[string]interface{}{"ok": true, "mode": "bg_postmessage", "hwnd": hwnd,
			"target_hwnd": tgt, "target_class": cls, "child_depth": depth,
			"win_x": x, "win_y": y, "btn": btn, "double": dbl})
		return
	}
	if bg && !has {
		usageErr("click --bg 需要目标窗口：--hwnd / --title / 先 target 锁定")
	}
	if has {
		activateWindow(hwnd)
	}
	clickAt(sx, sy, btn, dbl)
	jout(map[string]interface{}{
		"ok": true, "screen_x": sx, "screen_y": sy, "btn": btn, "double": dbl,
		"hwnd": hwnd, "window_title": windowText(hwnd), "window_rect": rectMap(rc),
	})
}

func cmdShot(hwndFlag uint, titleFlag, regionFlag, out string, bg bool, annot annotateOpt) {
	if out == "" {
		out = fmt.Sprintf("shot_%d.png", time.Now().Unix())
	}
	t0 := time.Now()
	ws, err := shotSmart(hwndFlag, titleFlag, regionFlag, !bg)
	if err != nil {
		die("%v", err)
	}
	tCap := time.Since(t0)
	img := ws.Img
	annotated := false
	// 可视化标注只作用于输出 PNG，不污染 find_*/OCR 的内部图源
	if annot.Grid || annot.Cursor || len(annot.Boxes) > 0 {
		annot.Panel = append([]string{
			fmt.Sprintf("ORIGIN %d,%d", ws.OX, ws.OY),
			fmt.Sprintf("SIZE %dX%d", img.Bounds().Dx(), img.Bounds().Dy()),
			fmt.Sprintf("MODE %s SCALE %s", diagStr(ws.Diag, "mode"), diagStr(ws.Diag, "scale")),
		}, annot.Panel...)
		img = annotateImage(ws.Img, ws.OX, ws.OY, annot)
		annotated = true
	}
	tAnnot := time.Since(t0)
	if err := savePNG(out, img); err != nil {
		die("保存失败: %v", err)
	}
	trace("capture=%dms annotate=%dms encode=%dms total=%dms",
		tCap.Milliseconds(), (tAnnot - tCap).Milliseconds(),
		(time.Since(t0) - tAnnot).Milliseconds(), time.Since(t0).Milliseconds())
	abs, _ := filepath.Abs(out)
	cx, cy := cursorPos()
	rec := map[string]interface{}{
		"ok": true, "path": abs, "width": img.Bounds().Dx(), "height": img.Bounds().Dy(),
		"origin_x": ws.OX, "origin_y": ws.OY, "dpi_mode": dpiMode, "annotated": annotated,
		"cursor": []int{cx, cy}, // 截帧时的鼠标屏幕物理坐标（与图中红色十字互证）
	}
	if ws.Rect.Right > ws.Rect.Left { // 窗口/区域+窗口模式：可见框
		rec["window_rect"] = rectMap(ws.Rect)
	}
	for k, v := range ws.Diag { // 诊断：mode / dwm_ok / awareness / scale / dpi_scaled / fallback …
		rec[k] = v
	}
	jout(rec)
}

// diagStr 诊断字段转字符串（缺项给 "-"）
func diagStr(m map[string]interface{}, k string) string {
	if v, ok := m[k]; ok && v != nil {
		return fmt.Sprintf("%v", v)
	}
	return "-"
}

// cmdAnnotate 对已有 PNG 叠加可视化标注（网格 + 鼠标十字 + 面板）
func cmdAnnotate(in, out string, gridStep int, cursor bool) {
	if in == "" {
		usageErr("annotate 需要 --in 输入 PNG 路径")
	}
	img, err := loadPNGImage(in)
	if err != nil {
		die("读取失败: %v", err)
	}
	if out == "" {
		ext := filepath.Ext(in)
		out = strings.TrimSuffix(in, ext) + "_annotated" + ext
	}
	opt := annotateOpt{Grid: gridStep > 0, GridStep: gridStep, Cursor: cursor,
		Panel: []string{"ANNOTATED " + filepath.Base(in)}}
	res := annotateImage(img, 0, 0, opt)
	if err := savePNG(out, res); err != nil {
		die("保存失败: %v", err)
	}
	abs, _ := filepath.Abs(out)
	jout(map[string]interface{}{"ok": true, "path": abs,
		"width": res.Bounds().Dx(), "height": res.Bounds().Dy()})
}
