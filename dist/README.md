# wcpilot

**让 AI 真正能操作你的 Windows 电脑** —— 截图、点击、输入、找字/找色/找图、等画面稳定，
全部收敛成「一次调用 → 一行 JSON」。窗口级防误触，单文件 exe，零第三方 Go 依赖。

```
$ wcpilot target --title Notepad
$ wcpilot click --x 300 --y 200
$ wcpilot type  --text "你好，世界"
$ wcpilot find_text --text "你好" --click
{"count":1,"hits":[{"x":145,"y":39,"text":"你好，世界","screen_x":784,"screen_y":224,...}],"ocr":{"mode":"daemon","ms":633}}
```

- Go 1.21+ · Windows 10/11 · 单 exe（约 5MB）· MIT
- 原生 syscall 调 `user32/gdi32/dwmapi`：PrintWindow 抓被遮挡窗口、SendInput 注入输入、
  PostMessage 后台注入（不抢焦点、不动真实鼠标）
- OCR 走 rapidocr（可选依赖），常驻守护进程复用模型，**每次调用省掉一次冷启动**

---

## 为什么不用现成方案

| 方案 | 问题 |
|---|---|
| pyautogui / autojson | 只有全屏坐标，一锁屏/切窗口就点错；中文输入要自己拼剪贴板 |
| AutoHotkey | 脚本语言，AI 生成易出错；JSON 输出要自己拼 |
| UI Automation / Playwright | 只覆盖支持无障碍树的程序；Electron/WPF/游戏大量拿不到元素 |
| 直接把截图丢给多模态模型 | 模型给的坐标经常差几十像素，且没有「防误触」闸门 |

wcpilot 的取舍：**像素级兜底（任何程序都能操作）+ 窗口级坐标围栏（绝不会误点别的窗口）+
机器可读输出（AI 一次调一个动作）**。

---

## 30 秒上手

```bash
# 1) 编译（需要 Go 1.21+）
git clone https://github.com/nimamasl114514/wcpilot.git && cd wcpilot
go build -o wcpilot.exe .

# 2) 看有什么窗口
./wcpilot.exe windows --keyword Notepad

# 3) 锁定目标：之后所有坐标都是「窗口内相对坐标」，越界直接拒绝
./wcpilot.exe target --title Notepad

# 4) 截图（默认叠加坐标网格 + 鼠标十字，方便人/模型读坐标）
./wcpilot.exe shot -o np.png

# 5) 点击 → 输入 → 回找
./wcpilot.exe click --x 300 --y 200
./wcpilot.exe type  --text "wcpilot v8 测试 OK"
./wcpilot.exe find_text --text "v8 测试" --click
```

OCR（可选，只有 `find_text` / `wait_text` 需要）：

```bash
pip install rapidocr-onnxruntime
./wcpilot.exe doctor          # 自检：python / rapidocr / 守护进程 / DPI / 缓存
```

---

## 命令速查

| 类别 | 命令 |
|---|---|
| 窗口 | `windows` `target` `activate` `wininfo` `win_tree` `winctl` |
| 画面 | `shot` `annotate` `get_pixel` |
| 鼠标 | `click` `dblclick` `move` `wheel` `drag` `keydown` `keyup` |
| 键盘 | `key` `type` `get_clipboard` `set_clipboard` |
| 定位 | `find_color` `find_image` `find_text` |
| 等待 | `wait_text` `wait_color` `wait_image` `wait_gone` `wait_stable` |
| 编排 | `batch`（20 种 op，一次调用跑完整条流程） |
| 环境 | `doctor` `ocr_status` `ocr_stop` `version` `help` |

`wcpilot help <子命令>` 看该命令详解。

### 坐标与防误触（最重要的一条）

- **锁定窗口后**（`--hwnd` / `--title` / 已 `target`）：`--x --y` 是**窗口可见区内相对坐标**，
  超出 `[0,w)×[0,h)` 直接 `exit 1` 拒绝执行 —— 这是防误触的核心，不会误点到别的窗口。
- **未锁定窗口**：`--x --y` 是整块虚拟屏的绝对坐标（多屏原点可为负）。
- 窗口口径 = **DWM 可见框**（`DWMWA_EXTENDED_FRAME_BOUNDS`），不含 Windows 那圈看不见的
  resize 边框，与截图图像素**严格一一对应**。

### 后台模式 `--bg`

不激活窗口、不移动真实鼠标、不抢物理键盘，全部走 `PostMessage`：

```bash
wcpilot click --bg --x 300 --y 200      # WM_MOUSEMOVE + 按下/抬起，自动下钻到最深子控件
wcpilot type  --bg --text "后台输入"     # WM_CHAR 逐字投递
wcpilot key   --bg --key ctrl+a         # 编辑控件命令消息（不依赖 GetKeyState）
```

代价是兼容性：**经典 Win32 控件**（Edit / RichEdit / Button / ListView）工作良好；
**WinUI / UWP / Electron / 自绘界面**基本不认（它们不走 `WM_*` 消息）。
先 `--bg` 再截图验证状态变化，不行就退回前台模式。

---

## 批量编排：一条 JSON 跑完整个流程

```json
[
  {"op": "click",  "x": 300, "y": 200, "comment": "聚焦输入框"},
  {"op": "key",    "key": "ctrl+a"},
  {"op": "type",   "text": "订单号 A-1029"},
  {"op": "wait_stable", "timeout_ms": 3000, "stable_ms": 300},
  {"op": "click_text",  "text": "查询", "timeout_ms": 8000},
  {"op": "wait_text",   "text": "查询结果", "timeout_ms": 20000}
]
```

```bash
wcpilot batch --jobs ops.json --evidence ./evidence --shots
```

- 默认**失败即短路**（`on_error: "continue"` 或 `--continue-on-error` 可跳过）
- 逐 job 回报 `{i, op, ok, ms, info}`；`--evidence` 失败自动留现场截图 + 上下文 JSON
- 结束时自动释放所有 `keydown` 按住的键（**防卡键**）
- 每个 job 可带 `hwnd` / `title`，一条流程里跨窗口操作

op 全集：`click` `move` `wheel` `key` `keydown` `keyup` `type` `drag` `sleep` `winctl`
`get_pixel` `click_text` `click_color` `click_image` `wait_text` `wait_color` `wait_image`
`wait_gone` `wait_stable`

---

## v8 做了什么（相对 v7）

### 性能

| 项 | 改造 | 实测 |
|---|---|---|
| 找色（4MP） | `map` 逐像素累加 → 扁平桶数组 + 行带并行 | 10.0ms → **2.8ms**（3.5×） |
| 找图（4MP） | 暴力滑窗 → 行带并行 + 首点早退 + 命中封顶 | 9.4ms → **2.0ms**（4.8×） |
| 截图像素转换 | 逐通道 4 次字节写 → uint32 整字原地交换，省掉中转缓冲 | 2.4 GB/s |
| PNG 编码 | 默认 `BestSpeed`（`WCPILOT_PNG` 可调） | 4MP 省 ~40-90% 编码时间 |
| PNG 解码 | 逐像素 `At()` → `draw.Draw` 快速路径 | 4MP 200ms → ~15ms |
| OCR 通道 | 每次 spawn python → **常驻守护进程**（TCP + 令牌 + 空闲自退） | 8.1s → **3.5s/次**（2.3×，省掉每次 4.6s 模型加载） |
| OCR 识别 | 全量识别 → **命中即停**（early-stop）+ 关掉方向分类 | 168 块全屏：13s → 1~3s |
| OCR 中转 | PNG 编+解 → **BMP 零压缩** | 少一次编码 + 一次解码 |
| OCR 结果 | 新增**内容寻址缓存**（画面没变直接复用） | 命中 20ms |
| 标注绘制 | 逐像素边界检查 → 裁剪后整行写 | 4MP 网格绘制 ~1ms |

### 修的真 bug

1. **`batch` 失败短路时不会释放按住的键** —— `os.Exit` **不执行** `defer`，注释写着"会自动释放"，
   实际不会 → 现在退出前显式 `releaseAll()`。踩到这个会留下"Ctrl 卡住"的系统级故障。
2. **`find_text` 结果里没有识别原文** —— `--all` 时你只能看到一堆坐标，无法核对 OCR 是否认错。
   现在每个 hit 带 `text`。
3. **`wheel` 无法在 (0,0) 滚动** —— 旧代码用 `x!=0||y!=0` 判断"是否指定坐标"，`--x 0 --y 0` 被当成没给。
4. **`type` 静默改写用户剪贴板** —— 现在默认**保存并恢复**（`--no-restore-clipboard` 关闭）。
5. **`isBlankCapture` 抽样走斜线** —— `i%w / i/w` 在大图上采出一串对角线点，"整图纯黑"的判定不可靠；
   改成按行列步进。
6. **`grabWindow` 回退路径沿用旧几何**（v7 遗留）。
7. **`--bg` 点击不投 `WM_MOUSEMOVE`** —— 很多控件要 hover 才建立按下状态，直接投 DOWN 会被丢弃。
8. **`--bg` 键盘 `lParam` 恒 0** —— 靠 `lParam` 取扫描码的程序（输入法、游戏、自绘控件）收不到键；
   现在补 `扫描码<<16 | 扩展位<<24 | 释放位`。
9. **`resolveTarget` 在库里直接 `os.Exit`** —— batch 里单个 job 解析失败会连带杀掉整个编排；
   拆出 `resolveTargetSafe`。

### 新能力

| 命令 | 用途 |
|---|---|
| `win_tree` | Win32 控件树：`class` / `text` / `rect` / 可见 / 可用 / `ctrl_id`。**不靠 OCR 就能定位控件**，比整屏找字快一个量级 |
| `wait_stable` | 等画面真的不动了（连续两帧差异 ≤ 阈值且持续 N ms），替代拍脑袋的 `sleep` |
| `doctor` | 环境自检：DPI 感知 / python / rapidocr / 守护进程 / 缓存 / 并行度 / 目标锁定 / 虚拟屏 |
| `ocr_status` `ocr_stop` | 查看/停止 OCR 守护进程 |
| `WCPILOT_TRACE=1` | 把 `capture / annotate / encode / find / ocr` 各阶段耗时打到 stderr（stdout 仍只有一行 JSON） |
| 退出码约定 | `0` 成功 · `1` 运行失败 · `2` 参数/用法错误 |

---

## 已知边界（别踩）

- **DPI**：进程启动即声明 `per_monitor_v2`。若目标程序是 DPI *未感知* 的，它在高缩放屏上
  `PrintWindow` 只画逻辑尺寸内容（右下留黑）——wcpilot 会检测并双线性放大，见 `docs/PITFALLS.md`。
- **被 GPU 独占的程序**（部分游戏、受保护视频）`PrintWindow` 出全黑，会回退"激活窗口后裁屏"，
  此时窗口不能被遮挡。
- **`--bg` 只对经典 Win32 控件可靠**。WinUI（Win11 记事本）/ UWP / Electron 需要前台模式。
- **OCR 慢是常态**：rapidocr 的识别时间与"文本块数量"成正比，不是与图片面积成正比。
  全屏 4MP 上百个文本块，慢机器上要几秒到十几秒。三个杠杆：`--hwnd`/`--region` 缩小范围、
  `--text` 给具体词（early-stop）、重复帧吃缓存。
- **`find_color` 的精度是网格级**：命中按 24px 桶聚合后返回桶中心，误差 ±12px。这是刻意的
  （否则一次返回上万个点，AI 没法用）。
- **`win_tree` 不是 UI Automation**：拿不到 WPF/UWP/Electron 内部元素树，只能看到 Win32 子窗口。

---

## 环境变量

| 变量 | 说明 |
|---|---|
| `WCPILOT_PYTHON` | 指定装了 rapidocr 的 python（优先级高于 PATH） |
| `WCPILOT_OCR` | `auto`(默认) / `daemon`(强制守护) / `spawn`(每次起进程) |
| `WCPILOT_OCR_IDLE` | 守护进程空闲退出秒数（默认 600，0 = 常驻） |
| `WCPILOT_OCR_CACHE` | `off` 关闭 OCR 结果缓存 |
| `WCPILOT_THREADS` | 并行查找线程数（默认 CPU 核数） |
| `WCPILOT_PNG` | `default` / `best` / `none`：PNG 压缩档位（默认 BestSpeed） |
| `WCPILOT_TRACE` | `1` 打开阶段耗时追踪（stderr） |

---

## 目录

```
main.go        CLI 分发 / 参数 / 帮助 / 坐标换算
shot.go        截图（BitBlt / PrintWindow / DWM 可见框 / DPI 归一化）
find.go        找色 / 找图（并行）
ocr.go         OCR 通道（守护进程 + 缓存 + 兜底）
ocr_helper.py  OCR 后端（一次性 / --serve 两种形态）
input.go       输入注入（SendInput / PostMessage / 剪贴板）
annotate.go    截图可视化标注（零依赖 5x7 点阵字模）
imgx.go        图像内核（像素转换 / PNG / BMP / 并行分带）
tree.go        控件树（win_tree）
stable.go      wait_stable
batch.go       编排引擎
winctl.go      窗口控制 / 读像素
target.go      目标锁定
dwm.go / dpi.go 几何与 DPI
doctor.go      环境自检
evidence.go    证据链（时间线 / 失败现场）
```

`docs/` 里有架构说明、踩坑合集、基准数据；`skills/pc-auto/SKILL.md` 是给 AI 代理用的技能说明
（工作流 + 纪律 + 常见错误），可以直接塞进 Claude Code / Codex / 灵犀 之类的工具里。

---

## Build & Release

```bash
go build -o wcpilot.exe .                       # 本机
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=8.0.0" -o wcpilot.exe .
go test ./...                                   # 单元测试
go test -run XXX -bench . -benchtime 5x         # 基准
```

CI 见 `.github/workflows/ci.yml`（Windows runner：build + gofmt + vet + test）。

---

## License

MIT
