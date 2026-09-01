package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"git.gorun.fun/share/grf-harness/config"
	"git.gorun.fun/share/grf-harness/llm"
	"git.gorun.fun/share/grf-harness/tools"
)

// 回归：DSML invoke 变体（parameter 带 string="true"）+ goal 自动续跑 +
// LLM 长时间不响应时，ctx 取消必须能终止循环（复现 test 项目卡死）。
func TestDSMLGoalContinuationCancelable(t *testing.T) {
	createDSML := "<｜｜DSML｜｜tool_calls>\n<｜｜DSML｜｜invoke name=\"create_goal\">\n<｜｜DSML｜｜parameter name=\"objective\" string=\"true\">测试目标管理功能</｜｜DSML｜｜parameter>\n</｜｜DSML｜｜invoke>\n</｜｜DSML｜｜tool_calls>"
	updateDSML := "<｜｜DSML｜｜tool_calls>\n<｜｜DSML｜｜invoke name=\"update_goal\">\n<｜｜DSML｜｜parameter name=\"action\" string=\"true\">edit</｜｜DSML｜｜parameter>\n<｜｜DSML｜｜parameter name=\"goal_id\" string=\"true\">goal_1_e62f9</｜｜DSML｜｜parameter>\n<｜｜DSML｜｜parameter name=\"objective\" string=\"true\">测试目标管理功能：已完成目标创建与状态查询，正在演示目标更新与完成流程，验证目标全生命周期。</｜｜DSML｜｜parameter>\n<｜｜DSML｜｜parameter name=\"revision\" string=\"false\">1</｜｜DSML｜｜parameter>\n</｜｜DSML｜｜invoke>\n</｜｜DSML｜｜tool_calls>"

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		var msg map[string]any
		switch n {
		case 1:
			msg = map[string]any{"role": "assistant", "content": createDSML}
		case 2:
			msg = map[string]any{"role": "assistant", "content": updateDSML}
		default:
			// 第三轮起模拟网络挂起：不响应，直到请求方断开。
			// 先排空请求体：net/http 要等 body 消费完才启动 background
			// read，不排空的话客户端断开不会触发 r.Context().Done()，
			// srv.Close() 会永远等这条 active 连接（收尾挂死的根因）。
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	defer srv.Close()

	st := &config.Settings{Policy: "write-workspace"}
	h, err := Build(Options{
		Workspace: t.TempDir(),
		Client:    llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "t", Model: "test-model"}),
		Settings:  st,
		Host:      &tools.NoopHost{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	root := h.Root

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var final string
	go func() {
		defer close(done)
		final, _ = root.RunAutoGoal(ctx, "帮我测试目标功能")
	}()
	time.Sleep(1500 * time.Millisecond)
	if atomic.LoadInt32(&calls) < 3 {
		t.Fatalf("continuation rounds not reached, calls=%d", calls)
	}
	cancel()
	select {
	case <-done:
		t.Logf("returned cleanly, calls=%d final=%.80s", calls, final)
	case <-time.After(5 * time.Second):
		t.Fatalf("RunAutoGoal did not return after cancel — 卡死复现, calls=%d", calls)
	}
}
