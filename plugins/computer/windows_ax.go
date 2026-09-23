// windows_ax.go Windows 可达性（UI Automation）实现：元素树/动作经
// PowerShell 调 .NET 自带的 System.Windows.Automation（ControlViewWalker
// 定序遍历、Invoke/Toggle/Value 等模式），应用/窗口枚举与前台判定经
// user32 P/Invoke。TSV 输出格式与 macOS 版一致（第 12 列为窗口 HWND，
// 供 computer_screenshot 的 window_id）。
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// psDump UIA 元素树遍历（TSV：同 macOS + 第 12 列窗口层=HWND）。
const psDump = `param($targPid,$maxDepth,$maxElems)
Add-Type -AssemblyName UIAutomationClient,UIAutomationTypes
$root=[System.Windows.Automation.AutomationElement]::RootElement
$cond=New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::ProcessIdProperty,$targPid)
$wins=$root.FindAll([System.Windows.Automation.TreeScope]::Children,$cond)
$walker=[System.Windows.Automation.TreeWalker]::ControlViewWalker
$script:count=0
function Clean($s){ if($null -eq $s -or $s -eq ''){return ''}; $t=$s.ToString(); if($t.Length -gt 90){$t=$t.Substring(0,90)}; return ($t -replace '[|\r\n\t]',' ') }
function Dump($el,$depth,$path){
  if($script:count -ge $maxElems){return}
  $c=$el.Current
  $role=($c.ControlType.ProgrammaticName -replace '^ControlType\.','')
  $val=''
  $vp=$null
  if($el.TryGetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern,[ref]$vp)){ $val=(Clean $vp.Current.Value) }
  $r=$c.BoundedRectangle
  Write-Output (@($depth,$path,$role,(Clean $c.Name),$val,(Clean $c.HelpText),[int]$r.X,[int]$r.Y,[int]$r.Width,[int]$r.Height,[int][bool]$c.IsEnabled) -join '|')
  $script:count++
  if($depth -ge $maxDepth){return}
  $i=1
  $kid=$walker.GetFirstChildElement($el)
  while($kid -and $script:count -lt $maxElems){
    Dump $kid ($depth+1) ($path+'.'+$i)
    $kid=$walker.GetNextSiblingElement($kid)
    $i++
  }
}
$wi=0
foreach($w in $wins){
  $wi++
  $c=$w.Current; $r=$c.BoundedRectangle
  Write-Output (@(0,$wi,'Window',(Clean $c.Name),'','',[int]$r.X,[int]$r.Y,[int]$r.Width,[int]$r.Height,[int][bool]$c.IsEnabled,$c.NativeWindowHandle) -join '|')
  $script:count++
  Dump $w 1 ([string]$wi)
}
if($wi -eq 0){ [Console]::Error.Write('ERR_NOWIN'); exit 3 }`

// psAction UIA 元素动作：按 path 重走（窗口序号.子序号...），校验角色/名
// 称后执行 press（Invoke/Toggle/SelectionItem/ExpandCollapse 模式）或
// set_value（ValuePattern）；无可用模式报 ERR_NOACTION 由坐标兜底。
const psAction = `param($targPid,$path,$wantRole,$wantTitle,$action,$value)
Add-Type -AssemblyName UIAutomationClient,UIAutomationTypes
$root=[System.Windows.Automation.AutomationElement]::RootElement
$cond=New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::ProcessIdProperty,$targPid)
$wins=$root.FindAll([System.Windows.Automation.TreeScope]::Children,$cond)
if($wins.Count -eq 0){ [Console]::Error.Write('ERR_NOPROC'); exit 3 }
$walker=[System.Windows.Automation.TreeWalker]::ControlViewWalker
$segs=$path.Split('.')
$el=$wins.Item([int]$segs[0]-1)
for($i=1;$i -lt $segs.Count -and $null -ne $el;$i++){
  $k=1; $kid=$walker.GetFirstChildElement($el)
  while($kid -and $k -lt [int]$segs[$i]){ $kid=$walker.GetNextSiblingElement($kid); $k++ }
  $el=$kid
}
if($null -eq $el){ [Console]::Error.Write('ERR_CHANGED element gone'); exit 4 }
$role=($el.Current.ControlType.ProgrammaticName -replace '^ControlType\.','')
$name=$el.Current.Name
if($role -ne $wantRole){ [Console]::Error.Write("ERR_CHANGED role=$role"); exit 4 }
if($wantTitle -ne '' -and $name -ne $wantTitle){ [Console]::Error.Write("ERR_CHANGED name=$name"); exit 4 }
if($action -eq 'press'){
  $p=$null
  if($el.TryGetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern,[ref]$p)){ $p.Invoke() }
  elseif($el.TryGetCurrentPattern([System.Windows.Automation.TogglePattern]::Pattern,[ref]$p)){ $p.Toggle() }
  elseif($el.TryGetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern,[ref]$p)){ $p.Select() }
  elseif($el.TryGetCurrentPattern([System.Windows.Automation.ExpandCollapsePattern]::Pattern,[ref]$p)){ $p.Expand() }
  else{ [Console]::Error.Write('ERR_NOACTION no pattern'); exit 5 }
  Write-Output 'pressed'
} elseif($action -eq 'set_value'){
  $p=$null
  if(-not $el.TryGetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern,[ref]$p)){ [Console]::Error.Write('ERR_NOACTION no value pattern'); exit 5 }
  $p.SetValue($value)
  Write-Output 'set'
} elseif($action -eq 'locate'){
  $r=$el.Current.BoundedRectangle
  Write-Output ([int]($r.X+$r.Width/2)).ToString()+','+([int]($r.Y+$r.Height/2)).ToString()
} else{ [Console]::Error.Write('ERR_BADACTION'); exit 6 }`

// psApps 运行中 GUI 应用（pid|名|前台）。
const psApps = `Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public class GrfFg {
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
}
'@;
$fg=[GrfFg]::GetForegroundWindow(); $fgpid=0; [void][GrfFg]::GetWindowThreadProcessId($fg,[ref]$fgpid)
Get-Process | Where-Object {$_.MainWindowHandle -ne 0} | ForEach-Object { "$($_.Id)|$($_.ProcessName)|$([int]($_.Id -eq $fgpid))" }`

// psWindows 指定进程的顶层窗口（id|标题|x|y|w|h|主窗口）。
const psWindows = `param($targPid)
Add-Type -AssemblyName UIAutomationClient,UIAutomationTypes
$root=[System.Windows.Automation.AutomationElement]::RootElement
$cond=New-Object System.Windows.Automation.PropertyCondition([System.Windows.Automation.AutomationElement]::ProcessIdProperty,$targPid)
$wins=$root.FindAll([System.Windows.Automation.TreeScope]::Children,$cond)
$first=$true
foreach($w in $wins){
  $c=$w.Current; $r=$c.BoundedRectangle
  "$($c.NativeWindowHandle)|$($c.Name)|$([int]$r.X)|$([int]$r.Y)|$([int]$r.Width)|$([int]$r.Height)|$([int]$first)"
  $first=$false
}`

// psActivate 激活/启动应用。
const psActivate = `param($name)
$p=Get-Process -Name $name -ErrorAction SilentlyContinue | Where-Object {$_.MainWindowHandle -ne 0} | Select-Object -First 1
if($p){ $ws=New-Object -ComObject WScript.Shell; [void]$ws.AppActivate($p.Id) } else { Start-Process $name }`

func (w windowsPlatform) Apps(ctx context.Context) ([]AppInfo, error) {
	out, err := ps(ctx, psApps)
	if err != nil {
		return nil, err
	}
	var apps []AppInfo
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 3 {
			continue
		}
		apps = append(apps, AppInfo{PID: atoi(f[0]), Name: f[1], Frontmost: f[2] == "1"})
	}
	return apps, nil
}

func (w windowsPlatform) AppWindows(ctx context.Context, pid int) ([]WinInfo, error) {
	out, err := runScript(ctx, 30e9, []string{"powershell", "-NoProfile", "-NonInteractive", "-File"}, ".ps1", psWindows, strconv.Itoa(pid))
	if err != nil {
		return nil, err
	}
	var wins []WinInfo
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 7 {
			continue
		}
		id, _ := strconv.ParseInt(f[0], 10, 64)
		wins = append(wins, WinInfo{ID: id, PID: pid, Title: f[1],
			X: atoi(f[2]), Y: atoi(f[3]), W: atoi(f[4]), H: atoi(f[5]), Main: f[6] == "1"})
	}
	return wins, nil
}

func (w windowsPlatform) DumpElements(ctx context.Context, pid int, maxDepth int) ([]*Node, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	if maxDepth > 15 {
		maxDepth = 15
	}
	out, err := runScript(ctx, 60e9, []string{"powershell", "-NoProfile", "-NonInteractive", "-File"}, ".ps1", psDump,
		strconv.Itoa(pid), strconv.Itoa(maxDepth), "300")
	if err != nil {
		if strings.Contains(err.Error(), "ERR_NOWIN") {
			return nil, fmt.Errorf("进程 %d 无可读窗口", pid)
		}
		return nil, err
	}
	nodes := parseTSVNodes(out)
	if len(nodes) == 0 {
		return nil, fmt.Errorf("进程 %d 无可读窗口", pid)
	}
	return nodes, nil
}

func (w windowsPlatform) ElementAction(ctx context.Context, pid int, path, wantRole, wantTitle, action, value string) (ElemActionResult, error) {
	out, err := runScript(ctx, 30e9, []string{"powershell", "-NoProfile", "-NonInteractive", "-File"}, ".ps1", psAction,
		strconv.Itoa(pid), path, wantRole, wantTitle, action, value)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "ERR_CHANGED"):
			return ElemActionResult{}, &errChanged{detail: "元素签名不符（" + msg + "）"}
		case strings.Contains(msg, "ERR_NOACTION") && action == "press":
			return w.ElementAction(ctx, pid, path, wantRole, wantTitle, "locate", "")
		case strings.Contains(msg, "ERR_NOPROC"):
			return ElemActionResult{}, fmt.Errorf("进程 %d 已退出", pid)
		}
		return ElemActionResult{}, err
	}
	out = strings.TrimSpace(out)
	if action == "locate" {
		xy := strings.SplitN(out, ",", 2)
		if len(xy) == 2 {
			return ElemActionResult{HasPos: true, X: atoi(xy[0]), Y: atoi(xy[1])}, nil
		}
		return ElemActionResult{}, fmt.Errorf("定位输出异常 %q", out)
	}
	return ElemActionResult{Text: out}, nil
}

func (w windowsPlatform) ActivateApp(ctx context.Context, name string) error {
	_, err := runScript(ctx, 30e9, []string{"powershell", "-NoProfile", "-NonInteractive", "-File"}, ".ps1", psActivate, name)
	if err != nil {
		return fmt.Errorf("无法启动/聚焦应用 %q（名称见 computer_list_apps，通常为不带 .exe 的进程名）: %w", name, err)
	}
	return nil
}

// psWindowRectShot 按 HWND 取窗口矩形并截屏到指定文件。
const psWindowRectShot = `param($hwnd,$outFile)
Add-Type -AssemblyName System.Windows.Forms,System.Drawing
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public class GrfWr {
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L,T,R,B; }
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
}
'@;
$r=New-Object GrfWr+RECT; [void][GrfWr]::GetWindowRect([IntPtr][int64]$hwnd,[ref]$r)
$bmp=New-Object System.Drawing.Bitmap(($r.R-$r.L),($r.B-$r.T))
$g=[System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($r.L,$r.T,0,0,$bmp.Size)
$bmp.Save($outFile,[System.Drawing.Imaging.ImageFormat]::Png)`

func (w windowsPlatform) ScreenshotWindow(ctx context.Context, winID int64) ([]byte, error) {
	tmp := tempFile("png")
	defer removeFile(tmp)
	if _, err := runScript(ctx, 30e9, []string{"powershell", "-NoProfile", "-NonInteractive", "-File"}, ".ps1", psWindowRectShot,
		strconv.FormatInt(winID, 10), tmp); err != nil {
		return nil, err
	}
	return readFile(tmp)
}
