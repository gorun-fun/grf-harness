// jobs.go 后台作业管理器：bash run_in_background 启动的命令立即返回
// job id；job_output 增量读取（流式作业只返回上次读取之后的输出，可
// wait 阻塞等待）、job_kill 取消、job_list 列表。每个响应以
// [status: ...] 结尾。
package tools

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// JobStatus 作业状态。
const (
	JobRunning = "running"
	JobDone    = "done"
	JobKilled  = "killed"
)

type job struct {
	id     string
	kind   string // "stream"（bash 后台）| "final"（一次性结果）
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	status string
	err    error
	result string // final 类型的最终结果
	read   int    // 上次 job_output 读到的位置
	descr  string // 描述（UI 展示）
	start  time.Time
}

// JobManager 后台作业表。
type JobManager struct {
	mu   sync.Mutex
	jobs map[string]*job
	seq  int
}

func NewJobManager() *JobManager {
	return &JobManager{jobs: map[string]*job{}}
}

// StartStream 启动一个流式后台作业：run 在新 goroutine 中执行，通过
// 返回的函数追加输出。返回 job id。
func (m *JobManager) StartStream(kind, descr string, run func(ctx context.Context, emit func(string)) error) string {
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("job_%d", m.seq)
	j := &job{id: id, kind: kind, status: JobRunning, descr: descr, start: time.Now()}
	j.cond = sync.NewCond(&j.mu)
	m.jobs[id] = j
	m.mu.Unlock()

	go func() {
		err := run(context.Background(), func(s string) {
			j.mu.Lock()
			j.buf.WriteString(s)
			j.cond.Broadcast()
			j.mu.Unlock()
		})
		j.mu.Lock()
		j.err = err
		if j.status == JobRunning {
			if ctxErr(j) {
				j.status = JobKilled
			} else {
				j.status = JobDone
			}
		}
		j.cond.Broadcast()
		j.mu.Unlock()
	}()
	return id
}

func ctxErr(j *job) bool { return strings.Contains(fmt.Sprint(j.err), "context canceled") }

// Kill 请求取消：把作业标记为 killed 并广播（实际停止由执行器监听 ctx）。
func (m *JobManager) Kill(id, reason string) error {
	j := m.lookup(id)
	if j == nil {
		return fmt.Errorf("unknown job %q", id)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status == JobRunning {
		j.status = JobKilled
		if reason != "" && j.buf.Len() > 0 {
			j.buf.WriteString("\n")
		}
		if reason != "" {
			j.buf.WriteString("[killed: " + reason + "]\n")
		}
	}
	j.cond.Broadcast()
	return nil
}

func (m *JobManager) lookup(id string) *job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// Output 读取作业：流式作业返回自上次读取以来的新输出；final 作业在
// 结束后返回最终结果。wait 为 true 时最多等待 timeout 毫秒直到终态。
// 返回 (文本, status, error)。文本末尾附 [status: ...]。
func (m *JobManager) Output(id string, wait bool, timeout time.Duration) (string, error) {
	j := m.lookup(id)
	if j == nil {
		return "", fmt.Errorf("unknown job %q; use job_list to see your jobs", id)
	}
	deadline := time.Now().Add(timeout)
	j.mu.Lock()
	defer j.mu.Unlock()
	if wait {
		for j.status == JobRunning {
			if timeout > 0 {
				// 带超时的等待：cond 无法直接超时，用后台唤醒
				go func() {
					time.Sleep(time.Until(deadline))
					j.cond.Broadcast()
				}()
			}
			j.cond.Wait()
			if j.status == JobRunning && timeout > 0 && time.Now().After(deadline) {
				break
			}
		}
	}
	var out string
	switch j.kind {
	case "stream":
		data := j.buf.Bytes()
		if j.read < len(data) {
			out = string(data[j.read:])
			j.read = len(data)
		}
	default:
		if j.status != JobRunning {
			out = j.result
		}
	}
	if j.err != nil && j.status != JobKilled {
		out += fmt.Sprintf("\n[error: %v]", j.err)
	}
	out = strings.TrimRight(out, "\n")
	out += fmt.Sprintf("\n[status: %s]", j.status)
	return out, nil
}

// FinishFinal 结束一个 final 类型作业（子代理等一次性结果复用）。
func (m *JobManager) FinishFinal(id string, result string, err error) {
	j := m.lookup(id)
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.result = result
	j.err = err
	j.status = JobDone
	j.cond.Broadcast()
}

// NewFinal 创建一个 final 类型的挂起作业（返回 id）。
func (m *JobManager) NewFinal(kind, descr string) string {
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("job_%d", m.seq)
	j := &job{id: id, kind: kind, status: JobRunning, descr: descr, start: time.Now()}
	j.cond = sync.NewCond(&j.mu)
	m.jobs[id] = j
	m.mu.Unlock()
	return id
}

// List 文本列出全部作业（id/kind/状态/描述）。
func (m *JobManager) List() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.jobs) == 0 {
		return "no background jobs"
	}
	ids := make([]string, 0, len(m.jobs))
	for id := range m.jobs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, k int) bool {
		return m.jobs[ids[i]].start.Before(m.jobs[ids[k]].start)
	})
	var b strings.Builder
	for _, id := range ids {
		j := m.jobs[id]
		descr := j.descr
		if descr == "" {
			descr = "-"
		}
		fmt.Fprintf(&b, "%s  %-7s %-8s %s\n", id, j.kind, j.status, descr)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ActiveCount 仍在运行的作业数。
func (m *JobManager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, j := range m.jobs {
		j.mu.Lock()
		if j.status == JobRunning {
			n++
		}
		j.mu.Unlock()
	}
	return n
}

// NeedsAttention 是否存在已结束（done）但输出从未被读取过的作业——
// 只有这类作业的结果会丢失，值得提醒模型收割；仍在运行的作业
// （如 dev server）是模型知情且刻意保留的，不打扰。
func (m *JobManager) NeedsAttention() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		j.mu.Lock()
		unread := j.status == JobDone && j.buf.Len() > j.read
		j.mu.Unlock()
		if unread {
			return true
		}
	}
	return false
}

// RegisterJobTools 注册 job_output / job_kill / job_list。
func RegisterJobTools(r *Registry, m *JobManager) {
	r.Register("job_output", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			JobID     string `json:"job_id"`
			Wait      bool   `json:"wait"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		timeout := time.Duration(a.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		if timeout > 10*time.Minute {
			timeout = 10 * time.Minute
		}
		out, err := m.Output(a.JobID, a.Wait, timeout)
		if err != nil {
			return Result{}, err
		}
		return TextResult(out), nil
	})

	r.Register("job_kill", func(ctx context.Context, args string) (Result, error) {
		var a struct {
			JobID  string `json:"job_id"`
			Reason string `json:"reason"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if err := m.Kill(a.JobID, a.Reason); err != nil {
			return Result{}, err
		}
		return TextResult(fmt.Sprintf("kill request accepted for %s; it settles as killed once it actually stops", a.JobID)), nil
	})

	r.Register("job_list", func(ctx context.Context, args string) (Result, error) {
		return TextResult(m.List()), nil
	})
}
