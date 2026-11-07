# -*- coding: utf-8 -*-
"""ocrbench.py —— OCR 通道基准：一次性 spawn vs 常驻守护进程

测的是 wcpilot 的 OCR 后端（ocr_helper.py），不依赖 wcpilot.exe 也能跑；
没图时会自动用 wcpilot shot 截一张，或者你自己传一张。

用法：
    python ocrbench.py                      # 自动取图，各跑 3 轮
    python ocrbench.py --rounds 5
    python ocrbench.py --image x.png
    python ocrbench.py --python C:/py/python.exe

指标解释：
    spawn   = 每次都要 import rapidocr + 加载模型 + 推理
    daemon  = 模型只加载一次，之后只剩推理
    → 两者之差就是「守护进程省下的冷启动」，也是 v8 的核心收益
"""

import argparse
import json
import os
import socket
import subprocess
import sys
import tempfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
HELPER = os.path.join(HERE, "ocr_helper.py")


def get_image(path):
    if path:
        return path
    tmp = os.path.join(tempfile.gettempdir(), "wcpilot_ocrbench.png")
    if not os.path.exists(tmp):
        exe = os.path.join(HERE, "wcpilot.exe")
        if not os.path.exists(exe):
            sys.exit("没找到测试图片，也没找到 wcpilot.exe；请用 --image 指定一张截图")
        print("截一张当前屏幕作为测试图 ...")
        subprocess.run([exe, "shot", "--no-annotate", "-o", tmp], check=True, capture_output=True)
    return tmp


def bench_spawn(py, image, rounds):
    times = []
    for i in range(rounds):
        t = time.time()
        r = subprocess.run([py, HELPER, image], capture_output=True, text=True, encoding="utf-8")
        el = (time.time() - t) * 1000
        times.append(el)
        n = -1
        try:
            n = len(json.loads(r.stdout.strip()))
        except Exception:
            pass
        print("  第%d轮 rc=%s %.0fms boxes=%d" % (i + 1, r.returncode, el, n))
    return times


def send_line(sock_file, sock, obj):
    sock.sendall((json.dumps(obj, ensure_ascii=False) + "\n").encode("utf-8"))
    line = sock_file.readline()
    if not line:
        raise RuntimeError("守护进程没有响应（可能已退出）")
    return json.loads(line)


def bench_daemon(py, image, rounds):
    register = os.path.join(tempfile.gettempdir(), "ocrbench.ready.%d.json" % os.getpid())
    token = "bench-token"
    if os.path.exists(register):
        os.remove(register)

    proc = subprocess.Popen(
        [py, HELPER, "--serve", "--register", register, "--token", token,
         "--idle", "60", "--port", "0"],
        stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
    )

    info = None
    deadline = time.time() + 60
    while time.time() < deadline:
        if os.path.exists(register):
            try:
                info = json.load(open(register, encoding="utf-8"))
                break
            except Exception:
                pass
        if proc.poll() is not None:
            err = proc.stderr.read().decode("utf-8", "ignore")
            sys.exit("守护进程启动失败：\n" + err)
        time.sleep(0.1)
    if not info:
        proc.kill()
        sys.exit("守护进程 60s 内未就绪")

    boot = time.time()
    sock = socket.create_connection(("127.0.0.1", info["port"]), timeout=600)
    f = sock.makefile("r", encoding="utf-8")
    send_line(f, sock, {"id": 0, "op": "ping", "token": token})
    boot_ms = (time.time() - boot) * 1000
    print("  握手 %.0fms（模型已在后台加载完毕）" % boot_ms)

    times = []
    for i in range(rounds):
        t = time.time()
        rsp = send_line(f, sock, {"id": i + 1, "op": "ocr", "image": image,
                                  "query": "", "all": True, "early": False, "token": token})
        el = (time.time() - t) * 1000
        times.append(el)
        print("  第%d轮 ok=%s %.0fms 引擎侧=%sms boxes=%d"
              % (i + 1, rsp.get("ok"), el, rsp.get("ms"), len(rsp.get("boxes") or [])))

    try:
        send_line(f, sock, {"id": 999, "op": "quit", "token": token})
    except Exception:
        pass
    try:
        sock.close()
    except Exception:
        pass
    try:
        proc.wait(timeout=10)
    except Exception:
        proc.kill()
    if os.path.exists(register):
        os.remove(register)
    return times, boot_ms


def avg(xs):
    return sum(xs) / len(xs) if xs else 0.0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--image", default="")
    ap.add_argument("--python", default=sys.executable)
    ap.add_argument("--rounds", type=int, default=3)
    args = ap.parse_args()

    image = get_image(args.image)
    print("测试图：%s" % image)
    print("解释器：%s\n" % args.python)

    print("[A] 一次性 spawn（每次重新加载引擎）")
    spawn = bench_spawn(args.python, image, args.rounds)

    print("\n[B] 常驻守护进程（引擎只加载一次）")
    daemon, boot_ms = bench_daemon(args.python, image, args.rounds)

    a, b = avg(spawn), avg(daemon)
    print("\n===== 结论 =====")
    print("spawn  平均 %6.0f ms/次" % a)
    print("daemon 平均 %6.0f ms/次（首次启动另付 %.0f ms，一次性）" % (b, boot_ms))
    if b > 0:
        print("每轮节省 %6.0f ms（%.1f×）" % (a - b, a / b))
    print("N 次要 OCR 的流程：spawn ≈ %.1f s，daemon ≈ %.1f s（N=10，含启动）"
          % (a * 10 / 1000, (boot_ms + b * 10) / 1000))


if __name__ == "__main__":
    main()
