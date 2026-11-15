// cluster_tasks.go 节点侧集群指令执行（M3b 中继下行）与落定回投（M5b）。
// InboxLoop 收到指令后薄委托 ExecuteClusterCommand：task 建会话/续聊并跑
// 后台回合、resume 在暂停会话上提交应答、cancel 取消在跑回合；一切终态
// 经 postClusterSettle 回投指挥官 /cluster/events（中继场景是唯一结果
// 通道；直连场景回投只是轮询的提前量）。直连派发的 HTTP 后台回合同样
// 回投（runBackground 尾部）——指挥官 runSegment 收到即跳过等待。
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"log"
	"strings"
	"time"

	"git.gorun.fun/share/grf-harness/agent"
	"git.gorun.fun/share/grf-harness/cluster"
)

// clusterTask 一个中继任务的节点侧登记（taskID = 指挥官侧远端句柄 id）：
// 续聊/取消/回投都经它定位会话与链尾。
type clusterTask struct {
	sid    string
	respID string // 最新 response id（回投关联键）
}

// ExecuteClusterCommand 执行一条收件箱指令（server_cmd 把 InboxLoop 的
// 回调接到这里）。同步返回——InboxLoop 串行投递，节点按序执行；回合本体
// 在后台 goroutine 跑，终态经事件回投，不阻塞通道。
func (s *Server) ExecuteClusterCommand(cmd cluster.InboxCommand) {
	switch cmd.Type {
	case cluster.InboxTask:
		s.execInboxTask(cmd)
	case cluster.InboxResume:
		s.execInboxResume(cmd)
	case cluster.InboxCancel:
		s.execInboxCancel(cmd)
	}
}

// execInboxTask 建会话/续聊并跑后台回合。
func (s *Server) execInboxTask(cmd cluster.InboxCommand) {
	if cmd.TaskID == "" || strings.TrimSpace(cmd.Input) == "" {
		s.postClusterSettle(&ResponseResource{
			Status: statusFailed, Error: &ErrorPayload{Message: "malformed task command"},
		}, cmd.TaskID)
		return
	}
	if reg := s.lookupClusterTask(cmd.TaskID); reg != nil {
		// 续聊：定位既有会话，按链尾续跑（会话在跑则回投 busy 失败——
		// 指挥官只在落定后续投，冲突即指令乱序）
		sess, err := s.getSession(reg.sid)
		if err != nil {
			s.postClusterSettle(&ResponseResource{Status: statusFailed,
				Error: &ErrorPayload{Message: "session vanished: " + err.Error()}}, cmd.TaskID)
			return
		}
		s.startClusterTurn(sess, cmd.TaskID, cmd.Input, nil)
		return
	}
	// 新任务：内联种子落本节点共享区（unpackSeed 在 newSession 内解包）
	var sess *session
	cfg := &grfSessionConfig{
		Policy:      cmd.Policy,
		HostContext: cmd.HostCtx,
		ReturnFiles: cmd.ReturnFiles,
	}
	if cmd.SeedB64 != "" {
		raw, err := base64.StdEncoding.DecodeString(cmd.SeedB64)
		if err != nil {
			s.postClusterSettle(&ResponseResource{Status: statusFailed,
				Error: &ErrorPayload{Message: "decode inline seed: " + err.Error()}}, cmd.TaskID)
			return
		}
		meta, err := s.clusterShare().AddFromReader(bytes.NewReader(raw))
		if err != nil {
			s.postClusterSettle(&ResponseResource{Status: statusFailed,
				Error: &ErrorPayload{Message: "store inline seed: " + err.Error()}}, cmd.TaskID)
			return
		}
		cfg.SeedRef = meta.Ref
	}
	var err error
	sess, err = s.newSession(cfg)
	if err != nil {
		s.postClusterSettle(&ResponseResource{Status: statusFailed,
			Error: &ErrorPayload{Message: "create session: " + err.Error()}}, cmd.TaskID)
		return
	}
	s.startClusterTurn(sess, cmd.TaskID, cmd.Input, nil)
}

// execInboxResume 暂停应答：在暂停会话上提交 items 续跑（等价 ORS 的
// grf:input_result input）。
func (s *Server) execInboxResume(cmd cluster.InboxCommand) {
	reg := s.lookupClusterTask(cmd.TaskID)
	if reg == nil {
		s.postClusterSettle(&ResponseResource{Status: statusFailed,
			Error: &ErrorPayload{Message: "resume for unknown task"}}, cmd.TaskID)
		return
	}
	sess, err := s.getSession(reg.sid)
	if err != nil {
		s.postClusterSettle(&ResponseResource{Status: statusFailed,
			Error: &ErrorPayload{Message: "session vanished: " + err.Error()}}, cmd.TaskID)
		return
	}
	if !sess.root.HasPausedCalls() {
		s.postClusterSettle(&ResponseResource{Status: statusFailed,
			Error: &ErrorPayload{Message: "session is not paused; resume items rejected"}}, cmd.TaskID)
		return
	}
	var resume []agent.ResumeInput
	for _, it := range cmd.Items {
		resume = append(resume, agent.ResumeInput{
			CallID: it.CallID, Answers: it.Answers, Approved: it.Approved,
			Feedback: it.Feedback, Dismiss: it.Dismiss,
		})
	}
	s.startClusterTurn(sess, cmd.TaskID, "", resume)
}

// execInboxCancel 取消在跑回合（幂等；终态回投由被取消回合的收尾发出）。
func (s *Server) execInboxCancel(cmd cluster.InboxCommand) {
	reg := s.lookupClusterTask(cmd.TaskID)
	if reg == nil {
		return // 未知任务的取消：安静丢弃（幂等语义）
	}
	if sess, err := s.getSession(reg.sid); err == nil {
		sess.cancelTurn()
	}
}

// startClusterTurn 占用回合槽并后台执行一个集群回合（text 新输入 /
// resume 暂停应答，二者互斥）。终态统一经 postClusterSettle 回投。
func (s *Server) startClusterTurn(sess *session, taskID, text string, resume []agent.ResumeInput) {
	if sess.root.IsRunning() {
		s.postClusterSettle(&ResponseResource{Status: statusFailed,
			Error: &ErrorPayload{Message: "session busy: a turn is already running"}}, taskID)
		return
	}
	claim, ok := sess.tryClaim()
	if !ok {
		s.postClusterSettle(&ResponseResource{Status: statusFailed,
			Error: &ErrorPayload{Message: "session busy: a turn is already running"}}, taskID)
		return
	}
	respID := sess.nextResp()
	s.registerClusterTask(taskID, sess.meta.ID, respID)
	req := &createRequest{Background: true}
	go func() {
		res, execErr := s.execute(context.Background(), sess, req, respID, claim, text, nil, resume, true, nil, nil)
		if execErr != nil {
			now := time.Now().Unix()
			res = &ResponseResource{ID: respID, Status: statusFailed, CompletedAt: &now,
				Error: &ErrorPayload{Type: errInvalid, Message: execErr.Error()}}
		}
		s.registerClusterTask(taskID, sess.meta.ID, respID)
		s.postClusterSettle(res, taskID)
	}()
}

// lookupClusterTask / registerClusterTask 中继任务登记表。
func (s *Server) lookupClusterTask(taskID string) *clusterTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.clusterTasks[taskID]; ok {
		c := *t
		return &c
	}
	return nil
}

func (s *Server) registerClusterTask(taskID, sid, respID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clusterTasks == nil {
		s.clusterTasks = map[string]*clusterTask{}
	}
	s.clusterTasks[taskID] = &clusterTask{sid: sid, respID: respID}
}

// clusterShare 本节点共享对象存储（集群未启用返回 nil——调用方先判空）。
func (s *Server) clusterShare() *cluster.ShareStore {
	if s.cluster == nil {
		return nil
	}
	return s.cluster.Share()
}

// clusterSelfName 事件回投用的本节点名（settings 显式名 > cluster.json/
// hostname，与注册/心跳同名）。
func (s *Server) clusterSelfName() string {
	id, err := cluster.LoadIdentity()
	if err != nil {
		return "node"
	}
	name := id.Name
	if s.clusterCfg != nil && s.clusterCfg.Name != "" {
		name = s.clusterCfg.Name
	}
	return name
}

// postClusterSettle 回投一个回合终态事件到指挥官 /cluster/events。
// 直连派发（HTTP 后台回合）与中继回合共用；taskID 空表示直连（指挥官按
// response_id 匹配唤醒）。尽力而为：失败记日志不重试（直连有轮询兜底；
// 中继失败即失联，由指挥官的掉线收敛路径兜底）。
func (s *Server) postClusterSettle(res *ResponseResource, taskID string) {
	if s.clusterCfg == nil || s.clusterCfg.CommanderURL == "" {
		return
	}
	ev := cluster.InboxEvent{
		Type:       "turn.settled",
		Node:       s.clusterSelfName(),
		TaskID:     taskID,
		ResponseID: res.ID,
		Status:     res.Status,
		FinalText:  finalTextOf(res),
		Artifacts:  res.GrfArtifacts,
	}
	if res.Error != nil {
		ev.Error = res.Error.Message
	}
	if d := res.IncompleteDetails; d != nil {
		ev.Reason = d.Reason
		for _, rq := range d.Requests {
			ev.Requests = append(ev.Requests, cluster.IncompleteRequest{
				CallID: rq.CallID, Name: rq.Name, Kind: rq.Kind, Arguments: rq.Arguments,
			})
		}
	}
	c := cluster.NewNodeClient(s.clusterCfg.CommanderURL, s.clusterCfg.Token)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.PostSettle(ctx, ev); err != nil {
		log.Printf("cluster settle 回投失败(%s): %v", res.ID, err)
	}
}

// finalTextOf 提取终态资源的 assistant 最终文本（output_text 拼接；与
// cluster 包 Response.FinalText 同语义，server 侧资源形状独立实现一份）。
func finalTextOf(r *ResponseResource) string {
	var texts []string
	for _, it := range r.Output {
		if it.Type != itemMessage || it.Role != "assistant" {
			continue
		}
		for _, p := range it.Content {
			if p.Type == "output_text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// settleHookBackground 直连后台回合落定后的集群回投钩子（runBackground
// 调用）：会话带宿主简报（集群派发的标记）且配置了 commander_url 才发。
func (s *Server) settleHookBackground(sess *session, res *ResponseResource) {
	if s.clusterCfg == nil || s.clusterCfg.CommanderURL == "" || sess.meta.HostContext == "" {
		return
	}
	go s.postClusterSettle(res, "")
}
