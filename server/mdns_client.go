package weavedns

import (
	"github.com/miekg/dns"
	"math"
	"net"
	"time"
)

// Portions of this code taken from github.com/armon/mdns

const (
	ipv4mdns = "224.0.0.251" // link-local multicast address
	mdnsPort = 5353          // mDNS assigned port
	// We wait this long to hear responses from other mDNS servers on the network.
	// TODO: introduce caching so we don't have to wait this long on every call.
	mDNSTimeout = 500 * time.Millisecond
	MaxDuration = time.Duration(math.MaxInt64)
	MailboxSize = 16
)

var (
	ipv4Addr = &net.UDPAddr{
		IP:   net.ParseIP(ipv4mdns),
		Port: mdnsPort,
	}
)

type ResponseA struct {
	Name string
	Addr net.IP
	Err  error
}

type responseInfo struct {
	timeout time.Time // if no answer by this time, give up
	ch      chan<- *ResponseA
}

// Represents one query that we have sent for one name.
// If we, internally, get several requests for the same name while we have
// a query in flight, then we don't want to send more queries out.
// Invariant on responseInfos: they are in non-descending order of timeout.
type inflightQuery struct {
	name          string
	id            uint16 // the DNS message ID
	responseInfos []*responseInfo
}

type MDNSClient struct {
	server    *dns.Server
	conn      *net.UDPConn
	addr      *net.UDPAddr
	inflight  map[string]*inflightQuery
	queryChan chan<- *MDNSInteraction
}

type mDNSQueryInfo struct {
	name       string
	querytype  uint16
	responseCh chan<- *ResponseA
}

func NewMDNSClient() (*MDNSClient, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	return &MDNSClient{
		conn:     conn,
		addr:     ipv4Addr,
		inflight: make(map[string]*inflightQuery)}, nil
}

func (c *MDNSClient) Start(ifi *net.Interface) error {
	multicast, err := LinkLocalMulticastListener(ifi)
	if err != nil {
		return err
	}

	handleMDNS := func(w dns.ResponseWriter, r *dns.Msg) {
		// Don't want to handle queries here, so filter anything out that isn't a response
		if len(r.Answer) > 0 {
			c.ResponseCallback(r)
		}
	}

	c.server = &dns.Server{Listener: nil, PacketConn: multicast, Handler: dns.HandlerFunc(handleMDNS)}
	go c.server.ActivateAndServe()

	queryChan := make(chan *MDNSInteraction, MailboxSize)
	c.queryChan = queryChan
	go c.queryLoop(queryChan)

	return nil
}

func LinkLocalMulticastListener(ifi *net.Interface) (net.PacketConn, error) {
	conn, err := net.ListenMulticastUDP("udp", ifi, ipv4Addr)
	return conn, err
}

// ACTOR client API

const (
	CSendQuery       = iota
	CShutdown        = iota
	CMessageReceived = iota
)

type MDNSInteraction struct {
	code       int
	resultChan chan<- interface{}
	payload    interface{}
}

// Async
func (c *MDNSClient) Shutdown() {
	c.queryChan <- &MDNSInteraction{code: CShutdown}
}

// Async
func (c *MDNSClient) SendQuery(name string, querytype uint16, responseCh chan<- *ResponseA) {
	c.queryChan <- &MDNSInteraction{
		code:    CSendQuery,
		payload: mDNSQueryInfo{name, querytype, responseCh},
	}
}

// Async - called from dns library multiplexer
func (c *MDNSClient) ResponseCallback(r *dns.Msg) {
	c.queryChan <- &MDNSInteraction{code: CMessageReceived, payload: r}
}

// ACTOR server

// Check all in-flight queries, close all that have already timed out,
// and return the duration until the next timeout
func (c *MDNSClient) checkInFlightQueries() time.Duration {
	now := time.Now()
	after := MaxDuration
	for name, query := range c.inflight {
		// Invariant on responseInfos: they are in non-descending order of timeout.
		numClosed := 0
		for _, item := range query.responseInfos {
			duration := item.timeout.Sub(now)
			if duration <= 0 { // timed out
				close(item.ch)
				numClosed++
			} else {
				if duration < after {
					after = duration
				}
				break // don't need to look at any more for this query
			}
		}
		// Remove timed-out items from the slice
		query.responseInfos = query.responseInfos[numClosed:]
		if len(query.responseInfos) == 0 {
			delete(c.inflight, name)
		}
	}
	return after
}

func (c *MDNSClient) queryLoop(queryChan <-chan *MDNSInteraction) {
	timer := time.NewTimer(MaxDuration)
	run := func() {
		timer.Reset(c.checkInFlightQueries())
	}

	terminate := false
	for !terminate {
		select {
		case query, ok := <-queryChan:
			if !ok {
				break
			}
			switch query.code {
			case CShutdown:
				c.server.Shutdown()
				terminate = true
			case CSendQuery:
				c.handleSendQuery(query.payload.(mDNSQueryInfo))
				run()
			case CMessageReceived:
				c.handleResponse(query.payload.(*dns.Msg))
				run()
			}
		case <-timer.C:
			run()
		}
	}

	// Close all response channels at termination
	for _, query := range c.inflight {
		for _, item := range query.responseInfos {
			close(item.ch)
		}
	}
}

func (c *MDNSClient) handleSendQuery(q mDNSQueryInfo) {
	query, found := c.inflight[q.name]
	if !found {
		m := new(dns.Msg)
		m.SetQuestion(q.name, q.querytype)
		m.RecursionDesired = false
		buf, err := m.Pack()
		if err != nil {
			q.responseCh <- &ResponseA{Err: err}
			close(q.responseCh)
			return
		}
		query = &inflightQuery{
			name: q.name,
			id:   m.Id,
		}
		_, err = c.conn.WriteTo(buf, c.addr)
		if err != nil {
			q.responseCh <- &ResponseA{Err: err}
			close(q.responseCh)
			return
		}
		c.inflight[q.name] = query
	}
	info := &responseInfo{
		ch:      q.responseCh,
		timeout: time.Now().Add(mDNSTimeout),
	}
	// Invariant on responseInfos: they are in non-descending order of timeout.
	// Since we use a fixed interval from Now(), this must be after all existing timeouts.
	query.responseInfos = append(query.responseInfos, info)
}

func (c *MDNSClient) handleResponse(r *dns.Msg) {
	for _, answer := range r.Answer {
		switch rr := answer.(type) {
		case *dns.A:
			name := rr.Hdr.Name
			if query, found := c.inflight[name]; found {
				for _, resp := range query.responseInfos {
					resp.ch <- &ResponseA{Name: rr.Hdr.Name, Addr: rr.A}
					close(resp.ch)
				}
				delete(c.inflight, name)
			} else {
				// We've received a response that didn't match a query
				// Do we want to cache it?
			}
		}
	}
}
