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
	// cancel 取消执行器 ctx（创建时写定、之后只读，随 job.mu 之外发布）。
	// final 类型作业无执行器，为 nil。
	cancel context.CancelFunc
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
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{id: id, kind: kind, status: JobRunning, descr: descr, start: time.Now(), cancel: cancel}
	j.cond = sync.NewCond(&j.mu)
	m.jobs[id] = j
	m.mu.Unlock()

	go func() {
		defer cancel() // 作业结束后释放 ctx 资源（对已完成的 ctx 重复 cancel 无害）
		err := run(ctx, func(s string) {
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

// Kill 请求取消：先取消执行器 ctx 触发真实停止（bash 侧 SIGINT，
// WaitDelay 后 SIGKILL 兜底），再把作业标记为 killed 并广播。标记先行
// 返回只是让 job_output 的等待立即解除，进程停止由执行器在 cancel 后
// 自行完成并落定终态。
func (m *JobManager) Kill(id, reason string) error {
	j := m.lookup(id)
	if j == nil {
		return fmt.Errorf("unknown job %q", id)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancel != nil {
		j.cancel()
	}
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
// 结束后返回最终结果。wait 为 true 时最多等待 timeout 毫秒直到终态；
// ctx 取消时立即返回 ctx.Err() —— 回合取消必须能打断阻塞中的
// job_output 等待，否则 cancel 端点等不到终态、只能谎报已取消，
// 会话进入"僵尸运行"态（running 恒真），后续请求全部 409。
func (m *JobManager) Output(ctx context.Context, id string, wait bool, timeout time.Duration) (string, error) {
	j := m.lookup(id)
	if j == nil {
		return "", fmt.Errorf("unknown job %q; use job_list to see your jobs", id)
	}
	deadline := time.Now().Add(timeout)
	j.mu.Lock()
	defer j.mu.Unlock()
	if wait {
		// cond 无法直接感知 ctx 取消/超时，统一由后台 goroutine 广播唤醒；
		// stop 关闭（本函数返回）后唤醒器退出，不泄漏。
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			var timer <-chan time.Time
			if timeout > 0 {
				timer = time.After(time.Until(deadline))
			}
			select {
			case <-ctx.Done():
			case <-timer:
			case <-stop:
				return
			}
			j.mu.Lock()
			j.cond.Broadcast()
			j.mu.Unlock()
		}()
		for j.status == JobRunning {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if timeout > 0 && time.Now().After(deadline) {
				break
			}
			j.cond.Wait()
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
		// final 类型作业结果收在 result（buf 恒空），done 且从未被
		// job_output 读过即待收割；stream 作业按增量未读判定。
		// final 是嵌入方扩展点（NewFinal/FinishFinal），内置仅 bash
		// 用 stream。
		unread := j.status == JobDone &&
			((j.kind != "stream" && j.read == 0) || j.buf.Len() > j.read)
		j.mu.Unlock()
		if unread {
			return true
		}
	}
	return false
}

// jobOutputArgs / jobKillArgs job 工具参数（具名类型供 spec 交叉校验
// 测试反射 json tag）。job_list 无参数。
type jobOutputArgs struct {
	JobID     string `json:"job_id"`
	Wait      bool   `json:"wait"`
	TimeoutMS int    `json:"timeout_ms"`
}

type jobKillArgs struct {
	JobID  string `json:"job_id"`
	Reason string `json:"reason"`
}

// RegisterJobTools 注册 job_output / job_kill / job_list。
func RegisterJobTools(r *Registry, m *JobManager) {
	r.Register("job_output", func(ctx context.Context, args string) (Result, error) {
		var a jobOutputArgs
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
		out, err := m.Output(ctx, a.JobID, a.Wait, timeout)
		if err != nil {
			return Result{}, err
		}
		return TextResult(out), nil
	})

	r.Register("job_kill", func(ctx context.Context, args string) (Result, error) {
		var a jobKillArgs
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
