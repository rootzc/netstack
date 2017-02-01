// Copyright 2016 The Netstack Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tcp

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"hash"
	"io"
	"sync"
	"time"

	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/seqnum"
	"github.com/google/netstack/tcpip/stack"
	"github.com/google/netstack/waiter"
)

const (
	// tsLen is the length, in bits, of the timestamp in the SYN cookie.
	tsLen = 8

	// tsMask is a mask for timestamp values (i.e., tsLen bits).
	tsMask = (1 << tsLen) - 1

	// tsOffset is the offset, in bits, of the timestamp in the SYN cookie.
	tsOffset = 24

	// hashMask is the mask for hash values (i.e., tsOffset bits).
	hashMask = (1 << tsOffset) - 1

	// maxTSDiff is the maximum allowed difference between a received cookie
	// timestamp and the current timestamp. If the difference is greater
	// than maxTSDiff, the cookie is expired.
	maxTSDiff = 2
)

var (
	// SynRcvdCountThreshold is the global maximum number of connections
	// that are allowed to be in SYN-RCVD state before TCP starts using SYN
	// cookies to accept connections.
	//
	// It is an exported variable only for testing, and should not otherwise
	// be used by importers of this package.
	SynRcvdCountThreshold uint64 = 1000

	// mssTable is a slice containing the possible MSS values that we
	// encode in the SYN cookie with two bits.
	mssTable = []uint16{536, 1300, 1440, 1460}
)

func encodeMSS(mss uint16) uint32 {
	for i := len(mssTable) - 1; i > 0; i-- {
		if mss >= mssTable[i] {
			return uint32(i)
		}
	}
	return 0
}

// syncRcvdCount is the number of endpoints in the SYN-RCVD state. The value is
// protected by a mutex so that we can increment only when it's guaranteed not
// to go above a threshold.
var synRcvdCount struct {
	sync.Mutex
	value uint64
}

// listenContext is used by a listening endpoint to store state used while
// listening for connections. This struct is allocated by the listen goroutine
// and must not be accessed or have its methods called concurrently as they
// may mutate the stored objects.
type listenContext struct {
	stack  *stack.Stack
	rcvWnd seqnum.Size
	nonce  [2][sha1.BlockSize]byte

	hasherMu sync.Mutex
	hasher   hash.Hash
	v6only   bool
	netProto tcpip.NetworkProtocolNumber
}

// timeStamp returns an 8-bit timestamp with a granularity of 64 seconds.
func timeStamp() uint32 {
	return uint32(time.Now().Unix()>>6) & tsMask
}

// incSynRcvdCount tries to increment the global number of endpoints in SYN-RCVD
// state. It succeeds if the increment doesn't make the count go beyond the
// threshold, and fails otherwise.
func incSynRcvdCount() bool {
	synRcvdCount.Lock()
	defer synRcvdCount.Unlock()

	if synRcvdCount.value >= SynRcvdCountThreshold {
		return false
	}

	synRcvdCount.value++

	return true
}

// decSynRcvdCount atomically decrements the global number of endpoints in
// SYN-RCVD state. It must only be called if a previous call to incSynRcvdCount
// succeeded.
func decSynRcvdCount() {
	synRcvdCount.Lock()
	defer synRcvdCount.Unlock()

	synRcvdCount.value--
}

// newListenContext creates a new listen context.
func newListenContext(stack *stack.Stack, rcvWnd seqnum.Size, v6only bool, netProto tcpip.NetworkProtocolNumber) *listenContext {
	l := &listenContext{
		stack:    stack,
		rcvWnd:   rcvWnd,
		hasher:   sha1.New(),
		v6only:   v6only,
		netProto: netProto,
	}

	rand.Read(l.nonce[0][:])
	rand.Read(l.nonce[1][:])

	return l
}

// cookieHash calculates the cookieHash for the given id, timestamp and nonce
// index. The hash is used to create and validate cookies.
func (l *listenContext) cookieHash(id stack.TransportEndpointID, ts uint32, nonceIndex int) uint32 {

	// Initialize block with fixed-size data: local ports and v.
	var payload [8]byte
	binary.BigEndian.PutUint16(payload[0:], id.LocalPort)
	binary.BigEndian.PutUint16(payload[2:], id.RemotePort)
	binary.BigEndian.PutUint32(payload[4:], ts)

	// Feed everything to the hasher.
	l.hasherMu.Lock()
	l.hasher.Reset()
	l.hasher.Write(payload[:])
	l.hasher.Write(l.nonce[nonceIndex][:])
	io.WriteString(l.hasher, string(id.LocalAddress))
	io.WriteString(l.hasher, string(id.RemoteAddress))

	// Finalize the calculation of the hash and return the first 4 bytes.
	h := make([]byte, 0, sha1.Size)
	h = l.hasher.Sum(h)
	l.hasherMu.Unlock()

	return binary.BigEndian.Uint32(h[:])
}

// createCookie creates a SYN cookie for the given id and incoming sequence
// number.
func (l *listenContext) createCookie(id stack.TransportEndpointID, seq seqnum.Value, data uint32) seqnum.Value {
	ts := timeStamp()
	v := l.cookieHash(id, 0, 0) + uint32(seq) + (ts << tsOffset)
	v += (l.cookieHash(id, ts, 1) + data) & hashMask
	return seqnum.Value(v)
}

// isCookieValid checks if the supplied cookie is valid for the given id and
// sequence number. If it is, it also returns the data originally encoded in the
// cookie when createCookie was called.
func (l *listenContext) isCookieValid(id stack.TransportEndpointID, cookie seqnum.Value, seq seqnum.Value) (uint32, bool) {
	ts := timeStamp()
	v := uint32(cookie) - l.cookieHash(id, 0, 0) - uint32(seq)
	cookieTS := v >> tsOffset
	if ((ts - cookieTS) & tsMask) > maxTSDiff {
		return 0, false
	}

	return (v - l.cookieHash(id, cookieTS, 1)) & hashMask, true
}

// createConnectedEndpoint creates a new connected endpoint, with the connection
// parameters given by the arguments.
func (l *listenContext) createConnectedEndpoint(s *segment, iss seqnum.Value, irs seqnum.Value, mss uint16, sndWndScale int) (*endpoint, error) {
	// Create a new endpoint.
	netProto := l.netProto
	if netProto == 0 {
		netProto = s.route.NetProto
	}
	n := newEndpoint(l.stack, netProto, nil)
	n.v6only = l.v6only
	n.id = s.id
	n.boundNICID = s.route.NICID()
	n.route = s.route.Clone()
	n.effectiveNetProtos = []tcpip.NetworkProtocolNumber{s.route.NetProto}
	n.rcvBufSize = int(l.rcvWnd)

	// Register new endpoint so that packets are routed to it.
	if err := n.stack.RegisterTransportEndpoint(n.boundNICID, n.effectiveNetProtos, ProtocolNumber, n.id, n); err != nil {
		n.Close()
		return nil, err
	}

	n.isRegistered = true
	n.state = stateConnected

	// Create sender and receiver.
	//
	// The receiver at least temporarily has a zero receive window scale,
	// but the caller may change it (before starting the protocol loop).
	n.snd = newSender(n, iss, s.window, mss, sndWndScale)
	n.rcv = newReceiver(n, irs, l.rcvWnd, 0)

	return n, nil
}

// createEndpoint creates a new endpoint in connected state and then performs
// the TCP 3-way handshake.
func (l *listenContext) createEndpointAndPerformHandshake(s *segment, mss uint16, sndWndScale int) (*endpoint, error) {
	// Create new endpoint.
	irs := s.sequenceNumber
	cookie := l.createCookie(s.id, irs, encodeMSS(mss))
	ep, err := l.createConnectedEndpoint(s, cookie, irs, mss, sndWndScale)
	if err != nil {
		return nil, err
	}

	// Perform the 3-way handshake.
	h, err := newHandshake(ep, l.rcvWnd)
	if err != nil {
		ep.Close()
		return nil, err
	}

	h.resetToSynRcvd(cookie, irs, mss, sndWndScale)
	if err := h.execute(); err != nil {
		ep.Close()
		return nil, err
	}

	// Update the receive window scaling. We can't do it before the
	// handshake because it's possible that the peer doesn't support window
	// scaling.
	ep.rcv.rcvWndScale = h.effectiveRcvWndScale()

	return ep, nil
}

// deliverAccepted delivers the newly-accepted endpoint to the listener. If the
// endpoint has transitioned out of the listen state, the new endpoint is
// closed instead.
func (e *endpoint) deliverAccepted(n *endpoint) {
	e.mu.RLock()
	if e.state == stateListen {
		e.acceptedChan <- n
		e.waiterQueue.Notify(waiter.EventIn)
	} else {
		n.Close()
	}
	e.mu.RUnlock()
}

// handleSynSegment is called in its own goroutine once the listening
// endpoint receives a SYN segment. It is responsible for completing the
// handshake and queueing the new endpoint for acceptance.
//
// A limited number of these goroutines are allowed before TCP starts using
// SYN cookies to accept connections.
func (e *endpoint) handleSynSegment(ctx *listenContext, s *segment, mss uint16, sndWndScale int) {
	defer decSynRcvdCount()
	defer s.decRef()

	n, err := ctx.createEndpointAndPerformHandshake(s, mss, sndWndScale)
	if err != nil {
		return
	}

	e.deliverAccepted(n)
}

// handleListenSegment is called when a listening endpoint receives a segment
// and needs to handle it.
func (e *endpoint) handleListenSegment(ctx *listenContext, s *segment) {
	switch s.flags {
	case flagSyn:
		mss, sws, ok := parseSynOptions(s)
		if !ok {
			return
		}
		if incSynRcvdCount() {
			s.incRef()
			go e.handleSynSegment(ctx, s, mss, sws)
		} else {
			cookie := ctx.createCookie(s.id, s.sequenceNumber, encodeMSS(mss))
			// Send SYN with window scaling disabled because we
			// currently can't encode this information in the
			// cookie.
			sendSynTCP(&s.route, s.id, flagSyn|flagAck, cookie, s.sequenceNumber+1, ctx.rcvWnd, -1)
		}

	case flagAck:
		if data, ok := ctx.isCookieValid(s.id, s.ackNumber-1, s.sequenceNumber-1); ok && int(data) < len(mssTable) {
			// Create newly accepted endpoint and deliver it.
			n, err := ctx.createConnectedEndpoint(s, s.ackNumber-1, s.sequenceNumber-1, mssTable[data], -1)
			if err == nil {
				e.deliverAccepted(n)
			}
		}
	}
}

// protocolListenLoop is the main loop of a listening TCP endpoint. It runs in
// its own goroutine and is responsible for handling connection requests.
func (e *endpoint) protocolListenLoop(rcvWnd seqnum.Size) error {
	defer func() {
		// Mark endpoint as closed. This will prevent goroutines running
		// handleSynSegment() from attempting to queue new connections
		// to the endpoint.
		e.mu.Lock()
		e.state = stateClosed
		e.mu.Unlock()

		// Notify waiters that the endpoint is shutdown.
		e.waiterQueue.Notify(waiter.EventIn | waiter.EventOut)

		// Do cleanup if needed.
		e.completeWorker()
	}()

	e.mu.Lock()
	v6only := e.v6only
	e.mu.Unlock()

	ctx := newListenContext(e.stack, rcvWnd, v6only, e.netProto)

	for {
		select {
		case s := <-e.segmentChan:
			e.handleListenSegment(ctx, s)
			s.decRef()

		case <-e.notifyChan:
			n := e.fetchNotifications()
			if n&notifyClose != 0 {
				return nil
			}
		}
	}
}