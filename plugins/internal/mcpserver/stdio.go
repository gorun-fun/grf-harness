// stdio.go 本地模式：行分隔 JSON-RPC（MCP stdio 约定）。stdin 关闭即
// 退出（grf-harness 关闭管道/进程退出时插件自然结束）。
package mcpserver

import (
	"bufio"
	"context"
	"io"
	"os"
)

// ServeStdio 从 stdin 读请求、stdout 写应答，直到 stdin 关闭。
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.Serve(ctx, os.Stdin, os.Stdout)
}

// Serve 行循环（测试可注入 reader/writer）。
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 1<<20)
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if resp := s.Dispatch(ctx, line); resp != nil {
				bw.Write(resp)
				bw.WriteByte('\n')
				bw.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
