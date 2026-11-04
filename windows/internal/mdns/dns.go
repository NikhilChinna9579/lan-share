// Package mdns is a small multicast-DNS responder and browser for one service type
// (PROTOCOL.md §2). It uses only the standard library, so the .exe stays small.
//
// How mDNS works, in short: every device listens on UDP 224.0.0.251:5353. To find
// "_lanshare._tcp.local." you multicast a question; every device offering it multicasts
// an answer: PTR (service → instance), SRV (instance → host + port), TXT (instance →
// key=value pairs) and A (host → IPv4 address).
package mdns

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
)

// DNS record types and classes we use.
const (
	typeA   = 1
	typePTR = 12
	typeTXT = 16
	typeSRV = 33
	typeANY = 255

	classIN    = 1
	cacheFlush = 0x8000 // top bit of class in answers: "replace what you cached"
	unicastQ   = 0x8000 // top bit of class in questions: "answer me directly"
)

type question struct {
	name  string
	qtype uint16
	qu    bool // unicast response requested
}

type record struct {
	name  string
	rtype uint16
	ttl   uint32

	target string            // PTR target, SRV target
	port   uint16            // SRV
	txt    map[string]string // TXT
	addr   netip.Addr        // A
}

type message struct {
	id        uint16
	response  bool
	questions []question
	records   []record // answers, authorities and additionals together
}

var errMalformed = errors.New("mdns: malformed packet")

// ---------- encoding ----------

type builder struct{ b []byte }

func (w *builder) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *builder) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }

// name writes a domain name without compression (allowed, just a little larger).
func (w *builder) name(n string) {
	for _, label := range strings.Split(strings.TrimSuffix(n, "."), ".") {
		if label == "" {
			continue
		}
		if len(label) > 63 {
			label = label[:63]
		}
		w.b = append(w.b, byte(len(label)))
		w.b = append(w.b, label...)
	}
	w.b = append(w.b, 0)
}

func encodeQuery(qs []question) []byte {
	w := &builder{}
	w.u16(0) // id: 0 for multicast
	w.u16(0) // flags: standard query
	w.u16(uint16(len(qs)))
	w.u16(0)
	w.u16(0)
	w.u16(0)
	for _, q := range qs {
		w.name(q.name)
		w.u16(q.qtype)
		class := uint16(classIN)
		if q.qu {
			class |= unicastQ
		}
		w.u16(class)
	}
	return w.b
}

// encodeResponse puts answers in the answer section and extras in additionals.
func encodeResponse(id uint16, answers, extras []record) []byte {
	w := &builder{}
	w.u16(id)
	w.u16(0x8400) // response + authoritative
	w.u16(0)
	w.u16(uint16(len(answers)))
	w.u16(0)
	w.u16(uint16(len(extras)))
	for _, r := range append(append([]record{}, answers...), extras...) {
		w.name(r.name)
		w.u16(r.rtype)
		class := uint16(classIN)
		if r.rtype != typePTR { // PTR is shared; SRV/TXT/A are unique to us
			class |= cacheFlush
		}
		w.u16(class)
		w.u32(r.ttl)
		lenAt := len(w.b)
		w.u16(0) // rdata length, patched below
		switch r.rtype {
		case typePTR:
			w.name(r.target)
		case typeSRV:
			w.u16(0) // priority
			w.u16(0) // weight
			w.u16(r.port)
			w.name(r.target)
		case typeTXT:
			keys := make([]string, 0, len(r.txt))
			for k := range r.txt {
				keys = append(keys, k)
			}
			sortStrings(keys)
			for _, k := range keys {
				s := k + "=" + r.txt[k]
				if len(s) > 255 {
					s = s[:255]
				}
				w.b = append(w.b, byte(len(s)))
				w.b = append(w.b, s...)
			}
			if len(keys) == 0 {
				w.b = append(w.b, 0)
			}
		case typeA:
			a := r.addr.As4()
			w.b = append(w.b, a[:]...)
		}
		binary.BigEndian.PutUint16(w.b[lenAt:], uint16(len(w.b)-lenAt-2))
	}
	return w.b
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------- decoding ----------

// readName reads a possibly-compressed name starting at off. It returns the name
// (lowercase-insensitive comparison is done by callers) and the offset after it.
func readName(msg []byte, off int) (string, int, error) {
	var labels []string
	end := -1 // where the name ends in the original position (before following pointers)
	for hops := 0; ; hops++ {
		if off >= len(msg) || hops > 64 {
			return "", 0, errMalformed
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			return strings.Join(labels, ".") + ".", end, nil
		case l&0xC0 == 0xC0: // compression pointer
			if off+1 >= len(msg) {
				return "", 0, errMalformed
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
		case l&0xC0 != 0:
			return "", 0, errMalformed
		default:
			if off+1+l > len(msg) {
				return "", 0, errMalformed
			}
			labels = append(labels, string(msg[off+1:off+1+l]))
			off += 1 + l
		}
	}
}

func parse(msg []byte) (*message, error) {
	if len(msg) < 12 {
		return nil, errMalformed
	}
	m := &message{
		id:       binary.BigEndian.Uint16(msg[0:]),
		response: msg[2]&0x80 != 0,
	}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	rr := int(binary.BigEndian.Uint16(msg[6:])) + int(binary.BigEndian.Uint16(msg[8:])) + int(binary.BigEndian.Uint16(msg[10:]))
	off := 12
	for i := 0; i < qd; i++ {
		name, n, err := readName(msg, off)
		if err != nil || n+4 > len(msg) {
			return nil, errMalformed
		}
		class := binary.BigEndian.Uint16(msg[n+2:])
		m.questions = append(m.questions, question{
			name: name, qtype: binary.BigEndian.Uint16(msg[n:]), qu: class&unicastQ != 0,
		})
		off = n + 4
	}
	for i := 0; i < rr; i++ {
		name, n, err := readName(msg, off)
		if err != nil || n+10 > len(msg) {
			return nil, errMalformed
		}
		r := record{
			name:  name,
			rtype: binary.BigEndian.Uint16(msg[n:]),
			ttl:   binary.BigEndian.Uint32(msg[n+4:]),
		}
		rdlen := int(binary.BigEndian.Uint16(msg[n+8:]))
		rd := n + 10
		if rd+rdlen > len(msg) {
			return nil, errMalformed
		}
		switch r.rtype {
		case typePTR:
			r.target, _, err = readName(msg, rd)
		case typeSRV:
			if rdlen >= 7 {
				r.port = binary.BigEndian.Uint16(msg[rd+4:])
				r.target, _, err = readName(msg, rd+6)
			}
		case typeTXT:
			r.txt = map[string]string{}
			for p := rd; p < rd+rdlen; {
				l := int(msg[p])
				if p+1+l > rd+rdlen {
					break
				}
				kv := string(msg[p+1 : p+1+l])
				if k, v, ok := strings.Cut(kv, "="); ok {
					r.txt[strings.ToLower(k)] = v
				}
				p += 1 + l
			}
		case typeA:
			if rdlen == 4 {
				r.addr = netip.AddrFrom4([4]byte(msg[rd : rd+4]))
			}
		}
		if err != nil {
			return nil, errMalformed
		}
		m.records = append(m.records, r)
		off = rd + rdlen
	}
	return m, nil
}

// sameName compares DNS names case-insensitively, ignoring a trailing dot.
func sameName(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}
