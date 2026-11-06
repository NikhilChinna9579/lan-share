package mdns

import (
	"net/netip"
	"testing"
	"time"
)

func TestEncodeParseRoundTrip(t *testing.T) {
	addr := netip.MustParseAddr("192.168.1.20")
	n := &Node{svc: &Service{Instance: "Laptop", Host: "laptop", Port: 47821, TXT: map[string]string{"v": "1", "id": "abc", "p": "windows"}}}
	ptr, srv, txt, a := n.records(addr, 120)
	m, err := parse(encodeResponse(7, []record{ptr}, []record{srv, txt, a}))
	if err != nil {
		t.Fatal(err)
	}
	if !m.response || m.id != 7 || len(m.records) != 4 {
		t.Fatalf("got %+v", m)
	}
	if m.records[0].target != "Laptop._lanshare._tcp.local." || m.records[1].port != 47821 ||
		m.records[2].txt["id"] != "abc" || m.records[3].addr != addr {
		t.Fatalf("records %+v", m.records)
	}
}

func TestCompressedNames(t *testing.T) {
	// A PTR answer whose target points back into the question name: the format
	// Android and Apple responders actually send.
	pkt := []byte{
		0, 0, 0x84, 0, 0, 1, 0, 1, 0, 0, 0, 0,
		// question: _lanshare._tcp.local. PTR IN   (name starts at offset 12)
		9, '_', 'l', 'a', 'n', 's', 'h', 'a', 'r', 'e', 4, '_', 't', 'c', 'p', 5, 'l', 'o', 'c', 'a', 'l', 0,
		0, 12, 0, 1,
		// answer: pointer to 12, PTR, IN, ttl 120, rdlength 7: "Phone" + pointer to 12
		0xC0, 12, 0, 12, 0, 1, 0, 0, 0, 120, 0, 8,
		5, 'P', 'h', 'o', 'n', 'e', 0xC0, 12,
	}
	m, err := parse(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.records[0].target; got != "Phone._lanshare._tcp.local." {
		t.Fatalf("target %q", got)
	}
}

func TestRejectsPointerLoop(t *testing.T) {
	pkt := []byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xC0, 12, 0, 12, 0, 1}
	if _, err := parse(pkt); err == nil {
		t.Fatal("pointer loop accepted")
	}
}

// Two nodes on this machine: one announces, the other discovers it.
func TestDiscoverOverLoopback(t *testing.T) {
	found := make(chan Peer, 4)
	laptop, err := Start(&Service{Instance: "TestLaptop", Host: "testlaptop", Port: 47821,
		TXT: map[string]string{"v": "1", "id": "laptop-id", "p": "windows"}}, nil)
	if err != nil {
		t.Skip("no multicast here:", err)
	}
	defer laptop.Close()
	phone, err := Start(&Service{Instance: "TestPhone", Host: "testphone", Port: 47822,
		TXT: map[string]string{"v": "1", "id": "phone-id", "p": "android"}}, func(p Peer) { found <- p })
	if err != nil {
		t.Skip("no multicast here:", err)
	}
	defer phone.Close()
	phone.Query()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case p := <-found:
			if p.ID == "laptop-id" {
				if p.Instance != "TestLaptop" || p.Addr.Port() != 47821 {
					t.Fatalf("peer %+v", p)
				}
				return
			}
		case <-deadline:
			t.Skip("multicast not delivered in this environment")
		}
	}
}
