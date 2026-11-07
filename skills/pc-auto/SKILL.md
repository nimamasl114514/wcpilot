---
name: pc-auto
description: Operate a Windows desktop through wcpilot (screenshot / click / type / find text-color-image / wait / batch). Window-scoped coordinates prevent misclicks.
description_zh: 通过 wcpilot 操作 Windows 桌面（截图/点击/输入/找字找色找图/等待/批量编排），窗口内相对坐标防误触。
description_en: Drive the Windows desktop with wcpilot CLI — screenshot, click, type, find by text/color/image, wait, and batch编排. All coordinates are window-scoped.
allowed-tools: Bash, Read
---

# 用 wcpilot 操作 Windows 桌面

wcpilot 每条命令做一件事，输出**一行 JSON**。你（AI）的循环是：
**看（shot / win_tree）→ 定位（find_*）→ 做（click / type / key）→ 验（wait_* / shot）**。

## 铁律

1. **先锁定窗口，再动手。**
   任何点击/输入前，先 `target --title <关键字>`（或每条命令带 `--hwnd`）。
   锁定后 `--x/--y` 就是**窗口内相对坐标**，越界会被拒绝执行——这是防误触的闸门，不要绕过它
   去用绝对坐标。
2. **不要凭想象给坐标。** 先 `shot -o x.png` 看图（默认带网格和鼠标十字），
   或者 `win_tree` / `find_text` / `find_image` 拿到真实坐标。
3. **不要用固定 `sleep` 等界面。** 用 `wait_text` / `wait_color` / `wait_image` / `wait_gone`，
   或者 `wait_stable`（等画面不动了）——比 sleep 可靠得多。
4. **一次只做一个动作，做完就看结果。** 把长流程写成 `batch`，而不是让每一步都当盲操作。
5. **OCR 只在必要时用。** 它是最慢的一环，且要求装了 python + rapidocr。
   能用 `win_tree`（控件类名/文本/矩形）或 `find_image`（模板）解决就别 OCR。

## 标准工作流

```bash
# 1) 找到窗口
wcpilot windows --keyword 记事本

# 2) 锁定（之后坐标都是窗口内相对坐标）
wcpilot target --title 记事本

# 3) 看清结构
wcpilot shot -o before.png          # 带网格 + 鼠标十字
wcpilot win_tree --depth 3          # Win32 控件树：class/text/rect

# 4) 定位目标
wcpilot find_text --text "确定"      # 返回 screen_x/screen_y，可直接用
wcpilot find_image --tpl btn.png
wcpilot find_color --color "#1677FF" --tol 12

# 5) 动作
wcpilot click --x 300 --y 200        # 窗口内坐标
wcpilot click_text --text "确定"      # = find_text + click（batch op）
wcpilot type --text "订单 A-1029"
wcpilot key --key ctrl+s

# 6) 验证
wcpilot wait_text --text "保存成功" --timeout 10000
wcpilot wait_stable --timeout 3000
wcpilot shot -o after.png
```

## 后台模式（不打扰用户）

`--bg` 走窗口消息注入：不激活窗口、不动真实鼠标、不抢键盘焦点。

```bash
wcpilot click --bg --x 300 --y 200
wcpilot type  --bg --text "批量填写"
wcpilot key   --bg --key ctrl+a
```

**只对经典 Win32 控件可靠**（Edit / RichEdit / Button / ListView / 菜单）。
WinUI（Win11 记事本、设置）/ UWP / Electron / 游戏基本不认 `WM_*` 消息 —— 这类程序请去掉
`--bg` 用前台模式。**任何 `--bg` 动作之后都要截图确认真的生效了**，不要假设成功。

## batch 编排

```json
[
  {"op":"click","x":300,"y":200,"comment":"聚焦"},
  {"op":"key","key":"ctrl+a"},
  {"op":"type","text":"hello"},
  {"op":"wait_stable","timeout_ms":3000,"stable_ms":300},
  {"op":"click_text","text":"提交","timeout_ms":8000},
  {"op":"wait_text","text":"成功","timeout_ms":20000}
]
```

- 失败默认**短路**（后面的不再执行）；`on_error:"continue"` 单个跳过，`--continue-on-error` 全局宽松。
- `--evidence DIR --shots` 留证据：时间线 + 失败现场截图 + 上下文 JSON。
- 结束时自动释放所有 `keydown` 按住的键。
- 跨窗口：每个 job 可带 `hwnd` / `title`。

## 常见错误对照表

| 症状 | 原因 | 处理 |
|---|---|---|
| `坐标 (x,y) 超出窗口，拒绝执行` | 用了绝对坐标，或窗口几何变了 | 重新 `target`，`shot` 看新尺寸 |
| 点击没反应 | 点在了不可交互区域 / 控件被遮挡 | `shot` 看十字与网格，`win_tree` 确认控件 |
| `--bg` 点击无效 | 目标是 WinUI/UWP/Electron | 去掉 `--bg` 用前台 |
| `--bg` 快捷键无效 | `GetKeyState` 类加速键 | 改用 `--bg` 支持的编辑命令（`ctrl+a/c/v/x`）或前台 |
| OCR 报缺少依赖 | python 没装 rapidocr | `pip install rapidocr-onnxruntime`，或 `WCPILOT_PYTHON` 指定解释器 |
| OCR 很慢 | 全屏文本块太多 | 加 `--hwnd`/`--region` 缩小范围；给具体 `--text` 词 |
| `find_color` 位置偏一点 | 命中是 24px 网格中心 | 精度 ±12px，要精确点用 `find_image`/`win_tree` |
| 截图全黑 | GPU 渲染窗口 PrintWindow 失败 | 已自动回退"激活后裁屏"，确保窗口没被遮挡 |
| 只有 exe 没有 ocr_helper.py | 部署漏文件 | OCR 需要同目录的 `ocr_helper.py` |

## 环境自检

```bash
wcpilot doctor      # DPI / python / rapidocr / OCR 守护进程 / 缓存 / 并行度 / 目标锁定
wcpilot version
```

`WCPILOT_TRACE=1` 打开阶段耗时（stderr），stdout 仍然只有一行结果 JSON。
