// stable.go —— wait_stable（v8 新增）：等画面「真的不动了」
//
// 自动化里最脆的一环是 sleep 猜时间：给少了截到半渲染帧，给多了白等几秒。
// wait_stable 用连续两帧的像素差异做判据：
//
//	· diff_ratio <= tol 且已经持续 >= stable_ms  → 判定稳定，立即返回（附耗时）
//	· 超时仍未稳定 → 返回错误 + 最后一轮的差异比例（调用方可据此决定是否硬上）
//
// 全内存比较，不落盘、不做 PNG 编码。
package main

import (
	"fmt"
	"time"
)

// waitStableCore 容错核心（CLI 与 batch 共用）
func waitStableCore(hwndFlag uint, titleFlag, regionFlag string, timeout, interval, stableMs int, tol float64) (map[string]interface{}, error) {
	if timeout <= 0 {
		timeout = 5000
	}
	if interval <= 0 {
		interval = 200
	}
	if stableMs <= 0 {
		stableMs = interval * 2
	}
	if tol <= 0 {
		tol = 0.001
	}
	st := time.Now()
	deadline := st.Add(time.Duration(timeout) * time.Millisecond)

	prev, err := shotSmart(hwndFlag, titleFlag, regionFlag, true)
	if err != nil {
		return nil, err
	}
	lastDiff := 1.0
	var sinceStable time.Time
	rounds := 1
	for {
		rest := time.Until(deadline)
		if rest <= 0 {
			return map[string]interface{}{
				"stable": false, "rounds": rounds,
				"elapsed_ms":      time.Since(st).Milliseconds(),
				"last_diff_ratio": lastDiff, "timeout_ms": timeout, "tol": tol,
			}, fmt.Errorf("画面在 %dms 内未稳定（最后差异比例 %.4f，阈值 %.4f）", timeout, lastDiff, tol)
		}
		d := time.Duration(interval) * time.Millisecond
		if rest < d {
			d = rest
		}
		time.Sleep(d)

		cur, err := shotSmart(hwndFlag, titleFlag, regionFlag, true)
		if err != nil {
			return nil, err
		}
		rounds++
		diff := imgDiffRatio(prev.Img, cur.Img)
		lastDiff = diff
		if diff <= tol {
			if sinceStable.IsZero() {
				sinceStable = time.Now()
			}
			if time.Since(sinceStable) >= time.Duration(stableMs)*time.Millisecond {
				return map[string]interface{}{
					"stable": true, "rounds": rounds,
					"elapsed_ms": time.Since(st).Milliseconds(),
					"stable_ms":  stableMs, "diff_ratio": diff, "tol": tol,
					"width": cur.Img.Bounds().Dx(), "height": cur.Img.Bounds().Dy(),
					"origin_x": cur.OX, "origin_y": cur.OY,
				}, nil
			}
		} else {
			sinceStable = time.Time{}
		}
		prev = cur
	}
}

// cmdWaitStable CLI 层
func cmdWaitStable(hwndFlag uint, titleFlag, regionFlag string, timeout, interval, stableMs int, tol float64) {
	info, err := waitStableCore(hwndFlag, titleFlag, regionFlag, timeout, interval, stableMs, tol)
	if err != nil {
		jout(map[string]interface{}{"ok": false, "result": info, "error": err.Error()})
		osExit(exitFail)
	}
	jout(map[string]interface{}{"ok": true, "result": info})
}
