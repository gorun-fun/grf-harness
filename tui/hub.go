// hub.go streamHub 流式汇聚：后台 agent 的流式增量（思考/正文按模型
// 输出顺序分段）在锁保护下缓存，notify 通知事件循环重绘；工具事件到达
// 或回合结束时由 seal 把已完成分段固化成内容块。
package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// hubSeg 流式分段：模型回复中按输出顺序交替出现的思考段/正文段。
type hubSeg struct {
	think bool
	sb    strings.Builder
	chars int
	start time.Time
	end   time.Time
}

// segView 分段只读快照（事件循环 goroutine 渲染用）。
type segView struct {
	think bool
	text  string
	chars int
	secs  int
	live  bool // 仍在接收增量
}

// streamHub 汇聚后台 agent 的流式状态（锁保护），notify 通知重绘。
// 思考与正文按模型实际输出顺序存为分段，工具事件到达时（applyEvent，
// chat.go）由事件循环把已完成分段固化成块，保证 思考/工具/正文 分开展示。
type streamHub struct {
	mu     sync.Mutex
	segs   []hubSeg
	notify chan struct{}
}

func newHub() *streamHub { return &streamHub{notify: make(chan struct{}, 1)} }

func (h *streamHub) begin() {
	h.mu.Lock()
	h.segs = nil
	h.mu.Unlock()
}

func (h *streamHub) ping() {
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

// append 写入增量；模式切换时开新分段并给上一段记结束时间。
func (h *streamHub) append(d string, think bool) {
	h.mu.Lock()
	now := time.Now()
	n := len(h.segs)
	if n == 0 || h.segs[n-1].think != think {
		if n > 0 && h.segs[n-1].end.IsZero() {
			h.segs[n-1].end = now
		}
		h.segs = append(h.segs, hubSeg{think: think, start: now})
		n++
	}
	s := &h.segs[n-1]
	s.chars += len([]rune(d))
	s.sb.WriteString(d)
	h.mu.Unlock()
	h.ping()
}

func (h *streamHub) content(d string)   { h.append(d, false) }
func (h *streamHub) reasoning(d string) { h.append(d, true) }

// segViews 分段快照（live 段按当前时间算秒数）。
func (h *streamHub) segViews() []segView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]segView, 0, len(h.segs))
	for i, s := range h.segs {
		end := s.end
		live := end.IsZero()
		if live {
			end = time.Now()
		}
		out = append(out, segView{
			think: s.think, text: s.sb.String(), chars: s.chars,
			secs: int(end.Sub(s.start).Seconds()), live: live || i == len(h.segs)-1,
		})
	}
	return out
}

// seal 取走全部分段并转成块（须在事件循环 goroutine 调用）。
func (h *streamHub) seal() []block {
	h.mu.Lock()
	segs := h.segs
	h.segs = nil
	h.mu.Unlock()
	now := time.Now()
	var out []block
	for _, s := range segs {
		end := s.end
		if end.IsZero() {
			end = now
		}
		if s.think {
			if s.chars == 0 {
				continue
			}
			out = append(out, block{
				kind:  blkThink,
				text:  fmt.Sprintf("💭 思考 %ds，%d 字（Ctrl+O 展开）", int(end.Sub(s.start).Seconds()), s.chars),
				think: s.sb.String(),
			})
		} else if s.sb.Len() > 0 {
			out = append(out, block{kind: blkText, text: s.sb.String()})
		}
	}
	return out
}
