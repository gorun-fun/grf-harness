// Package plugin 把外部 MCP 插件接入 harness：本地可执行文件（stdio 模式，
// harness 直接 spawn）或远程端点（Streamable HTTP 模式，远端以
// `插件 serve` 启动）。连接成功后枚举工具并注册进 tools.Registry，插件
// 工具对模型与内置工具同等可见。
//
// 连接为进程级共享（同一进程内多个 harness 复用同一批连接，浏览器等
// 有状态插件的上下文跨会话保留）；`go test` 下自动空转，测试构建
// harness 不会拉起外部插件。连接失败只降级（警告进系统提示词），不致命。
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/mcp"
	"git.gorun.fun/share/grf-harness/tools"
)

// DefaultToolTimeout 单次插件工具调用超时（config.Plugin.Timeout 可覆盖）。
const DefaultToolTimeout = 120 * time.Second

// handshakeTimeout 建连+握手+枚举工具的整体时限。
const handshakeTimeout = 15 * time.Second

// toolNameRe 模型可见工具名的安全字符集（OpenAI function 约定）。
var toolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Conn 一条插件连接（含展示信息与调用超时）。
type Conn struct {
	cli     *mcp.Client
	Name    string // 插件名（配置名 / 可执行文件名 / 主机）
	Mode    string // "stdio" 或 "http"
	Where   string // 命令行或端点 URL（诊断展示）
	Timeout time.Duration

	// deadErr 连接失败信息（首次握手失败即记；连接成功后为 nil）。
	// connect 完成后不可变，发布经 Manager.mu，读取无需加锁。
	deadErr error
}

// DeadErr 首次连接失败信息（nil = 连接正常）。
func (c *Conn) DeadErr() error { return c.deadErr }

// ServerInfo 服务端自述（未连接返回零值）。
func (c *Conn) ServerInfo() mcp.ServerInfo {
	if c.cli == nil {
		return mcp.ServerInfo{}
	}
	return c.cli.ServerInfo()
}

// toolEntry 已发现的插件工具。
type toolEntry struct {
	conn     *Conn
	def      mcp.Tool
	plugin   string
	readonly bool
}

// Manager 一批插件连接与其工具清单。
type Manager struct {
	mu       sync.Mutex
	conns    []*Conn
	tools    map[string]*toolEntry
	warnings []string
	loaded   bool // 已执行过 Load（含空结果）
}

// New 空管理器（gh plugins 体检等场景用独立实例；会话内统一走 Shared）。
func New() *Manager { return &Manager{tools: map[string]*toolEntry{}} }

// Shared 进程级共享单例：同一进程只连接一次（gh server 每会话重建
// harness 时复用连接，浏览器等有状态插件上下文得以保留）。go test 下
// 返回空壳（不连接任何外部进程），保证测试无副作用。
func Shared(settings *config.Settings) *Manager {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared != nil {
		return shared
	}
	shared = &Manager{}
	if !testing.Testing() {
		shared.Load(settings)
	} else {
		shared.loaded = true
	}
	return shared
}

var (
	sharedMu sync.Mutex
	shared   *Manager
)

// Load 按配置构建并连接全部插件（阻塞完成握手与工具枚举；单插件失败
// 记 warning 继续）。settings.Plugins 为 nil 时自动发现 ~/.grf/plugins。
func (m *Manager) Load(settings *config.Settings) {
	m.mu.Lock()
	if m.loaded {
		m.mu.Unlock()
		return
	}
	m.loaded = true
	m.tools = map[string]*toolEntry{}
	m.mu.Unlock()

	entries := resolveEntries(settings)
	// 并发连接（本地进程 spawn 与远程握手都可慢）
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e config.Plugin) {
			defer wg.Done()
			m.connect(e)
		}(e)
	}
	wg.Wait()
}

// resolveEntries 配置项 + 自动发现，按名字去重（配置优先）。
func resolveEntries(settings *config.Settings) []config.Plugin {
	if settings == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []config.Plugin
	for _, p := range settings.Plugins {
		name := pluginName(p)
		if name == "" {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, p)
	}
	if settings.Plugins == nil {
		for _, p := range discoverPlugins() {
			name := pluginName(p)
			if !seen[name] {
				seen[name] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// pluginName 插件展示名：显式配置名 > 可执行文件名 > URL 主机。
func pluginName(p config.Plugin) string {
	if p.Name != "" {
		return p.Name
	}
	if len(p.Command) > 0 && p.Command[0] != "" {
		base := p.Command[0]
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		if i := strings.LastIndexByte(base, '\\'); i >= 0 {
			base = base[i+1:]
		}
		return base
	}
	if p.URL != "" {
		s := p.URL
		s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
		if i := strings.IndexAny(s, "/:?"); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return ""
}

// connect 建连 + 枚举工具 + 登记（失败记 deadErr + warning）。
func (m *Manager) connect(e config.Plugin) {
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = int(DefaultToolTimeout / time.Second)
	}
	conn := &Conn{Name: pluginName(e), Timeout: time.Duration(timeout) * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	var cli *mcp.Client
	var err error
	if len(e.Command) > 0 {
		conn.Mode, conn.Where = "stdio", strings.Join(e.Command, " ")
		cli, err = mcp.DialStdio(ctx, e.Command, nil)
	} else if e.URL != "" {
		conn.Mode, conn.Where = "http", e.URL
		cli, err = mcp.DialHTTP(ctx, e.URL, e.Token)
	} else {
		m.warnf("插件 %q: command 与 url 均未配置，跳过", conn.Name)
		return
	}
	if err != nil {
		conn.deadErr = err
		m.warnf("插件 %s 连接失败: %v（该插件工具不可用，其余不受影响）", conn.Name, err)
		m.addConn(conn)
		return
	}
	conn.cli = cli
	toolsList, err := cli.ListTools(ctx)
	if err != nil {
		conn.deadErr = err
		m.warnf("插件 %s 枚举工具失败: %v", conn.Name, err)
		m.addConn(conn)
		return
	}
	m.addConn(conn)
	for _, t := range toolsList {
		m.addTool(conn, t)
	}
}

func (m *Manager) addConn(c *Conn) {
	m.mu.Lock()
	m.conns = append(m.conns, c)
	m.mu.Unlock()
}

func (m *Manager) addTool(c *Conn, t mcp.Tool) {
	switch {
	case !toolNameRe.MatchString(t.Name):
		m.warnf("插件 %s: 工具名 %q 不合法（限 [a-zA-Z0-9_-]{1,64}），跳过", c.Name, t.Name)
		return
	case specHasTool(t.Name):
		m.warnf("插件 %s: 工具 %q 与内置工具重名，跳过", c.Name, t.Name)
		return
	}
	readonly := t.Annotations != nil && t.Annotations.ReadOnlyHint
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tools == nil {
		m.tools = map[string]*toolEntry{}
	}
	if _, dup := m.tools[t.Name]; dup {
		m.warnf("工具 %q 在多个插件中重复出现，取先连接者（%s）", t.Name, m.tools[t.Name].plugin)
		return
	}
	m.tools[t.Name] = &toolEntry{conn: c, def: t, plugin: c.Name, readonly: readonly}
}

func (m *Manager) warn(s string) {
	m.mu.Lock()
	m.warnings = append(m.warnings, s)
	m.mu.Unlock()
}

func (m *Manager) warnf(format string, args ...any) { m.warn(fmt.Sprintf(format, args...)) }

// Warnings 连接期警告（进系统提示词，让模型知晓哪些插件不可用）。
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.warnings...)
}

// Conns 插件连接清单（gh plugins 体检展示用）。
func (m *Manager) Conns() []*Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Conn(nil), m.conns...)
}

// ToolInfo 单个插件工具的归属与只读标记（展示用）。
type ToolInfo struct {
	Plugin   string
	ReadOnly bool
}

// Tools 工具名 → 归属信息。
func (m *Manager) Tools() map[string]ToolInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]ToolInfo, len(m.tools))
	for n, e := range m.tools {
		out[n] = ToolInfo{Plugin: e.plugin, ReadOnly: e.readonly}
	}
	return out
}

// RegisterInto 把插件工具注册进一个 agent 的注册表；返回计划模式下应
// 拦截的工具名集合（未标只读的插件工具按改动类处理）。根与子代理各调
// 一次；注册表不同、底层连接共享。
func (m *Manager) RegisterInto(reg *tools.Registry) map[string]bool {
	blocked := map[string]bool{}
	m.mu.Lock()
	entries := make([]*toolEntry, 0, len(m.tools))
	for _, e := range m.tools {
		entries = append(entries, e)
	}
	m.mu.Unlock()
	for _, e := range entries {
		entry := e
		if !entry.readonly {
			blocked[entry.def.Name] = true
		}
		def := llm.Tool{
			Name:        entry.def.Name,
			Description: entry.def.Description + fmt.Sprintf("\n\n[grf 插件 %s]", entry.plugin),
			Parameters:  entry.def.InputSchema,
		}
		// 部分服务端（LM Studio 的 zod 校验）要求 parameters.properties
		// 也是对象，nil 兜底必须带上空 properties
		if def.Parameters == nil || def.Parameters["properties"] == nil {
			def.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		reg.RegisterCustom(def, entry.conn.handler(entry.def.Name))
	}
	return blocked
}

// handler 生成转发到 tools/call 的工具处理器。
func (c *Conn) handler(name string) tools.Handler {
	return func(ctx context.Context, args string) (tools.Result, error) {
		res, err := c.CallTool(ctx, name, json.RawMessage(args))
		if err != nil {
			return tools.Result{}, err
		}
		return toResult(res), nil
	}
}

// CallTool 带超时调用插件工具（超时取 conn.Timeout）。
func (c *Conn) CallTool(ctx context.Context, name string, args json.RawMessage) (mcp.CallResult, error) {
	if c.deadErr != nil {
		return mcp.CallResult{}, fmt.Errorf("插件 %s 不可用: %w", c.Name, c.deadErr)
	}
	if c.cli == nil {
		return mcp.CallResult{}, fmt.Errorf("插件 %s 未连接", c.Name)
	}
	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	return c.cli.CallTool(cctx, name, args)
}

// toResult MCP 内容 → harness 工具结果：text 拼接、image 走多模态回传。
func toResult(res mcp.CallResult) tools.Result {
	var lines []string
	var imgs []tools.Image
	for _, c := range res.Content {
		switch c.Type {
		case "text":
			lines = append(lines, c.Text)
		case "image":
			mime := c.MimeType
			if mime == "" {
				mime = "image/png"
			}
			imgs = append(imgs, tools.Image{Mime: mime, B64: c.Data})
		default:
			lines = append(lines, "["+c.Type+" 内容省略]")
		}
	}
	out := tools.Result{Text: strings.Join(lines, "\n"), Images: imgs}
	if res.IsError {
		if out.Text == "" {
			out.Text = "error: 插件工具执行失败"
		}
		out.Err = out.Text
	}
	return out
}

// Close 关闭全部插件连接（stdio 子进程随 stdin 关闭退出）。进程级共享
// 管理器通常不主动 Close（随主进程退出自然回收）；gh plugins 体检命令
// 用独立实例，查完即关。
func (m *Manager) Close() {
	m.mu.Lock()
	conns := m.conns
	m.conns = nil
	m.tools = map[string]*toolEntry{}
	m.mu.Unlock()
	for _, c := range conns {
		if c.cli != nil {
			_ = c.cli.Close()
		}
	}
}
