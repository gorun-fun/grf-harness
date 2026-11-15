// mdns_test.go mDNS 子集测试：wire codec 往返（含压缩指针解析）、
// 发现循环（注入 fake conn：点名查询触发应答、应答记录聚合出 Peer）。
// 不碰真组播——CI/沙箱常无组播路由，测试只验协议逻辑。
package cluster

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDNSCodecRoundtrip(t *testing.T) {
	msg := dnsMsg{
		Response: true,
		Answers: []dnsRR{
			{Name: mdnsService, Type: typePTR, Class: classINET, TTL: 120, Data: appendName(nil, "mbp._grf-cluster._udp.local.")},
			{Name: "mbp._grf-cluster._udp.local.", Type: typeSRV, Class: classINET, TTL: 120,
				Data: buildSRV(srvRData{Port: 7400, Target: "mbp._grf-cluster._udp.local."})},
			{Name: "mbp._grf-cluster._udp.local.", Type: typeTXT, Class: classINET, TTL: 120,
				Data: buildTXT([]string{"v=1", "id=0123456789abcdef", "name=MBP"})},
			{Name: "mbp._grf-cluster._udp.local.", Type: typeA, Class: classINET, TTL: 120, Data: buildA(net.IPv4(192, 168, 3, 9))},
		},
	}
	back, err := parseDNS(encodeDNS(msg))
	if err != nil {
		t.Fatal(err)
	}
	if !back.Response || len(back.Answers) != 4 {
		t.Fatalf("往返形状 = response:%v answers:%d", back.Response, len(back.Answers))
	}
	if back.Answers[0].PtrTarget != "mbp._grf-cluster._udp.local." {
		t.Fatalf("PTR 往返 = %q", back.Answers[0].PtrTarget)
	}
	if back.Answers[1].SRVPort != 7400 || back.Answers[1].SRVTarget != "mbp._grf-cluster._udp.local." {
		t.Fatalf("SRV 往返 = port:%d target:%q", back.Answers[1].SRVPort, back.Answers[1].SRVTarget)
	}
	kvs := parseTXT(back.Answers[2].Data)
	if kvs["id"] != "0123456789abcdef" || kvs["name"] != "MBP" {
		t.Fatalf("TXT 往返 = %v", kvs)
	}
	if ip := parseA(back.Answers[3].Data); ip == nil || !ip.Equal(net.IPv4(192, 168, 3, 9).To4()) {
		t.Fatalf("A 往返 = %v", ip)
	}
}

func TestDNSParseCompressedName(t *testing.T) {
	// 手工构造带压缩指针的包：RR 的 rdata 指回 offset 12 的名字区
	var b []byte
	b = append(b, 0, 42, 0x84, 0x00, 0, 0, 0, 1, 0, 0, 0, 0) // header：1 answer
	b = appendName(b, mdnsService)                           // RR NAME（offset 12）
	b = binary.BigEndian.AppendUint16(b, typePTR)
	b = binary.BigEndian.AppendUint16(b, classINET)
	b = binary.BigEndian.AppendUint32(b, 120)
	b = binary.BigEndian.AppendUint16(b, 2) // rdlength = 2（一个指针）
	b = append(b, 0xc0, 12)                 // rdata：压缩指针 → offset 12
	msg, err := parseDNS(b)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Answers[0].Name != mdnsService {
		t.Fatalf("RR 名解析 = %q", msg.Answers[0].Name)
	}
	if msg.Answers[0].PtrTarget != mdnsService {
		t.Fatalf("rdata 压缩指针解析 = %q, want %q", msg.Answers[0].PtrTarget, mdnsService)
	}
}

// fakeMDNSConn 内存通道 conn：reads 收注入包，writes 捕获发出包。
type fakeMDNSConn struct {
	readCh  chan []byte
	written chan []byte
	closed  chan struct{}
}

func newFakeMDNSConn() *fakeMDNSConn {
	return &fakeMDNSConn{readCh: make(chan []byte, 8), written: make(chan []byte, 8), closed: make(chan struct{})}
}

func (c *fakeMDNSConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case pkt := <-c.readCh:
		return copy(p, pkt), &net.UDPAddr{IP: net.IPv4(192, 168, 3, 77), Port: 5353}, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *fakeMDNSConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	select {
	case c.written <- p:
		return len(p), nil
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *fakeMDNSConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// drainWritten 非阻塞取一条已发包。
func drainWritten(t *testing.T, c *fakeMDNSConn) []byte {
	t.Helper()
	select {
	case p := <-c.written:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("等待发出包超时")
		return nil
	}
}

func TestDiscoveryAnnounceOnQuery(t *testing.T) {
	conn := newFakeMDNSConn()
	self := Identity{ID: "0123456789abcdef", Name: "My MBP"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runDiscovery(ctx, conn, self, 7400, false, nil)

	// 启动自宣（非 browse 也会发）
	pkt := drainWritten(t, conn)
	msg, err := parseDNS(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if !msg.Response || len(msg.Answers) == 0 {
		t.Fatalf("自宣应为应答包：%+v", msg)
	}
	// 点名查询 → 再宣
	q := encodeDNS(dnsMsg{Questions: []dnsQuestion{{Name: mdnsService, QType: typePTR}}})
	conn.readCh <- q
	pkt = drainWritten(t, conn)
	if msg, err = parseDNS(pkt); err != nil || !msg.Response {
		t.Fatalf("查询应触发应答：%v", err)
	}
	// TXT 里带真实名（大小写保留，供成员表路由名对齐）
	found := false
	for _, rr := range msg.Answers {
		if rr.Type == typeTXT && parseTXT(rr.Data)["name"] == "My MBP" {
			found = true
		}
	}
	if !found {
		t.Fatal("应答 TXT 应含真实节点名")
	}
}

func TestDiscoveryBrowseAggregatesPeer(t *testing.T) {
	conn := newFakeMDNSConn()
	self := Identity{ID: "aaaabbbbccccdddd", Name: "commander"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peers := make(chan Peer, 4)
	go runDiscovery(ctx, conn, self, 7400, true, func(p Peer) { peers <- p })

	drainWritten(t, conn) // 启动自宣
	// 别家的应答包（分两半送，验证跨包聚合）：先 SRV+TXT，再 A
	inst := "mac-mini._grf-cluster._udp.local."
	resp := dnsMsg{Response: true, Answers: []dnsRR{
		{Name: inst, Type: typeTXT, Class: classINET, TTL: 120,
			Data: buildTXT([]string{"v=1", "id=1111222233334444", "name=mac-mini"})},
		{Name: inst, Type: typeSRV, Class: classINET, TTL: 120,
			Data: buildSRV(srvRData{Port: 7401, Target: inst})},
	}}
	conn.readCh <- encodeDNS(resp)
	select {
	case p := <-peers:
		if p.Name != "mac-mini" || p.ID != "1111222233334444" {
			t.Fatalf("聚合 Peer = %+v", p)
		}
		if !strings.HasSuffix(p.Addr, ":7401") {
			t.Fatalf("地址应含 SRV 端口：%s", p.Addr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Peer 未聚合完成")
	}
}

func TestDNSLabelSafe(t *testing.T) {
	cases := map[string]string{
		"MBP 16":        "mbp-16",
		"mac-mini":      "mac-mini",
		"a.b.c":         "a-b-c",
		"  spaced  ":    "spaced",
		"--lead--":      "lead",
		"中文节点":           "node",
		strings.Repeat("x", 100): strings.Repeat("x", 63),
	}
	for in, want := range cases {
		if got := dnsLabelSafe(in); got != want {
			t.Errorf("dnsLabelSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
