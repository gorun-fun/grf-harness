// callhome.go 节点侧 call-home：向指挥官注册并周期心跳（NAT 友好——
// 指挥官无需反向可达节点）。失败容忍：注册/心跳出错静默重试（集群是
// 增强能力，不应因指挥官暂不可达影响节点本职服务）。
package cluster

import (
	"context"
	"time"
)

// callHomeInterval 心跳周期（与成员表 heartbeatTTL = 6 个周期配套）。
const callHomeInterval = 15 * time.Second

// CallHome 注册并心跳直到 ctx 取消。running 回调上报节点负载（在跑
// 回合数；可为 nil）。commanderURL 为指挥官基地址，token 为集群共享
// 密钥（指挥官 /cluster/* 的鉴权依据）。
func CallHome(ctx context.Context, commanderURL, token string, self Identity, running func() int) {
	c := NewNodeClient(commanderURL, token)
	reg := RegisterRequest{NodeID: self.ID, Name: self.Name}
	if caps, err := c.Capabilities(ctx); err == nil {
		reg.Capabilities = caps
	}
	registered := false
	tick := func() {
		if !registered {
			if err := c.Register(ctx, reg); err != nil {
				return // 指挥官暂不可达：下个周期重试
			}
			registered = true
			return
		}
		n := 0
		if running != nil {
			n = running()
		}
		_ = c.Heartbeat(ctx, HeartbeatRequest{NodeID: self.ID, Name: self.Name, Running: n})
	}
	tick()
	t := time.NewTicker(callHomeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
