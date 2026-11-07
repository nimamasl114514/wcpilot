// doctor.go —— 环境自检（v8 新增）：一条命令说清「为什么这台机器上跑不起来 / 哪里会慢」
//
// AI 代理最怕的不是报错，而是「静默降级后结果不对」。doctor 把所有隐式依赖摊开：
// 系统/架构、DPI 感知、虚拟屏范围、管理员权限、python 与 rapidocr、OCR 守护进程状态、
// 目标锁定、并行度、缓存目录。
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func cmdDoctor(exeDir string) {
	rep := map[string]interface{}{"ok": true}
	rep["version"] = version
	rep["go"] = runtime.Version()
	rep["arch"] = runtime.GOARCH
	rep["os"] = runtime.GOOS
	rep["exe"] = exeDir
	rep["threads"] = threads()
	rep["dpi_mode"] = dpiMode

	vx, vy, vw, vh := virtualScreenRect()
	rep["virtual_screen"] = map[string]int{"x": vx, "y": vy, "w": vw, "h": vh}

	// 目标锁定
	if t, ok := loadTarget(); ok && isWindow(t.Hwnd) {
		tgt := map[string]interface{}{
			"hwnd": t.Hwnd, "title": windowText(t.Hwnd), "class": className(t.Hwnd),
			"visible_rect": rectMap(visibleRect(t.Hwnd)),
		}
		for k, v := range winStateInfo(t.Hwnd) {
			tgt[k] = v
		}
		if isIconic(t.Hwnd) {
			tgt["warning"] = "窗口已最小化，visible_rect 不可用"
		}
		rep["target"] = tgt
	} else {
		rep["target"] = nil
	}

	// python / OCR
	pyNote := map[string]interface{}{}
	py, err := findPython()
	if err != nil {
		pyNote["error"] = err.Error()
	} else {
		pyNote["path"] = py
		pyNote["rapidocr"] = probeRapidOcr(py)
	}
	if _, err := ocrHelperPath(exeDir); err != nil {
		pyNote["helper"] = err.Error()
	} else {
		pyNote["helper"] = "ok"
	}
	if st, alive := ocrDaemonStatus(); len(st) > 0 {
		st["alive"] = alive
		pyNote["daemon"] = st
	} else {
		pyNote["daemon"] = map[string]interface{}{"alive": false, "note": "尚未启动（首次 OCR 时自动拉起）"}
	}
	if v := os.Getenv("WCPILOT_OCR"); v != "" {
		pyNote["ocr_mode_env"] = v
	}
	if v := os.Getenv("WCPILOT_PYTHON"); v != "" {
		pyNote["python_env"] = v
	}
	rep["ocr"] = pyNote

	// 缓存
	cdir := ocrCacheDir()
	entries, _ := os.ReadDir(cdir)
	rep["ocr_cache"] = map[string]interface{}{"dir": cdir, "files": len(entries)}

	// 建议
	var tips []string
	if m, _ := rep["ocr"].(map[string]interface{}); m != nil {
		if s, ok := m["rapidocr"].(string); ok && s != "ok" {
			tips = append(tips, "OCR 不可用："+s)
		}
	}
	if len(tips) == 0 {
		tips = append(tips, "环境正常；find_text/wait_text 走 OCR 守护进程，首次调用会有一次模型加载")
	}
	rep["tips"] = tips
	jout(rep)
}

// probeRapidOcr 用子进程探测 python 是否装了 rapidocr（不 import 到本进程，GO 侧无 python 绑定）
func probeRapidOcr(py string) string {
	cmd := exec.Command(py, "-c", "import rapidocr_onnxruntime,sys;sys.stdout.write(rapidocr_onnxruntime.__version__ if hasattr(rapidocr_onnxruntime,'__version__') else 'ok')")
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if strings.Contains(s, "ModuleNotFoundError") || strings.Contains(s, "ImportError") {
			return "missing (pip install rapidocr-onnxruntime)"
		}
		return "error: " + trimStr(s, 120)
	}
	if s == "" {
		s = "ok"
	}
	return s
}

// cmdOcrStatus 查守护进程
func cmdOcrStatus() {
	st, alive := ocrDaemonStatus()
	if len(st) == 0 {
		jout(map[string]interface{}{"ok": true, "running": false,
			"note": "未发现守护进程（首次 OCR 时自动拉起）"})
		return
	}
	st["running"] = alive
	jout(map[string]interface{}{"ok": true, "info": st})
}

// cmdOcrStop 主动停守护进程
func cmdOcrStop() {
	ok := stopOcrDaemon()
	jout(map[string]interface{}{"ok": true, "stopped": ok})
}

// startTime 供未来计时输出用（保留）
var startTime = time.Now()

// exeDirOf 返回可执行文件目录（doctor/ocr 辅助脚本定位用）
func exeDirOf() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(p)
}
