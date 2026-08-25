#!/usr/bin/env python3
"""PTY 冒烟（行模式）：ask_user_question 选项题 → 直接输入自定义文字作答。"""
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
    os.execv("/Users/wenjun/AiProjects/grf-harness/gh", ["./gh", "必须马上调用 ask_user_question 工具（不要在聊天文本里提问）：单个问题，id 为 mode，两个选项 A 和 B。得到用户答案后逐字复述原话，然后结束，不要做任何其他事。"])

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 30, 100, 0, 0))

ok = True
try:
    out = read_all(fd, 120)
    vis = visible(out)
    if "序号或自定义文字" not in vis:
        print("FAIL: 行模式提示未出现"); print(vis[-2000:]); ok = False
    else:
        print("STEP1 OK: 行模式选项题提示已出现（支持自定义文字）")
        os.write(fd, "自定义答案XYZ789\n".encode())
        out2 = read_all(fd, 120)
        vis2 = visible(out2)
        if "自定义答案XYZ789" in vis2:
            print("STEP2 OK: agent 收到并复述了自定义答案")
        else:
            print("FAIL: 未见复述"); print(vis2[-2000:]); ok = False
finally:
    os.write(fd, b"\x03")
    time.sleep(0.5)
    try: os.close(fd)
    except OSError: pass
    try: os.waitpid(pid, os.WNOHANG)
    except OSError: pass

print("SMOKE(line-mode)", "PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
