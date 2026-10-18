// fromconfig.go 生效配置 → 客户端的统一装配：config.Effective 与单个
// Provider 条目两形态。此前 app/server/weixin/tui 四表面各自逐字段拼
// Config（漂移温床），收敛于此——llm 依赖 config 属能力层→基础层的
// 合法方向。
package llm

import "git.gorun.fun/share/grf-harness/config"

// NewFromEffective 按生效配置构建客户端（config.Load 的结果直入）。
func NewFromEffective(eff config.Effective) *Client {
	return NewClient(Config{Provider: eff.Provider, BaseURL: eff.BaseURL, APIKey: eff.APIKey, Model: eff.Model})
}

// NewFromProvider 按单个供应商条目构建客户端（模型探测/会话内切模型
// 等不走 Effective 的场景；model 可空）。
func NewFromProvider(provider string, p config.Provider, model string) *Client {
	return NewClient(Config{Provider: provider, BaseURL: p.BaseURL, APIKey: p.APIKey, Model: model})
}
