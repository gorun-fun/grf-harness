// policy_test.go set_policy 工具：用户选择预制规则生效、保持现状/
// 自定义输入为拒绝、缺 reason 与无宿主拒绝。模型侧无 policy 参数可传。
package tools

import (
	"context"
	"strings"
	"testing"
)

// policyHost 按 preset 应答 AskUser。
type policyHost struct {
	NoopHost
	answer string
	err    error
}

func (h *policyHost) AskUser(ctx context.Context, qs []Question) (map[string]any, error) {
	if h.err != nil {
		return nil, h.err
	}
	return map[string]any{"set_policy": h.answer}, nil
}

func runSetPolicy(t *testing.T, host Host, reason string) (string, string) {
	t.Helper()
	r := NewRegistry()
	applied := ""
	RegisterSetPolicy(r, func() Host { return host }, func(p string) { applied = p })
	res := r.Execute(context.Background(), "set_policy",
		mustJSON(t, map[string]any{"reason": reason}))
	return res.Text, applied
}

func TestSetPolicyUserPicksPreset(t *testing.T) {
	for _, o := range SetPolicyOptions {
		text, applied := runSetPolicy(t, &policyHost{answer: o.Label}, "任务需要变更策略")
		if applied != o.Policy {
			t.Fatalf("user pick %q should apply %q, got %q", o.Label, o.Policy, applied)
		}
		if !strings.Contains(text, o.Policy) {
			t.Fatalf("result should state the applied policy, got: %s", text)
		}
	}
}

func TestSetPolicyKeepCurrentDeclines(t *testing.T) {
	for _, ans := range []string{setPolicyKeepOpt, "", "自定义不要改"} {
		text, applied := runSetPolicy(t, &policyHost{answer: ans}, "理由")
		if applied != "" {
			t.Fatalf("answer %q must not apply a policy, got %q", ans, applied)
		}
		if !strings.Contains(text, "declined") {
			t.Fatalf("expected decline text, got: %s", text)
		}
	}
}

func TestSetPolicyRequiresReason(t *testing.T) {
	text, applied := runSetPolicy(t, &policyHost{answer: SetPolicyOptions[1].Label}, "  ")
	if applied != "" || !strings.Contains(text, "reason is required") {
		t.Fatalf("expected reason-required error, got: %s (applied %q)", text, applied)
	}
}

func TestSetPolicyNoHost(t *testing.T) {
	r := NewRegistry()
	RegisterSetPolicy(r, func() Host { return nil }, func(string) {})
	res := r.Execute(context.Background(), "set_policy", mustJSON(t, map[string]any{"reason": "x"}))
	if !strings.Contains(res.Text, "no interactive user") {
		t.Fatalf("expected no-host rejection, got: %s", res.Text)
	}
}
