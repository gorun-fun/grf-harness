#!/usr/bin/env python3
"""PTY 冒烟（全屏 TUI）：选项题 → ✎ 自定义输入 行 → 手输自由文本作答。"""
import os, pty, select, sys, time, fcntl, termios, struct, re

ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b.")

def read_all(fd, timeout):
    buf = b""
    end = time.time() + timeout
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.3)
        if r:
            try:
                chunk = os.read(fd, 65536)
            except OSError:
                break
            if not chunk:
                break
            buf += chunk
            end = time.time() + 2.0
    return buf.decode("utf-8", "replace")

def visible(s):
    return ANSI.sub("", s)

pid, fd = pty.fork()
if pid == 0:
    os.environ["TERM"] = "xterm-256color"
    os.execv("/Users/wenjun/AiProjects/grf-harness/gh", ["./gh"])

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))

ok = True
try:
    read_all(fd, 5)  # 等首帧
    # 1) 在输入框提交任务
    os.write(fd, "必须马上调用 ask_user_question 工具（不要在聊天文本里提问）：单个问题，id 为 mode，两个选项 A 和 B。得到用户答案后逐字复述原话，然后结束。\r".encode())
    out = read_all(fd, 120)
    vis = visible(out)
    if "✎ 自定义输入" not in vis:
        print("FAIL: 表单未出现"); print(vis[-3000:]); ok = False
    else:
        print("STEP1 OK: 全屏表单出现且含 ✎ 自定义输入 行")
        # 2) ↓↓ 到自定义行 → 回车进手输 → 打字 → 回车提交
        os.write(fd, b"\x1b[B\x1b[B\r")
        time.sleep(0.5)
        os.write(fd, "手输答案ABC456".encode())
        time.sleep(0.3)
        os.write(fd, b"\r")
        out2 = read_all(fd, 120)
        vis2 = visible(out2)
        if "手输答案ABC456" in vis2:
            print("STEP2 OK: agent 收到并复述了手输答案")
        else:
            print("FAIL: 未见复述"); print(vis2[-3000:]); ok = False
finally:
    os.write(fd, b"\x03")
    time.sleep(0.5)
    try: os.close(fd)
    except OSError: pass
    try: os.waitpid(pid, os.WNOHANG)
    except OSError: pass

print("SMOKE(tui)", "PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
