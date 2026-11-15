// mdns.go mDNS 子集服务发现（settings cluster.discover = "lan"）：
// 服务类型 _grf-cluster._udp.local.，实例名 = 净化后的节点名。本机既应答
//（收到点名 PTR/SRV/TXT/A 查询即单播回包），也可浏览（指挥官周期广播
// PTR 查询、收集应答里 SRV 端口 + A 地址 + TXT 身份 → 成员表 Announce）。
// 零依赖合规：不引外部 mDNS 库，只实现本场景子集；多点播 socket 收发
// 共用一个 goroutine。发现到的节点"可见"——要派发仍需静态 token 条目
//（直连）或收件箱中继（call-home 节点），与 call-home 成员同规则。
package cluster

import (
	"context"
	"net"
	"sync"
	"time"
)

// mDNS 组播常量（RFC 6762）。
const (
	mdnsGroup      = "224.0.0.251"
	mdnsPort       = 5353
	mdnsService    = "_grf-cluster._udp.local."
	mdnsMaxPacket  = 1500 // 与 mDNS 建议的链路本地 MTU 对齐
	mdnsAnnounceTTL = 120 // 秒；应答/自宣记录 TTL
)

// Peer 一个发现的节点（实例身份 + 可达地址）。
type Peer struct {
	Name string // TXT 里的真实节点名（路由名）
	ID   string // 16 hex 身份
	Addr string // host:port（SRV 端口 + A 地址）
}

// mdnsConn 收发包抽象（真实现 = UDP 组播 socket；测试注入内存通道）。
type mdnsConn interface {
	ReadFrom(p []byte) (int, net.Addr, error)
	WriteTo(p []byte, addr net.Addr) (int, error)
	Close() error
}

// realMDNSConn 真实 UDP 组播连接：加入组播组、设复用（多实例共存）。
type realMDNSConn struct {
	pc *net.UDPConn
}

func newRealMDNSConn() (*realMDNSConn, error) {
	pc, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort})
	if err != nil {
		return nil, err
	}
	return &realMDNSConn{pc: pc}, nil
}

func (c *realMDNSConn) ReadFrom(p []byte) (int, net.Addr, error) { return c.pc.ReadFrom(p) }
func (c *realMDNSConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	// 应答一律发组播组（简化：查询方在组里必然收得到；比单播回源少一个
	// "源端口不可达"的边角）
	return c.pc.WriteTo(p, &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort})
}
func (c *realMDNSConn) Close() error { return c.pc.Close() }

// selfRecords 本机服务记录（应答与自宣共用）。
func selfRecords(self Identity, port int, ips []net.IP) []dnsRR {
	inst := dnsLabelSafe(self.Name) + "." + mdnsService
	var rr []dnsRR
	rr = append(rr, dnsRR{Name: mdnsService, Type: typePTR, Class: classINET, TTL: mdnsAnnounceTTL,
		Data: appendName(nil, inst)})
	rr = append(rr, dnsRR{Name: inst, Type: typeSRV, Class: classINET, TTL: mdnsAnnounceTTL,
		Data: buildSRV(srvRData{Port: uint16(port), Target: inst})})
	rr = append(rr, dnsRR{Name: inst, Type: typeTXT, Class: classINET, TTL: mdnsAnnounceTTL,
		Data: buildTXT([]string{"v=1", "id=" + self.ID, "name=" + self.Name})})
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			rr = append(rr, dnsRR{Name: inst, Type: typeA, Class: classINET, TTL: mdnsAnnounceTTL, Data: buildA(v4)})
		}
	}
	return rr
}

// localIPv4s 本机非环回 IPv4 地址（A 记录用；失败容忍——无 A 也能靠
// 包源地址发现）。
func localIPv4s() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if v4 := ipn.IP.To4(); v4 != nil && !v4.IsLoopback() {
					out = append(out, v4)
				}
			}
		}
	}
	return out
}

// browseState 浏览侧的实例聚合（多包应答拼一个完整视图）。
type browseState struct {
	mu    sync.Mutex
	peers map[string]*Peer // 键 = 实例名（净化名）；值逐步补全
}

// feed 吃进一条应答记录，凑齐 Name+Addr 即回调（每实例只回调一次，
// 之后刷新字段不重报——成员表 Announce 幂等，重报无害但省点噪音）。
// 名字类载荷（PTR/SRV）用 parseDNS 预解的伴生字段（rdata 压缩指针只有
// 对整包可解）。
func (bs *browseState) feed(rr dnsRR, src net.Addr) (complete *Peer) {
	switch rr.Type {
	case typePTR:
		if rr.PtrTarget == "" {
			return nil
		}
		bs.mu.Lock()
		if _, ok := bs.peers[rr.PtrTarget]; !ok {
			bs.peers[rr.PtrTarget] = &Peer{}
		}
		bs.mu.Unlock()
		return nil
	case typeTXT:
		kvs := parseTXT(rr.Data)
		bs.mu.Lock()
		p := bs.peers[rr.Name]
		if p == nil {
			p = &Peer{}
			bs.peers[rr.Name] = p
		}
		if v, ok := kvs["name"]; ok && v != "" {
			p.Name = v
		}
		if v, ok := kvs["id"]; ok {
			p.ID = v
		}
		complete = bs.completeLocked(rr.Name)
		bs.mu.Unlock()
		return complete
	case typeSRV:
		if rr.SRVPort == 0 {
			return nil
		}
		bs.mu.Lock()
		p := bs.peers[rr.Name]
		if p == nil {
			p = &Peer{}
			bs.peers[rr.Name] = p
		}
		// 地址缺 A 时用包源地址兜底（SRV 端口 + 源 IP）
		host := ""
		if src != nil {
			if h, _, err := net.SplitHostPort(src.String()); err == nil {
				host = h
			}
		}
		if p.Addr == "" && host != "" {
			p.Addr = net.JoinHostPort(host, itoa(int(rr.SRVPort)))
		} else if p.Addr != "" && rr.SRVPort != 0 {
			// 已有地址（源 IP 兜底或 A 记录）补/校端口
			if host2, port, err := net.SplitHostPort(p.Addr); err == nil && port == "0" {
				p.Addr = net.JoinHostPort(host2, itoa(int(rr.SRVPort)))
			}
		}
		complete = bs.completeLocked(rr.Name)
		bs.mu.Unlock()
		return complete
	case typeA:
		ip := parseA(rr.Data)
		if ip == nil {
			return nil
		}
		bs.mu.Lock()
		p := bs.peers[rr.Name]
		if p == nil {
			p = &Peer{}
			bs.peers[rr.Name] = p
		}
		if p.Addr == "" { // 已有地址不覆盖（先到先得，避免抖动）
			p.Addr = ip.String() + ":0" // 端口未知占位，SRV 到达后补
		}
		complete = bs.completeLocked(rr.Name)
		bs.mu.Unlock()
		return complete
	}
	return nil
}

// completeLocked 实例视图凑齐（有名 + 有址且有端口——":0" 是 A 先到
// 的占位）即取出（清空占位防重报）。
func (bs *browseState) completeLocked(inst string) *Peer {
	p := bs.peers[inst]
	if p == nil || p.Name == "" || p.Addr == "" {
		return nil
	}
	if _, port, err := net.SplitHostPort(p.Addr); err != nil || port == "" || port == "0" {
		return nil
	}
	c := *p
	bs.peers[inst] = &Peer{}
	return &c
}

func itoa(n int) string {
	if n < 0 {
		return "0"
	}
	// 小整数快路径（端口 < 65536）
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// RunDiscovery 启动发现循环（阻塞直到 ctx 取消或 socket 失败）。
// 应答器：任何角色都开（节点供指挥官发现）；浏览：browse=true 时周期
// 组播 PTR 查询并回调 onPeer（commander 侧并入成员表）。组播不可用
//（无组播路由的宿主/CI）返回错误——调用方记日志降级，不影响集群其余
// 通道。
func RunDiscovery(ctx context.Context, self Identity, port int, browse bool, onPeer func(Peer)) error {
	conn, err := newRealMDNSConn()
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	runDiscovery(ctx, conn, self, port, browse, onPeer)
	return nil
}

// runDiscovery 可注入 conn 的发现循环本体（测试用）。
func runDiscovery(ctx context.Context, conn mdnsConn, self Identity, port int, browse bool, onPeer func(Peer)) {
	inst := dnsLabelSafe(self.Name) + "." + mdnsService
	bs := &browseState{peers: map[string]*Peer{}}

	query := encodeDNS(dnsMsg{Questions: []dnsQuestion{{Name: mdnsService, QType: typePTR}}})
	announce := encodeDNS(dnsMsg{Response: true, Answers: selfRecords(self, port, localIPv4s())})

	// 浏览侧：周期查询；应答侧：启动自宣 + 周期重宣（对端错过首发也有
	// 兜底）；周期 30s（查询）/120s（重宣）。
	queryT := time.NewTicker(30 * time.Second)
	defer queryT.Stop()
	annT := time.NewTicker(120 * time.Second)
	defer annT.Stop()
	var groupAddr net.Addr = &net.UDPAddr{IP: net.ParseIP(mdnsGroup), Port: mdnsPort}
	if browse {
		_, _ = conn.WriteTo(query, groupAddr)
	}
	_, _ = conn.WriteTo(announce, groupAddr)

	buf := make([]byte, mdnsMaxPacket)
	for {
		select {
		case <-ctx.Done():
			return
		case <-queryT.C:
			if browse {
				_, _ = conn.WriteTo(query, groupAddr)
			}
		case <-annT.C:
			_, _ = conn.WriteTo(announce, groupAddr)
		default:
		}
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			return // ctx 取消走 Close；真错误由外层重启（RunDiscovery 不重试——调用方降级）
		}
		if n <= 12 {
			continue
		}
		msg, err := parseDNS(buf[:n])
		if err != nil {
			continue
		}
		// 应答侧：问题点名本机服务/实例（任意类型）→ 回应答
		for _, q := range msg.Questions {
			if q.Name == mdnsService || q.Name == inst {
				_, _ = conn.WriteTo(announce, groupAddr)
				break
			}
		}
		// 浏览侧：应答记录喂聚合器
		if browse && msg.Response && onPeer != nil {
			for _, rr := range msg.Answers {
				if p := bs.feed(rr, src); p != nil {
					onPeer(*p)
				}
			}
		}
	}
}
