// stats.go 会话级 LLM 用量统计：轮数、LLM 总耗时、首 token 延迟、
// token 用量（服务端流里带 usage 时精确累加，否则按字符启发式估算）。
package llm

import (
	"sync"
	"time"
	"unicode"
)

// usageInfo OpenAI 兼容 usage 字段。
type usageInfo struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// StatsSnapshot 统计只读快照（UI 展示用）。
type StatsSnapshot struct {
	Rounds           int
	LLMTime          time.Duration
	FirstTokenAvg    time.Duration
	GenerationTime   time.Duration // LLMTime - 首token总和（生成耗时）
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	UsageSeen        bool // 服务端是否返回过 usage
	CachePct         int  // 缓存命中率 0-100；<0 表示未知
	// LastPromptTokens/LastCompletionTokens 最后一次 LLM 调用的服务端
	// 精确 usage（prompt 为该次全量上下文，含缓存命中部分）。累计值
	// （PromptTokens 等）把回合内每次工具循环调用的全量 prompt 都加一遍，
	// 不能当上下文长度用；上下文占用展示一律取 Last*（无 usage 回报时为 0）。
	LastPromptTokens     int64
	LastCompletionTokens int64
}

type stats struct {
	mu            sync.Mutex
	rounds        int
	llmTime       time.Duration
	firstTotal    time.Duration
	firstN        int
	promptTok     int64 // usage 精确值 + 无 usage 轮次的估算
	promptUsage   int64 // 仅 usage 精确值（算缓存命中率）
	cachedTok     int64
	completionTok int64
	usageSeen     bool
	lastPromptTok int64 // 最后一次 usage 精确回报的 prompt（=该次上下文长度）
	lastComplTok  int64
}

// estimateTokens 字符启发式：CJK 字按 1 token，其余约 4 字符 1 token。
func estimateTokens(s string) int64 {
	cjk, other := 0, 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			cjk++
		} else if r != ' ' {
			other++
		}
	}
	return int64(cjk) + int64(other)/4
}

func estimateMessagesTokens(messages []Message) int64 {
	var n int64
	for _, m := range messages {
		n += estimateTokens(m.Content)
		for _, c := range m.ToolCalls {
			n += estimateTokens(c.Function.Name) + estimateTokens(c.Function.Arguments)
		}
	}
	return n
}

// EstimateMessagesTokens 消息 token 估算（导出版，供上层复用——agent 的
// 自动压缩阈值判定；口径与本包统计一致：CJK 每字 1 token，其余约 4
// 字符 1 token）。
func EstimateMessagesTokens(messages []Message) int64 { return estimateMessagesTokens(messages) }

func (s *stats) addRound(dur, first time.Duration, u *usageInfo, promptEst, outEst int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rounds++
	s.llmTime += dur
	s.firstTotal += first
	s.firstN++
	if u != nil && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		// 服务端给了精确 usage：不用估算值
		s.usageSeen = true
		s.promptTok += u.PromptTokens
		s.promptUsage += u.PromptTokens
		s.completionTok += u.CompletionTokens
		s.lastPromptTok = u.PromptTokens
		s.lastComplTok = u.CompletionTokens
		if u.PromptTokensDetails != nil {
			s.cachedTok += u.PromptTokensDetails.CachedTokens
		}
	} else {
		s.completionTok += outEst
		s.promptTok += promptEst
	}
}

// TokPerSec 生成吞吐（输出 token / 生成耗时）；无数据返回 0。
func (s StatsSnapshot) TokPerSec() int {
	secs := s.GenerationTime.Seconds()
	if secs <= 0 || s.CompletionTokens <= 0 {
		return 0
	}
	return int(float64(s.CompletionTokens) / secs)
}

func (s *stats) snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := StatsSnapshot{
		Rounds:               s.rounds,
		LLMTime:              s.llmTime,
		PromptTokens:         s.promptTok,
		CompletionTokens:     s.completionTok,
		CachedTokens:         s.cachedTok,
		UsageSeen:            s.usageSeen,
		LastPromptTokens:     s.lastPromptTok,
		LastCompletionTokens: s.lastComplTok,
		CachePct:             -1,
	}
	if s.firstN > 0 {
		snap.FirstTokenAvg = s.firstTotal / time.Duration(s.firstN)
	}
	snap.GenerationTime = s.llmTime - s.firstTotal
	if snap.GenerationTime < 0 {
		snap.GenerationTime = 0
	}
	if s.usageSeen && s.promptUsage > 0 {
		snap.CachePct = int(100 * s.cachedTok / s.promptUsage)
	}
	return snap
}
