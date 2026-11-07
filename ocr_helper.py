# -*- coding: utf-8 -*-
"""ocr_helper.py —— wcpilot 的 OCR 后端（rapidocr-onnxruntime，离线，中英混合）

两种运行形态：

1) 一次性（兼容旧调用）
       python ocr_helper.py <图片路径> [--query 词] [--all]
   输出单行 JSON 数组：[{text,score,x,y,w,h,cx,cy}, ...]

2) 常驻服务（v8 主力形态，由 wcpilot 自动拉起）
       python ocr_helper.py --serve --register <就绪文件路径> --token <令牌> --idle 600
   - 引擎只初始化一次（省掉每次 ~2.5s 的 import + 模型加载）
   - 监听 127.0.0.1 随机端口，把 {port, token, pid} 写进就绪文件
   - 协议：一行一个 JSON 请求，一行一个 JSON 响应
       → {"id":1,"image":"x.bmp","query":"确定","all":false,"angle":false,"token":"..."}
       ← {"id":1,"ok":true,"boxes":[...],"ms":37,"total":168,"scanned":12,"partial":true}
   - 空闲 --idle 秒无请求自动退出（防孤儿进程）
   - 任意时刻可用 {"op":"quit"} 让它干净退出

v8 关键优化：
   · 默认关闭方向分类（--angle 才开）：截屏文字都是水平的，cls 白花 1.5s/张（本机实测）
   · 渐进识别 + 命中即停：只找某个词、不要全部命中时，识别到该词就返回，
     典型 168 个文本块的全屏图从 13s 降到 1~3s
"""

import json
import os
import socket
import sys
import time

TEXT_SCORE = 0.5
BATCH = 8


def load_engine():
    try:
        from rapidocr_onnxruntime import RapidOCR
    except ImportError:
        return None, ("缺少依赖，请先执行: pip install rapidocr-onnxruntime "
                      "（或用 WCPILOT_PYTHON 指向装好依赖的解释器）")
    try:
        return RapidOCR(use_angle_cls=False), None
    except TypeError:
        return RapidOCR(), None


def _mk(box, text, score):
    xs = [float(p[0]) for p in box]
    ys = [float(p[1]) for p in box]
    x, y = int(min(xs)), int(min(ys))
    w, h = int(max(xs)) - x, int(max(ys)) - y
    return {
        "text": text, "score": round(float(score), 4),
        "x": x, "y": y, "w": w, "h": h,
        "cx": x + w // 2, "cy": y + h // 2,
    }


def recognize(engine, image, query="", want_all=True, angle=False, early=True):
    """返回 (boxes, meta)。query 非空且 early 时，命中首个就停。"""
    t0 = time.time()
    if angle:
        # 带方向分类的完整链路（慢，但能处理竖排/旋转文字）
        result, _ = engine(image)
        boxes = [_mk(p, t, s) for p, t, s in (result or [])]
        return boxes, {"ms": int((time.time() - t0) * 1000), "total": len(boxes),
                       "scanned": len(boxes), "partial": False, "stage": "full"}

    img = engine.load_img(image)
    dt_boxes, _ = engine.text_detector(img)
    if dt_boxes is None or len(dt_boxes) < 1:
        return [], {"ms": int((time.time() - t0) * 1000), "total": 0,
                    "scanned": 0, "partial": False, "stage": "det"}
    dt_boxes = engine.sorted_boxes(dt_boxes)
    crops = engine.get_crop_img_list(img, dt_boxes)

    out = []
    scanned = 0
    for i in range(0, len(crops), BATCH):
        chunk = crops[i:i + BATCH]
        try:
            rec, _ = engine.text_recognizer(chunk)
        except Exception as exc:  # 单批识别失败不该拖垮整轮
            sys.stderr.write("rec batch failed: %r\n" % (exc,))
            continue
        for j, item in enumerate(rec):
            scanned += 1
            text, score = item[0], float(item[1])
            if score < TEXT_SCORE or not text:
                continue
            out.append(_mk(dt_boxes[i + j], text, score))
            if early and not want_all and query and query in text:
                return out, {"ms": int((time.time() - t0) * 1000), "total": len(crops),
                             "scanned": scanned, "partial": True, "stage": "early"}
    return out, {"ms": int((time.time() - t0) * 1000), "total": len(crops),
                 "scanned": scanned, "partial": False, "stage": "rec"}


# ---------------- 一次性模式 ----------------

def one_shot(argv):
    if not argv:
        print(json.dumps({"error": "missing image path"}, ensure_ascii=False))
        return 1
    image = argv[0]
    query, want_all, angle = "", False, False
    i = 1
    while i < len(argv):
        a = argv[i]
        if a == "--query" and i + 1 < len(argv):
            query = argv[i + 1]
            i += 2
            continue
        if a == "--all":
            want_all = True
        elif a == "--angle":
            angle = True
        i += 1
    engine, err = load_engine()
    if err:
        print(json.dumps({"error": err}, ensure_ascii=False))
        return 1
    boxes, _meta = recognize(engine, image, query, want_all, angle, early=bool(query))
    print(json.dumps(boxes, ensure_ascii=False))
    return 0


# ---------------- 常驻服务模式 ----------------

def serve(register, token, idle, port):
    engine, err = load_engine()
    if err:
        sys.stderr.write(err + "\n")
        return 1

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", port))
    srv.listen(8)
    srv.settimeout(1.0)
    real_port = srv.getsockname()[1]

    # 原子写就绪文件：先写临时文件再 replace，避免 Go 侧读到半截 JSON
    tmp = register + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump({"port": real_port, "token": token, "pid": os.getpid(), "version": 2}, f)
    os.replace(tmp, register)

    last = time.time()
    while True:
        try:
            conn, _ = srv.accept()
        except socket.timeout:
            if idle > 0 and time.time() - last > idle:
                break
            continue
        except OSError:
            break
        conn.settimeout(300)
        buf = b""
        try:
            while True:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                buf += chunk
                while b"\n" in buf:
                    line, buf = buf.split(b"\n", 1)
                    if not line.strip():
                        continue
                    try:
                        req = json.loads(line.decode("utf-8"))
                    except Exception as exc:
                        conn.sendall((json.dumps({"ok": False, "error": "bad json: %r" % (exc,)}) + "\n").encode())
                        continue
                    last = time.time()
                    if req.get("token") != token:
                        conn.sendall(b'{"ok":false,"error":"bad token"}\n')
                        continue
                    rid = req.get("id", 0)
                    op = req.get("op", "ocr")
                    if op == "ping":
                        rsp = {"id": rid, "ok": True, "pong": True, "pid": os.getpid()}
                    elif op == "quit":
                        conn.sendall((json.dumps({"id": rid, "ok": True, "bye": True}) + "\n").encode())
                        try:
                            os.remove(register)
                        except OSError:
                            pass
                        return 0
                    else:
                        try:
                            boxes, meta = recognize(
                                engine, req.get("image", ""),
                                req.get("query", ""), bool(req.get("all", False)),
                                bool(req.get("angle", False)),
                                early=bool(req.get("early", True)),
                            )
                            rsp = {"id": rid, "ok": True, "boxes": boxes}
                            rsp.update(meta)
                        except Exception as exc:
                            rsp = {"id": rid, "ok": False, "error": "%s: %s" % (type(exc).__name__, exc)}
                    conn.sendall((json.dumps(rsp, ensure_ascii=False) + "\n").encode("utf-8"))
        except (socket.timeout, ConnectionError, OSError):
            pass
        finally:
            try:
                conn.close()
            except Exception:
                pass
        if idle > 0 and time.time() - last > idle:
            break

    try:
        os.remove(register)
    except OSError:
        pass
    return 0


def main():
    argv = sys.argv[1:]
    if argv and argv[0] == "--serve":
        register, token, idle, port = "", "", 600, 0
        i = 1
        while i < len(argv):
            a = argv[i]
            if a == "--register" and i + 1 < len(argv):
                register = argv[i + 1]
                i += 2
                continue
            if a == "--token" and i + 1 < len(argv):
                token = argv[i + 1]
                i += 2
                continue
            if a == "--idle" and i + 1 < len(argv):
                idle = int(argv[i + 1])
                i += 2
                continue
            if a == "--port" and i + 1 < len(argv):
                port = int(argv[i + 1])
                i += 2
                continue
            i += 1
        if not register:
            sys.stderr.write("--serve 需要 --register\n")
            return 1
        return serve(register, token, idle, port)
    return one_shot(argv)


if __name__ == "__main__":
    sys.exit(main())
