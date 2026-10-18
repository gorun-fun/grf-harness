// stdio.go 本地插件传输：spawn 子进程，行分隔 JSON-RPC 走 stdin/stdout
// （MCP stdio 约定），stderr 环形截留用于错误诊断。服务端通知/反向请求
// 由读 goroutine 丢弃；进程退出会使连接进入 dead 状态，后续调用报错。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// stdioTransport 一条 stdio 插件连接。
type stdioTransport struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	wmu     sync.Mutex // 写互斥
	nextID  int64
	idMu    sync.Mutex
	pmu     sync.Mutex
	pending map[int64]chan rpcResponse

	deadMu   sync.Mutex
	deadErr  error // 非空 = 连接已失效（进程退出/写失败）
	stderrMu sync.Mutex
	stderr   []byte // 尾部截留（最多 stderrCap）
	done     chan struct{}
	waitDone chan struct{} // 进程回收完毕（stderr 泵已写尽）
}

const stderrCap = 8 << 10

// stderrTailWait 取 stderr 尾部前等进程回收的上限：孙进程（如插件拉起的
// 浏览器）继承 stderr 管道会让 cmd.Wait 长期不返回，超时即拿现有尾部，
// 尽力而为。
const stderrTailWait = 500 * time.Millisecond

// DialStdio 启动本地插件进程（command = 可执行文件 + 参数）并完成握手。
// extraEnv 追加到子进程环境（默认继承当前进程环境）。
func DialStdio(ctx context.Context, command []string, extraEnv []string) (*Client, error) {
	if len(command) == 0 || command[0] == "" {
		return nil, fmt.Errorf("stdio 插件命令为空")
	}
	t, err := startStdio(command, extraEnv)
	if err != nil {
		return nil, err
	}
	c, err := dial(ctx, t)
	if err != nil {
		// 握手失败：tr.Close 已被 dial 调用；确保进程回收
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
		return nil, err
	}
	return c, nil
}

func startStdio(command []string, extraEnv []string) (*stdioTransport, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin, pending: map[int64]chan rpcResponse{},
		done: make(chan struct{}), waitDone: make(chan struct{})}
	cmd.Stderr = t // 经 Write 尾部截留（exec 以管道 + 拷贝 goroutine 接泵）
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动插件 %s: %w", command[0], err)
	}
	go t.readLoop(stdout)
	go func() {
		<-t.done
		_ = t.cmd.Wait() // 回收进程，避免僵尸；返回即 stderr 拷贝 goroutine 已写尽
		close(t.waitDone)
	}()
	return t, nil
}

// readLoop 逐行读 stdout 并分发响应；EOF/解析无关行（如插件误写日志到
// stdout）都容忍——只有结构正确的响应才投递。
func (t *stdioTransport) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			t.handleLine(line)
		}
		if err != nil {
			close(t.done) // 唤醒进程回收 goroutine（唯一的退出路径）
			if tail := strings.TrimSpace(t.drainStderrTail()); tail != "" {
				t.markDead(fmt.Errorf("插件进程已退出: %s", tail))
			} else {
				t.markDead(fmt.Errorf("插件进程已退出"))
			}
			return
		}
	}
}

func (t *stdioTransport) handleLine(line []byte) {
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return // stdout 上的非协议内容，丢弃
	}
	if resp.Method != "" {
		return // 服务端通知/反向请求，v1 忽略
	}
	t.pmu.Lock()
	ch, ok := t.pending[resp.ID]
	delete(t.pending, resp.ID)
	t.pmu.Unlock()
	if ok {
		select {
		case ch <- resp:
		default:
		}
	}
}

func (t *stdioTransport) markDead(err error) {
	t.deadMu.Lock()
	if t.deadErr == nil {
		t.deadErr = err
	}
	t.deadMu.Unlock()
	t.failAll()
}

// failAll 失败全部未决请求（带连接失效的真实原因）。
func (t *stdioTransport) failAll() {
	t.deadMu.Lock()
	err := t.deadErr
	t.deadMu.Unlock()
	if err == nil {
		err = fmt.Errorf("connection closed")
	}
	t.pmu.Lock()
	for id, ch := range t.pending {
		select {
		case ch <- rpcResponse{ID: id, Error: &rpcError{Code: -32000, Message: err.Error()}}:
		default:
		}
		delete(t.pending, id)
	}
	t.pmu.Unlock()
}

func (t *stdioTransport) isDead() error {
	t.deadMu.Lock()
	defer t.deadMu.Unlock()
	return t.deadErr
}

// Write 实现 io.Writer：exec.Cmd 把子进程 stderr 逐段写到这里，尾部
// 截留最近 stderrCap 字节（超出丢头部）。
func (t *stdioTransport) Write(p []byte) (int, error) {
	t.stderrMu.Lock()
	t.stderr = append(t.stderr, p...)
	if n := len(t.stderr); n > stderrCap {
		t.stderr = append(t.stderr[:0], t.stderr[n-stderrCap:]...)
	}
	t.stderrMu.Unlock()
	return len(p), nil
}

// drainStderrTail 等进程回收（stderr 已写尽）后取尾部截留；孙进程占住
// stderr 管道时至多等 stderrTailWait，拿现有尾部尽力而为。
func (t *stdioTransport) drainStderrTail() string {
	select {
	case <-t.waitDone:
	case <-time.After(stderrTailWait):
	}
	return t.Stderr()
}

// Stderr 尾部截留的诊断输出（进程异常退出时带回给调用方）。
func (t *stdioTransport) Stderr() string {
	t.stderrMu.Lock()
	defer t.stderrMu.Unlock()
	return string(t.stderr)
}

func (t *stdioTransport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := t.isDead(); err != nil {
		return nil, fmt.Errorf("%w（stderr: %s）", err, t.Stderr())
	}
	t.idMu.Lock()
	t.nextID++
	id := t.nextID
	t.idMu.Unlock()
	ch := make(chan rpcResponse, 1)
	t.pmu.Lock()
	t.pending[id] = ch
	t.pmu.Unlock()
	defer func() {
		t.pmu.Lock()
		delete(t.pending, id)
		t.pmu.Unlock()
	}()

	raw, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	t.wmu.Lock()
	if _, err := t.stdin.Write(append(raw, '\n')); err != nil {
		t.wmu.Unlock()
		t.markDead(fmt.Errorf("写插件 stdin: %w", err))
		return nil, fmt.Errorf("写插件 stdin: %w", err)
	}
	t.wmu.Unlock()

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-t.done:
		return nil, fmt.Errorf("插件进程已退出（stderr: %s）", t.drainStderrTail())
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *stdioTransport) Notify(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_, err = t.stdin.Write(append(raw, '\n'))
	return err
}

// Close 关闭 stdin（插件按约定自行退出），最多等 3 秒后强杀。
func (t *stdioTransport) Close() error {
	if t.isDead() != nil {
		return nil // 已退出
	}
	_ = t.stdin.Close()
	done := make(chan struct{})
	go func() {
		<-t.done
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		_ = t.cmd.Process.Kill()
		return nil
	}
}
