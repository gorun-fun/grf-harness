// chat_test.go 流式分段（思考/正文按模型输出顺序固化）单测。
package tui

import (
	"strings"
	"testing"
)

func TestHubSealOrder(t *testing.T) {
	h := newHub()
	h.begin()
	h.reasoning("想一下")
	h.content("第一段正文")
	h.reasoning("再想想")
	h.content("第二段正文")

	blocks := h.seal()
	if len(blocks) != 4 {
		t.Fatalf("期望 4 块（思考/正文交替），得到 %d", len(blocks))
	}
	kinds := []blockKind{blocks[0].kind, blocks[1].kind, blocks[2].kind, blocks[3].kind}
	want := []blockKind{blkThink, blkText, blkThink, blkText}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("第 %d 块类型 = %v，期望 %v", i, kinds[i], want[i])
		}
	}
	if !strings.Contains(blocks[1].text, "第一段正文") || !strings.Contains(blocks[3].text, "第二段正文") {
		t.Fatalf("正文块内容不对: %q / %q", blocks[1].text, blocks[3].text)
	}
	if !strings.Contains(blocks[2].think, "再想想") {
		t.Fatalf("思考块内容不对: %q", blocks[2].think)
	}
	// seal 后分段清空，再次 seal 为空
	if again := h.seal(); len(again) != 0 {
		t.Fatalf("seal 后应无残留分段，得到 %d 块", len(again))
	}
}

func TestHubSegViewsLive(t *testing.T) {
	h := newHub()
	h.begin()
	h.reasoning("a")
	views := h.segViews()
	if len(views) != 1 || !views[0].think || !views[0].live {
		t.Fatalf("思考分段应为 live: %+v", views)
	}
	h.content("b")
	views = h.segViews()
	if len(views) != 2 || views[0].live || !views[1].live {
		t.Fatalf("切到正文后思考段应固定、正文段 live: %+v", views)
	}
}
