# 踩坑合集

每一条都是实测撞出来的，不是抄文档。踩之前读一遍能省几小时。

## 截图与坐标

### 1. 高分屏全屏截图只截到左上角

**现象**：2560×1600 @150% 的屏，`shot` 出 1707×1067 的图，内容和右下角全丢。

**根因**：Go 编译的 exe 默认没有 manifest，进程被判为 **DPI 未感知**。
`GetSystemMetrics(SM_CXSCREEN)` 返回的是虚拟化后的**逻辑**尺寸（1707），
而 `GetDC(0)+BitBlt` 取到的仍是**物理**像素（2560）。按逻辑尺寸从 (0,0) 拷贝物理像素，
自然只有左上 44%。

**修法**：`main()` 第一行 `SetProcessDpiAwarenessContext(-4)`（PER_MONITOR_AWARE_V2），
三级降级到 `SetProcessDpiAwareness(2)` / `SetProcessDPIAware()`。

### 2. 窗口截图多一圈黑边、坐标整体偏左上

**根因**：`GetWindowRect` 含 DWM 的不可见 resize 边框。实测记事本左/右/下各多 7px，
无边框 tkinter 窗多 11px。

**修法**：一律用 `DwmGetWindowAttribute(hwnd, DWMWA_EXTENDED_FRAME_BOUNDS)`（可见框）。
`wininfo` 子命令能把三套口径和 `inset` 一次打出来对账。

### 3. DPI 未感知的窗口，PrintWindow 右下大片纯黑

**现象**：600×450 画布，内容只画在左上 400×300。

**根因**：unaware 窗口按逻辑尺寸绘制，物理画布右侧/底部没有内容。

**坑点**：`GetDpiForWindow` 对 unaware 窗口**恒返回 96**（它自认 100%），
判断"是否被系统放大"必须用 `GetDpiForMonitor(MDT_EFFECTIVE_DPI)`（实测 150% 屏 = 144）。

**修法**：`normalizeUnawareCapture` —— 内容是纯黑包围时，把内容区双线性放大铺满画布。
触发条件是保守的（感知级别 unaware + 缩放 >1.05 + 右/下都缺 >16px 且 >6%）。

### 4. 「暗色界面」被误判成"截图为空"

**根因**：旧判据"暗像素占比 >97% 就算失败"，现代暗色主题 UI 会被误杀，然后错误回退到
前台裁屏（引入遮挡污染）。

**修法**：改成"整图**严格**纯黑（每通道 == 0）"才算失败。

## 输入注入

### 5. `PostMessage` 点不动控件

**根因**：顶层窗口往往不是真正接收鼠标的那个（记事本编辑区是子窗口 `RichEditD2DPT`）。

**修法**：`ChildWindowFromPointEx` 逐层下钻（上限 16 层），把坐标换算进子控件坐标系再投递。
输出里的 `child_depth` / `target_class` 就是给这件事留的痕。

### 6. `--bg` 下 `ctrl+c` / `ctrl+v` 全部失效

**根因**：`PostMessage` 投递的 `WM_KEYDOWN` **不更新键盘状态**，编辑控件用 `GetKeyState`
判断修饰键时会认为 Ctrl 没按下。

**修法**：改投语义等价的编辑控件命令消息 —— `WM_COPY` / `WM_CUT` / `WM_PASTE` / `EM_SETSEL` / `WM_CLEAR`。
输出里 `via: "edit_command"` 表示走了这条可靠路径，`via: "key_events"` 表示逐键投递。

### 7. `--bg` 键盘在某些程序里完全没反应

**根因之一**：`lParam` 恒为 0。不少程序（输入法、游戏、自绘控件）从 `lParam` 取扫描码。

**修法**：`lParam = 1 | (扫描码<<16) | (扩展位<<24) | (释放 ? 1<<30|1<<31 : 0)`，
扫描码用 `MapVirtualKeyW(vk, MAPVK_VK_TO_VSC)`。

**根因之二**：WinUI（Win11 记事本）/ UWP / Electron 根本不走 `WM_*` 输入消息。
这类程序只用前台 `SendInput`，别再调 `--bg` 浪费时间。

### 8. `--bg` 点击"看起来点了但没生效"

**根因**：缺 `WM_MOUSEMOVE`。很多控件要收到 hover 才建立按下状态，直接投 `DOWN` 会被丢弃。

**修法**：先投 `WM_MOUSEMOVE`，再 `DOWN`，再 `UP`。

### 9. `keydown` 之后 Ctrl 卡住不释放

**根因**：`os.Exit` **不会执行 `defer`**。注释写着"退出前 defer 全量释放"，实际短路退出时跳过了。

**修法**：退出前显式调用 `releaseAll()`。（这是个会污染整个系统状态的 bug，优先级很高。）

## OCR

### 10. OCR 每次调用都慢 2~5 秒

**根因**：`import rapidocr_onnxruntime` + 加载模型 ≈ 2.5s，而 CLI 每次调用都是新进程。

**修法**：常驻守护进程（detached、TCP + 令牌、空闲自退）。`ocr_status` / `ocr_stop` 管理，
`WCPILOT_OCR=spawn` 强制退回旧行为。

### 11. OCR 成本与图片面积不成正比

**实测**（rapidocr 1.2.3，本机）：检测 4.6s + 方向分类 1.5s + 识别 = **文本块数 × ~80ms**。
一张全屏 168 个文本块 → 13s；同尺寸但只有 6 个文本块 → 不到 1s。

**结论**：优化 OCR 要**减少文本块数量**，不是缩小图片。三个杠杆：
`--hwnd`/`--region` 框小范围、`--text` 给具体词（early-stop）、画面不变时吃缓存。

### 12. Python 在 Git Bash 里"找不到"

**根因**：从 Git Bash 启动时继承的 `PATH` 是 `/c/Users/...` 这种 POSIX 形式，
Windows 的 `exec.LookPath` 解析不了。

**修法**：`findPython()` 按 PATH 逐项做 POSIX→Windows 转换后再探测，并跳过 `WindowsApps`
（那是应用商店占位符，不是真 python）。也可以用 `WCPILOT_PYTHON` 直接指定。

## Go / Win32 语言层面

### 13. `struct` 字段组共享 tag

```go
X, Y int `json:"x"`   // ❌ Y 的键永远是 "x"
```
拆成独立字段行、各自挂 tag。`go vet` 会报 `struct field repeats json tag`，别忽略。

### 14. Win32 常量负数传参

`GWL_EXSTYLE = -20`、`HWND_TOPMOST = -1` 这类值不能直接写 `uintptr(-20)`（编译期常量溢出）。
要么走变量做符号扩展，要么用 `^uintptr(0)` 形式。

### 15. `go vet` 的 `unsafe.Pointer` 误报

`GlobalLock` 返回 `uintptr`，转 `unsafe.Pointer` 写内存是标准剪贴板写法，但 `go vet` 会报
`possible misuse of unsafe.Pointer`。改用 `unsafe.Slice` 后仍然报——这是 syscall 固有误报，
CI 里用 `go vet -unsafeptr=false`。

### 16. 分支里 `jout()` 之后忘了 `return`

`jout()` 只打印不退出。`--bg` 分支忘了 `return` 会继续跑完前台通道，输**两行 JSON**，
而且"验证通过"的其实是前台通道干的活。**输出多了一行 = 通道重复执行**，是强信号。
