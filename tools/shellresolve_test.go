// shellresolve_test.go bash 工具的 windows shell 解析与 PowerShell 参数
// 包装：bash 契约优先（Git Bash 安装位 → PATH 非 WSL bash.exe），均无退
// 回 PowerShell；WSL shim（System32\bash.exe）跑在 Linux 侧必须跳过。
// resolveShell 是纯函数（goos 注入），非 windows 平台同样可测。
package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeShellEnv 构造 lookPath/env 注入。
func fakeShellEnv(found map[string]string, env map[string]string) (func(string) (string, error), func(string) string) {
	look := func(name string) (string, error) {
		if p, ok := found[name]; ok {
			return p, nil
		}
		return "", &os.PathError{Op: "exec", Path: name, Err: os.ErrNotExist}
	}
	get := func(k string) string { return env[k] }
	return look, get
}

func TestResolveShellNonWindows(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		look, env := fakeShellEnv(nil, nil)
		sh := resolveShell(goos, look, env)
		if sh.name != "bash" || sh.exe != "bash" {
			t.Fatalf("%s: 期望 bash，得到 %s %s", goos, sh.name, sh.exe)
		}
		if got := sh.args("echo hi"); len(got) != 2 || got[0] != "-c" || got[1] != "echo hi" {
			t.Fatalf("%s: bash 参数应为 [-c cmd]，得到 %v", goos, got)
		}
	}
}

func TestResolveWindowsPrefersGitBashInstallDir(t *testing.T) {
	dir := t.TempDir()
	gitBin := filepath.Join(dir, "Git", "bin")
	if err := os.MkdirAll(gitBin, 0o755); err != nil {
		t.Fatal(err)
	}
	bashExe := filepath.Join(gitBin, "bash.exe")
	if err := os.WriteFile(bashExe, []byte{0}, 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH 上只有 WSL shim 与 pwsh：安装位 bash 仍应胜出
	look, env := fakeShellEnv(
		map[string]string{"bash": `C:\Windows\System32\bash.exe`, "pwsh": `C:\pwsh.exe`},
		map[string]string{"ProgramFiles": dir},
	)
	sh := resolveShell("windows", look, env)
	if sh.name != "bash" || !strings.HasSuffix(filepath.ToSlash(sh.exe), "Git/bin/bash.exe") {
		t.Fatalf("期望安装位 bash，得到 %s %s", sh.name, sh.exe)
	}
}

func TestResolveWindowsPathBashAndWSLShim(t *testing.T) {
	// PATH 上非 System32 的 bash.exe 可用
	look, env := fakeShellEnv(map[string]string{"bash": `C:\Git\bin\bash.exe`}, nil)
	if sh := resolveShell("windows", look, env); sh.name != "bash" || sh.exe != `C:\Git\bin\bash.exe` {
		t.Fatalf("期望 PATH bash，得到 %s %s", sh.name, sh.exe)
	}
	// 只有 System32 的 WSL shim：跳过，退回 PowerShell
	look, env = fakeShellEnv(map[string]string{"bash": `C:\Windows\System32\bash.exe`}, nil)
	if sh := resolveShell("windows", look, env); sh.name != "powershell" {
		t.Fatalf("WSL shim 应被跳过退回 powershell，得到 %s %s", sh.name, sh.exe)
	}
}

func TestResolveWindowsPowerShellFallback(t *testing.T) {
	// bash 缺失：pwsh 在则优先
	look, env := fakeShellEnv(map[string]string{"pwsh": `C:\pwsh.exe`}, nil)
	if sh := resolveShell("windows", look, env); sh.exe != `C:\pwsh.exe` {
		t.Fatalf("期望 pwsh，得到 %s", sh.exe)
	}
	// 全缺失：系统自带 powershell（LookPath 也找不到时按名直呼，报错可诊断）
	look, env = fakeShellEnv(nil, nil)
	if sh := resolveShell("windows", look, env); sh.name != "powershell" || sh.exe != "powershell" {
		t.Fatalf("期望 powershell 兜底，得到 %s %s", sh.name, sh.exe)
	}
}

func TestPowerShellArgsWrap(t *testing.T) {
	args := powerShellArgs("Get-ChildItem")
	if len(args) != 3 || args[0] != "-NoProfile" || args[1] != "-Command" {
		t.Fatalf("参数形状不对: %v", args)
	}
	script := args[2]
	for _, want := range []string{
		"[System.Text.Encoding]::UTF8", // 输出统一 UTF-8（默认 OEM 码页中文乱码）
		"Get-ChildItem",                // 命令原文
		"exit $LASTEXITCODE",           // 原生命令退出码透传为进程退出码
		"if (-not $?) { exit 1 }",      // cmdlet 失败不静默成功
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("包装脚本缺 %q:\n%s", want, script)
		}
	}
}

func TestShellNoteMatchesShell(t *testing.T) {
	// 本机（非 windows 测试环境）恒解析为 bash：无需提示词说明
	if note := ShellNote(); note != "" {
		t.Fatalf("bash 环境不应有 shell 提示词说明，得到: %s", note)
	}
}
