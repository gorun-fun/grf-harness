// mdns_codec.go mDNS 子集的 DNS wire 编解码：只做本包需要的 PTR/SRV/TXT/A
// 四类与问题/应答两段（authority/additional 不产生、解析时跳过）。发送侧
// 不做名字压缩（合法且实现简单）；解析侧支持压缩指针（macOS/Avahi 的
// 应答都压缩）。错误一律整包拒绝——发现通道宽容丢包、不容忍坏状态。
package cluster

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
)

// 记录类型（DNS 通用常量）。
const (
	typeA   = 1
	typePTR = 12
	typeTXT = 16
	typeSRV = 33
)

// classINET 唯一用到的 class；mDNS 的 cache-flush 位（0x8000）不设——
// 我们不依赖对端缓存语义。
const classINET = 1

// dnsQuestion 一个问题条目。
type dnsQuestion struct {
	Name  string
	QType uint16
}

// dnsRR 一条资源记录。RDATA 原样字节保留（A/TXT 直读）；名字类载荷
//（PTR 目标 / SRV 目标）在 parseDNS 时对整包解压后存进伴生字段——
// rdata 里的压缩指针指向整包偏移，脱离整包的切片无法解。
type dnsRR struct {
	Name      string
	Type      uint16
	Class     uint16
	TTL       uint32
	Data      []byte
	PtrTarget string // PTR：rdata 指向的实例名（FQDN）
	SRVPort   uint16 // SRV：端口
	SRVTarget string // SRV：目标名（FQDN）
}

// dnsMsg 头 + 问题 + 应答（mDNS 子集形状）。
type dnsMsg struct {
	ID        uint16
	Response  bool
	Questions []dnsQuestion
	Answers   []dnsRR
}

// errBadPacket wire 格式非法。
var errBadPacket = errors.New("mdns: malformed packet")

// encodeDNS 编码整包（问题在前、应答在后；无压缩）。
func encodeDNS(m dnsMsg) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, m.ID)
	flags := uint16(0)
	if m.Response {
		flags |= 0x8000 // QR
		flags |= 0x0400 // AA：自答权威
	}
	b = binary.BigEndian.AppendUint16(b, flags)
	b = binary.BigEndian.AppendUint16(b, uint16(len(m.Questions)))
	b = binary.BigEndian.AppendUint16(b, uint16(len(m.Answers)))
	b = binary.BigEndian.AppendUint16(b, 0) // NSCOUNT
	b = binary.BigEndian.AppendUint16(b, 0) // ARCOUNT
	for _, q := range m.Questions {
		b = appendName(b, q.Name)
		b = binary.BigEndian.AppendUint16(b, q.QType)
		b = binary.BigEndian.AppendUint16(b, classINET)
	}
	for _, rr := range m.Answers {
		b = appendName(b, rr.Name)
		b = binary.BigEndian.AppendUint16(b, rr.Type)
		b = binary.BigEndian.AppendUint16(b, rr.Class)
		b = binary.BigEndian.AppendUint32(b, rr.TTL)
		b = binary.BigEndian.AppendUint16(b, uint16(len(rr.Data)))
		b = append(b, rr.Data...)
	}
	return b
}

// parseDNS 解析整包（支持名字压缩指针）。
func parseDNS(b []byte) (dnsMsg, error) {
	if len(b) < 12 {
		return dnsMsg{}, errBadPacket
	}
	var m dnsMsg
	m.ID = binary.BigEndian.Uint16(b[0:2])
	m.Response = b[2]&0x80 != 0
	qd := int(binary.BigEndian.Uint16(b[4:6]))
	an := int(binary.BigEndian.Uint16(b[6:8]))
	ns := int(binary.BigEndian.Uint16(b[8:10]))
	ar := int(binary.BigEndian.Uint16(b[10:12]))
	if qd > 16 || an > 64 || ns > 64 || ar > 64 {
		return dnsMsg{}, errBadPacket // 防解引用疯狂计数
	}
	off := 12
	for i := 0; i < qd; i++ {
		name, n, err := readName(b, off)
		if err != nil || off+n+4 > len(b) {
			return dnsMsg{}, errBadPacket
		}
		m.Questions = append(m.Questions, dnsQuestion{Name: name, QType: binary.BigEndian.Uint16(b[off+n : off+n+2])})
		off += n + 4
	}
	// NS/AR 段的 RR 一并入 Answers（发现只关心记录本身，不关心段位）
	for i := 0; i < an+ns+ar; i++ {
		name, n, err := readName(b, off)
		if err != nil || off+n+10 > len(b) {
			return dnsMsg{}, errBadPacket
		}
		p := off + n
		typ := binary.BigEndian.Uint16(b[p : p+2])
		class := binary.BigEndian.Uint16(b[p+2 : p+4])
		ttl := binary.BigEndian.Uint32(b[p+4 : p+8])
		rdlen := int(binary.BigEndian.Uint16(b[p+8 : p+10]))
		if p+10+rdlen > len(b) {
			return dnsMsg{}, errBadPacket
		}
		rr := dnsRR{Name: name, Type: typ, Class: class, TTL: ttl, Data: b[p+10 : p+10+rdlen]}
		switch typ {
		case typePTR:
			if t, _, err := readName(b, p+10); err == nil {
				rr.PtrTarget = t
			}
		case typeSRV:
			if rdlen >= 8 {
				rr.SRVPort = binary.BigEndian.Uint16(b[p+10+4 : p+10+6])
				if t, _, err := readName(b, p+10+6); err == nil {
					rr.SRVTarget = t
				}
			}
		}
		m.Answers = append(m.Answers, rr)
		off = p + 10 + rdlen
	}
	return m, nil
}

// appendName 追加一个点分名字（无压缩；标签长度前缀编码）。
func appendName(b []byte, name string) []byte {
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return append(b, 0)
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 {
			label = label[:63]
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0)
}

// readName 读取一个名字（含压缩指针跟随；返回名字与消费的字节数——
// 指针本身占 2 字节，指针前的字节数照常计入，符合 DNS 消耗语义）。
// 返回名恒带尾部点（DNS FQDN 惯例）：与包内常量（mdnsService 等）的
// 比较无需再纠结点。
func readName(b []byte, off int) (string, int, error) {
	var labels []string
	consumed := 0
	ptrBudget := 16 // 指针环防护
	cur := off
	jumped := false
	for {
		if cur >= len(b) {
			return "", 0, errBadPacket
		}
		l := int(b[cur])
		switch {
		case l == 0:
			if !jumped {
				consumed = cur + 1 - off
			}
			return strings.Join(labels, ".") + ".", consumed, nil
		case l&0xc0 == 0xc0: // 压缩指针
			if cur+2 > len(b) {
				return "", 0, errBadPacket
			}
			target := int(binary.BigEndian.Uint16(b[cur:cur+2]) & 0x3fff)
			if !jumped {
				consumed = cur + 2 - off
				jumped = true
			}
			ptrBudget--
			if ptrBudget <= 0 || target >= cur {
				return "", 0, errBadPacket // 环/后跳（后跳必环）
			}
			cur = target
		case l <= 63:
			if cur+1+l > len(b) {
				return "", 0, errBadPacket
			}
			labels = append(labels, string(b[cur+1:cur+1+l]))
			cur += 1 + l
			if len(labels) > 128 {
				return "", 0, errBadPacket
			}
		default:
			return "", 0, errBadPacket
		}
	}
}

// srvRData SRV 记录载荷。
type srvRData struct {
	Priority uint16
	Weight   uint16
	Port     uint16
	Target   string
}

func buildSRV(d srvRData) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, d.Priority)
	b = binary.BigEndian.AppendUint16(b, d.Weight)
	b = binary.BigEndian.AppendUint16(b, d.Port)
	return appendName(b, d.Target)
}

func parseSRV(data []byte) (srvRData, error) {
	if len(data) < 8 {
		return srvRData{}, errBadPacket
	}
	d := srvRData{
		Priority: binary.BigEndian.Uint16(data[0:2]),
		Weight:   binary.BigEndian.Uint16(data[2:4]),
		Port:     binary.BigEndian.Uint16(data[4:6]),
	}
	target, n, err := readName(data, 6)
	if err != nil {
		return srvRData{}, err
	}
	if 6+n != len(data) {
		return srvRData{}, errBadPacket
	}
	d.Target = target
	return d, nil
}

// buildTXT / parseTXT TXT 载荷（长度前缀字符串对 "k=v"）。
func buildTXT(kvs []string) []byte {
	var b []byte
	for _, kv := range kvs {
		if len(kv) > 255 {
			kv = kv[:255]
		}
		b = append(b, byte(len(kv)))
		b = append(b, kv...)
	}
	if len(b) == 0 {
		return []byte{0}
	}
	return b
}

func parseTXT(data []byte) map[string]string {
	out := map[string]string{}
	for len(data) > 0 {
		l := int(data[0])
		if 1+l > len(data) {
			return out
		}
		kv := string(data[1 : 1+l])
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
		data = data[1+l:]
	}
	return out
}

// buildA / parseA IPv4 地址载荷。
func buildA(ip net.IP) []byte {
	ip = ip.To4()
	if ip == nil {
		return make([]byte, 4)
	}
	return ip
}

func parseA(data []byte) net.IP {
	if len(data) != 4 {
		return nil
	}
	return net.IPv4(data[0], data[1], data[2], data[3]).To4()
}

// dnsLabelSafe 服务实例名净化：DNS 标签字符集 [a-z0-9-]、≤63 字节。
func dnsLabelSafe(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "node"
	}
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}
