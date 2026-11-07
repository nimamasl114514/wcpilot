// evidence.go —— 操作证据链与失败现场留存（找 bug 用）
//
// 两类产物（batch 编排时启用）：
//
//	· 时间线（--shots <dir>）：每个 job 执行前后各截一图 + timeline.json 索引
//	· 现场（失败自动）：失败时刻截图 + fail*.json（job 定义 / 错误 / 窗口几何 / 截图原点 / DPI）
//
// 设计原则：证据链是旁路能力，任何落盘失败都不得打断主流程（静默降级）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// evidenceOpt 证据链配置（nil = 关闭）
type evidenceOpt struct {
	Dir    string
	Shots  bool // 每 job 前/后截图（时间线）
	OnFail bool // 失败落现场
}

// buildEvidence 组装证据链配置；未指定目录时用 <cwd>/wcpilot_evidence
func buildEvidence(dir string, shots bool) *evidenceOpt {
	if dir == "" && !shots {
		return nil
	}
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = filepath.Join(wd, "wcpilot_evidence")
		} else {
			dir = filepath.Join(os.TempDir(), "wcpilot_evidence")
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil
	}
	return &evidenceOpt{Dir: dir, Shots: shots, OnFail: true}
}

// captureInto 截当前锁定窗口到 dir/name.png，返回含几何诊断的元数据
func captureInto(dir, name string) map[string]interface{} {
	ws, err := shotSmart(0, "", "", true)
	if err != nil {
		return map[string]interface{}{"error": err.Error(), "at": time.Now().Format("15:04:05.000")}
	}
	fp := filepath.Join(dir, name+".png")
	if err := savePNG(fp, ws.Img); err != nil {
		return map[string]interface{}{"error": err.Error(), "at": time.Now().Format("15:04:05.000")}
	}
	meta := map[string]interface{}{
		"file":   name + ".png",
		"width":  ws.Img.Bounds().Dx(),
		"height": ws.Img.Bounds().Dy(),
		"origin": []int{ws.OX, ws.OY},
		"at":     time.Now().Format("15:04:05.000"),
	}
	if ws.Rect.Right > ws.Rect.Left {
		meta["window_rect"] = rectMap(ws.Rect)
	}
	for k, v := range ws.Diag {
		meta[k] = v
	}
	return meta
}

// writeJSON 落盘缩进 JSON；失败静默（证据链不得打断主流程）
func writeJSON(path string, v interface{}) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0644)
}

// saveScene 失败现场：截图 + 上下文（job / 错误 / 窗口三口径 / DPI）
func saveScene(ev *evidenceOpt, idx int, j *BatchJob, errMsg string, info map[string]interface{}) map[string]interface{} {
	if ev == nil || !ev.OnFail {
		return nil
	}
	base := fmt.Sprintf("fail%03d", idx)
	shot := captureInto(ev.Dir, base)
	ctx := map[string]interface{}{
		"at":         time.Now().Format("2006-01-02 15:04:05.000"),
		"job_index":  idx,
		"job":        j,
		"error":      errMsg,
		"info":       info,
		"screenshot": shot,
	}
	if hwnd, rc, has := resolveTarget(0, ""); has {
		dpiInfo := windowDpiInfo(hwnd)
		ctx["target"] = map[string]interface{}{
			"hwnd":         hwnd,
			"title":        windowText(hwnd),
			"class":        className(hwnd),
			"visible_rect": rectMap(rc),
			"window_rect":  rectMap(windowRect(hwnd)),
			"client_rect":  rectMap(clientRect(hwnd)),
			"monitor_dpi":  monitorDpi(hwnd),
			"awareness":    dpiInfo["awareness"],
		}
	}
	writeJSON(filepath.Join(ev.Dir, base+".json"), ctx)
	return map[string]interface{}{"context": base + ".json", "screenshot": shot}
}
