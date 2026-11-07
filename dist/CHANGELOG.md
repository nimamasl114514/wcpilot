# 更新日志

本文件记录每次发布的**用户可见变化**。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/)，
版本号遵循语义化版本。

## [8.0.0] - 2026-11-07

v8 是一次「性能 + 正确性」大修，同时补了三个真实用户会天天用的能力。

### 新增

- `win_tree`：Win32 控件树（class / text / rect / visible / enabled / ctrl_id），
  不靠 OCR 就能定位控件。`--depth` 限深、`--text`/`--class` 过滤。
- `wait_stable`：等画面真的不动了（连续两帧差异 ≤ `--ratio` 且持续 `--stable_ms`），
  替代 `sleep` 猜时间；batch 里对应 `{"op":"wait_stable"}`。
- `doctor`：环境自检（DPI 感知 / python / rapidocr / OCR 守护进程 / 缓存 / 并行度 /
  目标锁定 / 虚拟屏范围）。
- `ocr_status` / `ocr_stop`：查看、停止 OCR 守护进程。
- `version` / `help <子命令>`。
- `WCPILOT_TRACE=1`：把 capture / annotate / encode / find / ocr 各阶段耗时打到 stderr。
- batch：`hwnd` / `title` 字段（每个 job 可操作不同窗口）、`wheel` 支持 `bg`、`all`/`angle` 字段。
- 退出码约定：`0` 成功、`1` 运行失败、`2` 参数/用法错误。

### 性能

- 找色：桶聚合从 `map` 改扁平数组 + 行带并行，4MP **10.0ms → 2.8ms**。
- 找图：行带并行 + 首点早退，4MP **9.4ms → 2.0ms**。
- 截图像素转换：`GetDIBits` 直接写进 `image.RGBA`，省一块 16MB 中转缓冲；uint32 整字换色。
- PNG 解码：逐像素 `At()` 改 `draw.Draw` 快速路径（4MP 200ms → ~15ms）。
- PNG 编码：默认 `BestSpeed`，`WCPILOT_PNG=default|best|none` 可调。
- OCR：新增**常驻守护进程**（TCP + 令牌 + 空闲自退 + spawn 兜底），
  每次调用省掉一次 python import + 模型加载（本机实测 8.1s → 3.5s/次）。
- OCR：**命中即停**（early-stop）——只要找一个词就不必识别整屏；默认关闭方向分类。
- OCR：中转文件从 PNG 改 BMP（零压缩），省一次编码 + 一次解码。
- OCR：内容寻址缓存，同一帧重复查询 20ms 返回。
- 标注：网格绘制改整行写，4MP 网格 ~1ms。

### 修复

- **`batch` 失败短路时不释放按住的键**：`os.Exit` 不执行 `defer`，注释承诺的"自动释放"实际没发生，
  会留下 Ctrl/Shift 卡住。现在显式 `releaseAll()`。
- `find_text` 结果缺 `text` 字段，`--all` 时无法核对识别内容。
- `wheel` 无法在 `(0,0)` 滚动（`x!=0||y!=0` 把合法坐标当"未指定"）。
- `type` 静默改写用户剪贴板；现在默认保存并恢复（`--no-restore-clipboard` 关闭）。
- `isBlankCapture` 抽样沿对角线漂移，全黑判定不可靠。
- `--bg` 点击不投 `WM_MOUSEMOVE`，部分控件丢弃按下消息。
- `--bg` 键盘 `lParam` 恒 0，缺扫描码/扩展位/释放位。
- `resolveTarget` 在库函数里直接 `os.Exit`，batch 里单个 job 解析失败会杀掉整个编排。
- 窗口截图回退路径沿用激活前的旧几何（v7 遗留）。

### 其他

- 源码分层整理：`imgx.go`（图像内核）、`tree.go`、`stable.go`、`doctor.go`、`version.go` 新增；
  `ocr.go` 重写为「守护进程 + 缓存 + 兜底」三段式。
- 新增单元测试与基准（`wcpilot_test.go`）：解析器、裁剪、找色/找图正确性、
  串并行一致性、PNG/BMP 往返、缓存键、标注、batch 解析、vk 映射。
- 新增 CI（Windows runner：build + gofmt + vet + test）。

## [7.0.0] - 2026-10-11

- 截图口径三修：改用 DWM 可见框（去不可见边框）、DPI 未感知窗口内容归一化、空白判定改严格纯黑。
- 新增 `wininfo`（整窗/可见框/客户区/DPI 三口径对照）、`annotate`、`shot --grid/--no-annotate`、
  `find_* -o` 命中框可视化（零依赖 5x7 点阵字模）。
- 新增证据链：`batch --evidence DIR [--shots]`（时间线 + 失败现场）。

## [4.0.0] - 2026-09-12

- 新增 `--bg` 键盘通道：`WM_CHAR` 逐字、`WM_KEYDOWN/UP` 组合键、编辑控件命令消息映射
  （`WM_COPY/WM_CUT/WM_PASTE/EM_SETSEL`），解决 `GetKeyState` 导致的 bg 快捷键失效。
- `--bg` 点击支持子控件下钻（`ChildWindowFromPointEx`）与焦点控件定位（`GetGUIThreadInfo`）。

## [3.0.0] - 2026-09-12

- 新增 `winctl`（移动/缩放/最大化/最小化/置顶）与 `get_pixel`。
- 新增 `batch` 编排、`drag`、`keydown/keyup`、`wait_*` 引擎、剪贴板子命令。

## [2.0.0] - 2026-09-12

- `find_text`（OCR）/ `find_image`（模板匹配）落地；记事本端到端闭环。

## [1.0.0] - 2026-09-12

- 首个可用版本：窗口枚举 / 目标锁定 / 三种截图 / 点击 / 键盘 / 中文输入 / 找色。
