// caps.go 节点能力清单：/cluster/capabilities 的响应结构与收集输入。
// 结构定义在本包（server 侧收集、router/成员表消费共用）；数据源由
// server 装配处注入（工具 spec、插件管理器、模型表），cluster 包不反向
// 依赖 server / plugin。
package cluster

// Capabilities 节点能力清单。
type Capabilities struct {
	NodeID   string       `json:"node_id"`
	Name     string       `json:"name"`
	Version  string       `json:"version"`  // gh 版本（config.Version）
	Platform string       `json:"platform"` // runtime.GOOS/GOARCH
	Tools    []string     `json:"tools"`    // 可用内置工具名（spec 全集）
	Plugins  []PluginCaps `json:"plugins,omitempty"`
	Models   []string     `json:"models,omitempty"` // "provider/model" 条目（不含密钥）
	TS       int64        `json:"ts"`
}

// PluginCaps 单个已接入插件的能力摘要。
type PluginCaps struct {
	Name  string `json:"name"`
	Tools int    `json:"tools"`
}
