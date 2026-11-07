# 基准数据

数据来自一台 Windows 11 虚机（24 vCPU / 2560×1600 @150% DPI），**数字绝对值别当真，看比例**。
所有数据都能自己复现：

```bash
go test -run XXX -bench . -benchtime 20x          # 内核
WCPILOT_TRACE=1 wcpilot shot -o x.png              # 各阶段（stderr）
python ocrbench.py                                 # OCR 守护进程 vs 一次性
```

## 1. 查找内核（4MP ≈ 2560×1600 全屏）

| 基准 | 串行 | 并行（24 线程） | 提速 |
|---|---|---|---|
| `find_color` | 6.11 ms | **1.43 ms** | 4.3× |
| `find_image`（180×26 模板） | 5.66 ms | **0.83 ms** | 6.8× |

v7 的 `find_color` 在同样素材上 ≈ 10ms（`map` 逐像素累加）。所以找色这一层合计约 **7×**。

> 并行按行带切分，无锁；`WCPILOT_THREADS=1` 退化为串行，结果与并行逐字段一致（有单测）。

## 2. 图像编解码

| 项 | 耗时 | 说明 |
|---|---|---|
| BGRA → RGBA（4MP，原地） | **3.3 ms**（≈5 GB/s） | v7 逐通道字节赋值 ≈ 20 ms |
| PNG 编码 BestSpeed（4MP） | **14.7 ms** | v8 默认 |
| PNG 编码 Default（4MP） | **338.5 ms** | v7 行为 —— **23×** 差距 |
| PNG 解码（4MP，`draw.Draw`） | ~15 ms | v7 逐像素 `At()` ≈ 200 ms |
| BMP 写盘（4MP，OCR 中转） | 14.2 ms | 免压缩，cv2 解码也快 |
| 双线性放大 2MP→4MP | 47.2 ms | 只在 DPI 未感知窗口上触发 |

## 3. 单次子命令各阶段（`WCPILOT_TRACE=1`）

2560×1600 全屏：

| 命令 | capture | 计算 | encode | 合计 |
|---|---|---|---|---|
| `shot --no-annotate` | 21–26 ms | — | 16–18 ms | **38–44 ms** |
| `shot`（默认带网格标注） | 23 ms | 1 ms | 19 ms | **44 ms** |
| `find_color` | 55–65 ms | 2–5 ms | — | **60–70 ms** |
| `find_image` | 55–65 ms | 0–3 ms | — | **60–70 ms** |

> 内容越花，PNG 编码越久（同一命令在复杂桌面上 encode 可到 90–100 ms）。
> `shot` 首次遇到最小化窗口时会先 `SC_RESTORE` 并等重绘，capture 会到 ~500 ms，属正常。

## 4. OCR（本机最慢的一环，也是 v8 优化重点）

### 守护进程 vs 每次 spawn（1200×400 区域，同一查询连跑 3 次取均值）

| 通道 | 平均 | 备注 |
|---|---|---|
| `WCPILOT_OCR=daemon` | **3.55 s** | 省掉每次 model load |
| `WCPILOT_OCR=spawn`（v7 行为） | **8.15 s** | 每次 import + load ≈ 4.6 s |
| 命中内容缓存 | **20 ms** | 同一帧重复查询 |

### 早期停止（early-stop）的效果

同一屏 168 个文本块：

| 策略 | 耗时 |
|---|---|
| 全量识别（v7，`--all` 或找不到时） | ~13 s |
| 目标词在靠前位置（`scanned=1`） | **~1 s** |
| 目标词在靠后位置（`scanned=168`） | ~13 s |
| 加上 `--region` 把范围缩到 1/6 | 再降一档 |

结论：OCR 的成本 ≈ **文本块数量 × ~80 ms/块**（外加检测 ~4.6 s、方向分类 1.5 s）。
所以 v8 默认关掉方向分类、按需提前停止，并优先缩小截图范围。

> 这台机器上 onnxruntime 明显偏慢（同样任务在普通台式机上通常是 1/5~1/10）。
> 比例关系仍然成立：**冷启动 > 检测 > 识别 × 块数**。

## 5. 二进制

| 项 | 值 |
|---|---|
| `wcpilot.exe`（`-s -w -trimpath`） | ~3.6 MB |
| Go 第三方依赖 | 0 |
| 运行时依赖 | 可选：python + rapidocr-onnxruntime（仅 OCR） |
