package weavedns

import (
	"github.com/miekg/dns"
	"net"
)

type MDNSServer struct {
	localAddrs []net.Addr
	sendconn   *net.UDPConn
	zone       Zone
}

func NewMDNSServer(zone Zone) (*MDNSServer, error) {
	// This is a bit of a kludge - per the RFC we should send responses from 5353, but that doesn't seem to work
	sendconn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	retval := &MDNSServer{sendconn: sendconn, zone: zone}

	return retval, nil
}

func makeDNSReply(r *dns.Msg, name string, qtype uint16, addrs []net.IP) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.RecursionAvailable = true
	hdr := dns.RR_Header{Name: name, Rrtype: qtype, Class: dns.ClassINET, Ttl: 3600}
	for _, addr := range addrs {
		if qtype == dns.TypeA {
			if ip4 := addr.To4(); ip4 != nil {
				m.Answer = append(m.Answer, &dns.A{hdr, addr})
			}
		} else if qtype == dns.TypeAAAA {
			if ip4 := addr.To4(); ip4 == nil {
				m.Answer = append(m.Answer, &dns.AAAA{hdr, addr})
			}
		}
	}
	return m
}

// Return true if testaddr is a UDP address with IP matching my local i/f
func (s *MDNSServer) addrIsLocal(testaddr net.Addr) bool {
	if udpaddr, ok := testaddr.(*net.UDPAddr); ok {
		for _, localaddr := range s.localAddrs {
			if ipnetlocal, ok := localaddr.(*net.IPNet); ok {
				if ipnetlocal.IP.Equal(udpaddr.IP) {
					return true
				}
			}
		}
	}
	return false
}

func (s *MDNSServer) Start(ifi *net.Interface) error {
	handleMDNS := func(w dns.ResponseWriter, r *dns.Msg) {
		// Ignore answers to other questions
		if len(r.Answer) == 0 && len(r.Question) > 0 {
			q := r.Question[0]
			ip, err := s.zone.MatchLocal(q.Name)
			if err == nil {
				m := makeDNSReply(r, q.Name, dns.TypeA, []net.IP{ip})
				s.SendResponse(m)
			} else if s.addrIsLocal(w.RemoteAddr()) {
				// ignore this - it's our own query received via multicast
			} else {
				Debug.Printf("Failed MDNS lookup for %s", q.Name)
			}
		}
	}

	conn, err := LinkLocalMulticastListener(ifi)
	if err != nil {
		return err
	}
	if ifi == nil {
		s.localAddrs, err = net.InterfaceAddrs()
	} else {
		s.localAddrs, err = ifi.Addrs()
	}
	if err != nil {
		return err
	}

	go dns.ActivateAndServe(nil, conn, dns.HandlerFunc(handleMDNS))

	return err
}

func (s *MDNSServer) SendResponse(m *dns.Msg) error {
	buf, err := m.Pack()
	if err != nil {
		return err
	}
	_, err = s.sendconn.WriteTo(buf, ipv4Addr)
	return err
}
