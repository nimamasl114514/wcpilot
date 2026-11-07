# 架构说明

## 一句话

一个无窗口的 Go 进程，每次调用做一件事，用 `syscall` 直接和 Win32 对话，结果打成一行 JSON。

```
                        ┌──────────────────────────────┐
   AI / 脚本  ──argv──▶ │  main.go  参数 → 子命令分发   │
                        └──────────────┬───────────────┘
                                       │
        ┌──────────────────────────────┼──────────────────────────────┐
        │                              │                              │
   ┌────▼─────┐                  ┌─────▼──────┐                 ┌─────▼─────┐
   │  shot.go │                  │  input.go  │                 │  find.go  │
   │ 画面采集 │                  │  输入注入  │                 │ 找色/找图 │
   └────┬─────┘                  └─────┬──────┘                 └─────┬─────┘
        │                              │                              │
   PrintWindow / BitBlt          SendInput / PostMessage        并行行带扫描
   DWM 可见框 / DPI 归一化        WM_MOUSE* / WM_KEY* / WM_CHAR        │
        │                              │                              │
        └──────────────┬───────────────┴──────────────────────────────┘
                       │
               ┌───────▼────────┐        ┌──────────────────────────┐
               │  ocr.go        │──TCP──▶│ ocr_helper.py --serve    │
               │ 守护进程客户端 │        │ rapidocr（引擎只加载一次）│
               │ 内容寻址缓存   │◀─JSON──│ 渐进识别 / 命中即停      │
               └───────┬────────┘        └──────────────────────────┘
                       │
               ┌───────▼────────┐
               │  batch.go      │  串行编排 20 种 op，失败短路，防卡键
               └────────────────┘
```

## 坐标模型（最容易错的地方）

系统里有三套「窗口矩形」，选错就会偏 7~11 像素：

| 口径 | API | 含义 |
|---|---|---|
| 整窗 | `GetWindowRect` | 含 DWM 那圈**看不见**的 resize 边框 |
| 可见框 | `DwmGetWindowAttribute(DWMWA_EXTENDED_FRAME_BOUNDS)` | 用户**真的看得见**的范围（物理像素） |
| 客户区 | `GetClientRect` | 窗口自身坐标系，不含标题栏/边框 |

wcpilot 统一用**可见框**：`resolveTarget()` 返回它，截图按它裁剪，`--x/--y` 相对它的左上角。
这样「截图里的像素」和「点击坐标」是同一个坐标系，误差为 0。

DPI 方面：进程启动第一件事是 `SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2)`，
否则 150% 缩放的屏上 `GetSystemMetrics` 返回虚拟化后的逻辑尺寸，而 `BitBlt` 抓的是物理像素
——全屏截图只会拿到左上角 ≈44% 的区域。

## 两条输入通道

| | 前台（默认） | 后台 `--bg` |
|---|---|---|
| 机制 | `SendInput` 注入真实输入事件 | `PostMessage` 投窗口消息 |
| 需要焦点 | 是（会 `SetForegroundWindow`） | 否 |
| 动真实鼠标 | 是 | 否 |
| 兼容性 | 几乎 100% | 仅经典 Win32 控件 |
| 适用 | 兜底、WinUI/Electron/游戏 | 不打扰用户、批量、CI |

`--bg` 点击会先 `ChildWindowFromPointEx` 逐层下钻到最深的子控件（顶层窗口往往不是真正接收
鼠标的那个），把坐标换算到子控件坐标系后投 `WM_MOUSEMOVE → WM_LBUTTONDOWN → WM_LBUTTONUP`。

`--bg` 组合键优先走**编辑控件命令消息**（`WM_COPY` / `WM_CUT` / `WM_PASTE` / `EM_SETSEL` / `WM_CLEAR`），
因为投递的 `WM_KEYDOWN` 不会更新键盘状态，控件用 `GetKeyState` 判断修饰键时会认为 Ctrl 没按下。

## OCR 通道

```
第 1 次调用                     后续调用
   │                              │
   ├─ 查内容哈希缓存 ───命中──▶ 20ms 返回
   │      │未命中                  │
   │      ▼                       ▼
   ├─ 守护进程活着？ ──是──▶ 发请求（~推理时间）
   │      │否
   │      ▼
   ├─ 拉起 detached python（--serve，一次 ~5s）
   │
   └─ 失败 → 静默降级 spawn（每次 ~+5s）
```

请求协议（一行 JSON 一来一回）：

```json
{"id":1,"op":"ocr","image":"...bmp","query":"确定","all":false,"early":true,"token":"..."}
{"id":1,"ok":true,"boxes":[...],"ms":633,"total":22,"scanned":3,"partial":true,"stage":"early"}
```

三个关键决策：

1. **守护进程**：wcpilot 是"一次调用一个进程"的 CLI，每次重新 import rapidocr + 加载模型要
   ~2.5s，而 AI 一个任务要调几十次 OCR。守护进程把这份成本摊成一次。
2. **命中即停**：rapidocr 的成本 ≈ 文本块数量 × 每块识别时间（不是图片面积）。
   找一个词时没必要把整屏几十上百块都识别完 —— 识别到就返回。
3. **BMP 中转**：调用方已经在内存里有 `*image.RGBA`，写 PNG 要压缩、python 读还要解压；
   写 BMP 是纯内存拷贝，两边都省。

## 编排引擎

`batch` 是串行状态机，每个 job 独立成败，默认失败短路。三处细节：

- **防卡键**：`keydown` 按住的键记在 `pressed` 里，正常结束走 `defer`、异常退出显式调用，
  两条路径都释放（`os.Exit` 不跑 `defer`，这是 v7 的隐藏 bug）。
- **前台对齐**：`key`/`wheel`/`type` 发之前把目标窗口重新激活，防止前一个 OCR 探测把焦点带跑。
- **证据链**：`--shots` 每个 job 前后各截一图 + `timeline.json`；失败额外落 `failNNN.png/json`
  （job 定义、错误、窗口三口径、DPI）。落盘失败一律静默，绝不打断主流程。

## 并发模型

`findColor` / `findImage` 按**行带**切分：每带独立累加，最后合并。行带之间没有写冲突，
所以不需要锁（`findImage` 只对"命中数封顶"用一次原子 + 互斥）。
`WCPILOT_THREADS=1` 时退化为串行，行为与并行完全一致（有单测保证）。
