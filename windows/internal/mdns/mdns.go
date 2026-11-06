package mdns

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// ServiceType is the DNS-SD service LAN Share devices announce.
const ServiceType = "_lanshare._tcp.local."

var group = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Service describes this device.
type Service struct {
	Instance string            // friendly name, e.g. "DESKTOP-AB12"
	Host     string            // host label, e.g. "DESKTOP-AB12" (".local." is added)
	Port     int               // TCP port of our TLS server
	TXT      map[string]string // v, id, p
}

// Peer is another LAN Share device seen on the network.
type Peer struct {
	Instance string
	ID       string // key fingerprint from TXT "id"
	Platform string
	Addr     netip.AddrPort
}

// Node answers questions about our service and reports peers it hears about.
type Node struct {
	svc    *Service // nil = browse only, never answer
	onPeer func(Peer)
	conns  []*ifConn

	mu    sync.Mutex
	srv   map[string]record // instance name → SRV
	txt   map[string]record // instance name → TXT
	addrs map[string]netip.Addr
	done  chan struct{}
}

type ifConn struct {
	c    *net.UDPConn
	ip   netip.Addr   // our IPv4 on this interface
	net  netip.Prefix // its subnet
	name string
}

// Start listens on every usable network interface. svc may be nil (browse only).
func Start(svc *Service, onPeer func(Peer)) (*Node, error) {
	n := &Node{
		svc: svc, onPeer: onPeer, done: make(chan struct{}),
		srv: map[string]record{}, txt: map[string]record{}, addrs: map[string]netip.Addr{},
	}
	if svc != nil {
		svc.Instance = cleanLabel(svc.Instance)
		svc.Host = cleanLabel(svc.Host)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := range ifaces {
		ifi := &ifaces[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		ip, pfx, ok := ipv4Of(ifi)
		if !ok {
			continue
		}
		c, err := net.ListenMulticastUDP("udp4", ifi, group)
		if err != nil {
			lastErr = err
			continue
		}
		ic := &ifConn{c: c, ip: ip, net: pfx, name: ifi.Name}
		n.conns = append(n.conns, ic)
		go n.read(ic)
	}
	if len(n.conns) == 0 {
		if lastErr == nil {
			lastErr = net.UnknownNetworkError("no network interface with IPv4 multicast")
		}
		return nil, lastErr
	}
	if svc != nil {
		n.announce(120)
	}
	return n, nil
}

// Query asks every interface "who offers _lanshare._tcp?". Answers arrive via onPeer.
func (n *Node) Query() {
	pkt := encodeQuery([]question{{name: ServiceType, qtype: typePTR}})
	for _, ic := range n.conns {
		ic.c.WriteToUDP(pkt, group)
	}
}

// Close says goodbye (TTL 0, so peers drop us at once) and stops listening.
func (n *Node) Close() {
	select {
	case <-n.done:
		return
	default:
	}
	if n.svc != nil {
		n.announce(0)
	}
	close(n.done)
	for _, ic := range n.conns {
		ic.c.Close()
	}
}

func (n *Node) instanceName() string { return n.svc.Instance + "." + ServiceType }
func (n *Node) hostName() string     { return n.svc.Host + ".local." }

// records builds our full record set for one interface's address.
func (n *Node) records(ip netip.Addr, ttl uint32) (ptr, srv, txt, a record) {
	ptr = record{name: ServiceType, rtype: typePTR, ttl: ttl, target: n.instanceName()}
	srv = record{name: n.instanceName(), rtype: typeSRV, ttl: ttl, port: uint16(n.svc.Port), target: n.hostName()}
	txt = record{name: n.instanceName(), rtype: typeTXT, ttl: ttl, txt: n.svc.TXT}
	a = record{name: n.hostName(), rtype: typeA, ttl: ttl, addr: ip}
	return
}

func (n *Node) announce(ttl uint32) {
	for _, ic := range n.conns {
		ptr, srv, txt, a := n.records(ic.ip, ttl)
		ic.c.WriteToUDP(encodeResponse(0, []record{ptr, srv, txt, a}, nil), group)
	}
}

func (n *Node) read(ic *ifConn) {
	buf := make([]byte, 9000)
	for {
		size, src, err := ic.c.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-n.done:
				return
			default:
			}
			time.Sleep(200 * time.Millisecond) // transient error: don't spin
			continue
		}
		m, err := parse(buf[:size])
		if err != nil {
			continue
		}
		if m.response {
			n.learn(m)
		} else if n.svc != nil {
			n.answer(m, src)
		}
	}
}

// answer replies to questions about our service, instance or host.
func (n *Node) answer(m *message, src *net.UDPAddr) {
	srcIP, _ := netip.AddrFromSlice(src.IP.To4())
	// Reply on the interface whose subnet the asker is on, with that interface's IP.
	ic := n.conns[0]
	for _, c := range n.conns {
		if c.net.Contains(srcIP) {
			ic = c
			break
		}
	}
	ptr, srv, txt, a := n.records(ic.ip, 120)
	var answers, extras []record
	unicast := src.Port != 5353 // a "legacy" one-shot query wants a direct reply
	for _, q := range m.questions {
		any := q.qtype == typeANY
		switch {
		case sameName(q.name, ServiceType) && (q.qtype == typePTR || any):
			answers = append(answers, ptr)
			extras = append(extras, srv, txt, a)
		case sameName(q.name, n.instanceName()) && (q.qtype == typeSRV || any):
			answers = append(answers, srv)
			extras = append(extras, a)
		case sameName(q.name, n.instanceName()) && q.qtype == typeTXT:
			answers = append(answers, txt)
		case sameName(q.name, n.hostName()) && (q.qtype == typeA || any):
			answers = append(answers, a)
		default:
			continue
		}
		if q.qu {
			unicast = true
		}
	}
	if len(answers) == 0 {
		return
	}
	if unicast {
		ic.c.WriteToUDP(encodeResponse(m.id, answers, extras), src)
	} else {
		ic.c.WriteToUDP(encodeResponse(0, answers, extras), group)
	}
}

// learn remembers records from a response and reports complete peers.
func (n *Node) learn(m *message) {
	var instances []string
	n.mu.Lock()
	for _, r := range m.records {
		switch r.rtype {
		case typePTR:
			if sameName(r.name, ServiceType) {
				instances = append(instances, r.target)
			}
		case typeSRV:
			n.srv[strings.ToLower(r.name)] = r
			instances = append(instances, r.name)
		case typeTXT:
			n.txt[strings.ToLower(r.name)] = r
		case typeA:
			if r.addr.IsValid() {
				n.addrs[strings.ToLower(r.name)] = r.addr
			}
		}
	}
	var peers []Peer
	for _, inst := range instances {
		key := strings.ToLower(inst)
		if !strings.HasSuffix(key, ServiceType) {
			continue
		}
		srv, ok1 := n.srv[key]
		txt, ok2 := n.txt[key]
		addr, ok3 := n.addrs[strings.ToLower(srv.target)]
		if !ok1 || !ok2 || !ok3 || srv.ttl == 0 || txt.txt["id"] == "" {
			continue
		}
		if n.svc != nil && txt.txt["id"] == n.svc.TXT["id"] {
			continue // that's us
		}
		peers = append(peers, Peer{
			Instance: strings.TrimSuffix(inst[:len(inst)-len(ServiceType)], "."),
			ID:       txt.txt["id"],
			Platform: txt.txt["p"],
			Addr:     netip.AddrPortFrom(addr, srv.port),
		})
	}
	n.mu.Unlock()
	if n.onPeer != nil {
		for _, p := range peers {
			n.onPeer(p)
		}
	}
}

func ipv4Of(ifi *net.Interface) (netip.Addr, netip.Prefix, bool) {
	addrs, _ := ifi.Addrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		ip, _ := netip.AddrFromSlice(ipn.IP.To4())
		ones, _ := ipn.Mask.Size()
		return ip, netip.PrefixFrom(ip, ones).Masked(), true
	}
	return netip.Addr{}, netip.Prefix{}, false
}

// cleanLabel makes a name safe as one DNS label: no dots, at most 63 bytes.
func cleanLabel(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, ".", " "))
	if s == "" {
		s = "LanShare"
	}
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}
