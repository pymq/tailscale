// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package derp

import (
	"bufio"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/nacl/box"
	"golang.org/x/time/rate"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

// Client is a DERP client.
type Client struct {
	serverKey   key.Public // of the DERP server; not a machine or node key
	privateKey  key.Private
	publicKey   key.Public // of privateKey
	logf        logger.Logf
	nc          Conn
	br          *bufio.Reader
	meshKey     string
	canAckPings bool
	isProber    bool

	wmu  sync.Mutex // hold while writing to bw
	bw   *bufio.Writer
	rate *rate.Limiter // if non-nil, rate limiter to use

	// Owned by Recv:
	peeked  int   // bytes to discard on next Recv
	readErr error // sticky read error
}

// ClientOpt is an option passed to NewClient.
type ClientOpt interface {
	update(*clientOpt)
}

type clientOptFunc func(*clientOpt)

func (f clientOptFunc) update(o *clientOpt) { f(o) }

// clientOpt are the options passed to newClient.
type clientOpt struct {
	MeshKey     string
	ServerPub   key.Public
	CanAckPings bool
	IsProber    bool
}

// MeshKey returns a ClientOpt to pass to the DERP server during connect to get
// access to join the mesh.
//
// An empty key means to not use a mesh key.
func MeshKey(key string) ClientOpt { return clientOptFunc(func(o *clientOpt) { o.MeshKey = key }) }

// IsProber returns a ClientOpt to pass to the DERP server during connect to
// declare that this client is a a prober.
func IsProber(v bool) ClientOpt { return clientOptFunc(func(o *clientOpt) { o.IsProber = v }) }

// ServerPublicKey returns a ClientOpt to declare that the server's DERP public key is known.
// If key is the zero value, the returned ClientOpt is a no-op.
func ServerPublicKey(key key.Public) ClientOpt {
	return clientOptFunc(func(o *clientOpt) { o.ServerPub = key })
}

// CanAckPings returns a ClientOpt to set whether it advertises to the
// server that it's capable of acknowledging ping requests.
func CanAckPings(v bool) ClientOpt {
	return clientOptFunc(func(o *clientOpt) { o.CanAckPings = v })
}

func NewClient(privateKey key.Private, nc Conn, brw *bufio.ReadWriter, logf logger.Logf, opts ...ClientOpt) (*Client, error) {
	var opt clientOpt
	for _, o := range opts {
		if o == nil {
			return nil, errors.New("nil ClientOpt")
		}
		o.update(&opt)
	}
	return newClient(privateKey, nc, brw, logf, opt)
}

func newClient(privateKey key.Private, nc Conn, brw *bufio.ReadWriter, logf logger.Logf, opt clientOpt) (*Client, error) {
	c := &Client{
		privateKey:  privateKey,
		publicKey:   privateKey.Public(),
		logf:        logf,
		nc:          nc,
		br:          brw.Reader,
		bw:          brw.Writer,
		meshKey:     opt.MeshKey,
		canAckPings: opt.CanAckPings,
		isProber:    opt.IsProber,
	}
	if opt.ServerPub.IsZero() {
		if err := c.recvServerKey(); err != nil {
			return nil, fmt.Errorf("derp.Client: failed to receive server key: %v", err)
		}
	} else {
		c.serverKey = opt.ServerPub
	}
	if err := c.sendClientKey(); err != nil {
		return nil, fmt.Errorf("derp.Client: failed to send client key: %v", err)
	}
	return c, nil
}

func (c *Client) recvServerKey() error {
	var buf [40]byte
	t, flen, err := readFrame(c.br, 1<<10, buf[:])
	if err == io.ErrShortBuffer {
		// For future-proofing, allow server to send more in its greeting.
		err = nil
	}
	if err != nil {
		return err
	}
	if flen < uint32(len(buf)) || t != frameServerKey || string(buf[:len(magic)]) != magic {
		return errors.New("invalid server greeting")
	}
	copy(c.serverKey[:], buf[len(magic):])
	return nil
}

func (c *Client) parseServerInfo(b []byte) (*serverInfo, error) {
	const maxLength = nonceLen + maxInfoLen
	fl := len(b)
	if fl < nonceLen {
		return nil, fmt.Errorf("short serverInfo frame")
	}
	if fl > maxLength {
		return nil, fmt.Errorf("long serverInfo frame")
	}
	// TODO: add a read-nonce-and-box helper
	var nonce [nonceLen]byte
	copy(nonce[:], b)
	msgbox := b[nonceLen:]
	msg, ok := box.Open(nil, msgbox, &nonce, c.serverKey.B32(), c.privateKey.B32())
	if !ok {
		return nil, fmt.Errorf("failed to open naclbox from server key %x", c.serverKey[:])
	}
	info := new(serverInfo)
	if err := json.Unmarshal(msg, info); err != nil {
		return nil, fmt.Errorf("invalid JSON: %v", err)
	}
	return info, nil
}

type clientInfo struct {
	Version int `json:"version,omitempty"`

	// MeshKey optionally specifies a pre-shared key used by
	// trusted clients.  It's required to subscribe to the
	// connection list & forward packets. It's empty for regular
	// users.
	MeshKey string `json:"meshKey,omitempty"`

	// CanAckPings is whether the client declares it's able to ack
	// pings.
	CanAckPings bool

	// IsProber is whether this client is a prober.
	IsProber bool `json:",omitempty"`
}

func (c *Client) sendClientKey() error {
	var nonce [nonceLen]byte
	if _, err := crand.Read(nonce[:]); err != nil {
		return err
	}
	msg, err := json.Marshal(clientInfo{
		Version:     ProtocolVersion,
		MeshKey:     c.meshKey,
		CanAckPings: c.canAckPings,
		IsProber:    c.isProber,
	})
	if err != nil {
		return err
	}
	msgbox := box.Seal(nil, msg, &nonce, c.serverKey.B32(), c.privateKey.B32())

	buf := make([]byte, 0, nonceLen+keyLen+len(msgbox))
	buf = append(buf, c.publicKey[:]...)
	buf = append(buf, nonce[:]...)
	buf = append(buf, msgbox...)
	return writeFrame(c.bw, frameClientInfo, buf)
}

// ServerPublicKey returns the server's public key.
func (c *Client) ServerPublicKey() key.Public { return c.serverKey }

// Send sends a packet to the Tailscale node identified by dstKey.
//
// It is an error if the packet is larger than 64KB.
func (c *Client) Send(dstKey key.Public, pkt []byte) error { return c.send(dstKey, pkt) }

func (c *Client) send(dstKey key.Public, pkt []byte) (ret error) {
	defer func() {
		if ret != nil {
			ret = fmt.Errorf("derp.Send: %w", ret)
		}
	}()

	if len(pkt) > MaxPacketSize {
		return fmt.Errorf("packet too big: %d", len(pkt))
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.rate != nil {
		pktLen := frameHeaderLen + len(dstKey) + len(pkt)
		if !c.rate.AllowN(time.Now(), pktLen) {
			return nil // drop
		}
	}
	if err := writeFrameHeader(c.bw, frameSendPacket, uint32(len(dstKey)+len(pkt))); err != nil {
		return err
	}
	if _, err := c.bw.Write(dstKey[:]); err != nil {
		return err
	}
	if _, err := c.bw.Write(pkt); err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *Client) ForwardPacket(srcKey, dstKey key.Public, pkt []byte) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("derp.ForwardPacket: %w", err)
		}
	}()

	if len(pkt) > MaxPacketSize {
		return fmt.Errorf("packet too big: %d", len(pkt))
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	timer := time.AfterFunc(5*time.Second, c.writeTimeoutFired)
	defer timer.Stop()

	if err := writeFrameHeader(c.bw, frameForwardPacket, uint32(keyLen*2+len(pkt))); err != nil {
		return err
	}
	if _, err := c.bw.Write(srcKey[:]); err != nil {
		return err
	}
	if _, err := c.bw.Write(dstKey[:]); err != nil {
		return err
	}
	if _, err := c.bw.Write(pkt); err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *Client) writeTimeoutFired() { c.nc.Close() }

func (c *Client) SendPong(data [8]byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := writeFrameHeader(c.bw, framePong, 8); err != nil {
		return err
	}
	if _, err := c.bw.Write(data[:]); err != nil {
		return err
	}
	return c.bw.Flush()
}

// NotePreferred sends a packet that tells the server whether this
// client is the user's preferred server. This is only used in the
// server for stats.
func (c *Client) NotePreferred(preferred bool) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("derp.NotePreferred: %v", err)
		}
	}()

	c.wmu.Lock()
	defer c.wmu.Unlock()

	if err := writeFrameHeader(c.bw, frameNotePreferred, 1); err != nil {
		return err
	}
	var b byte = 0x00
	if preferred {
		b = 0x01
	}
	if err := c.bw.WriteByte(b); err != nil {
		return err
	}
	return c.bw.Flush()
}

// WatchConnectionChanges sends a request to subscribe to the peer's connection list.
// It's a fatal error if the client wasn't created using MeshKey.
func (c *Client) WatchConnectionChanges() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := writeFrameHeader(c.bw, frameWatchConns, 0); err != nil {
		return err
	}
	return c.bw.Flush()
}

// ClosePeer asks the server to close target's TCP connection.
// It's a fatal error if the client wasn't created using MeshKey.
func (c *Client) ClosePeer(target key.Public) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c.bw, frameClosePeer, target[:])
}

// ReceivedMessage represents a type returned by Client.Recv. Unless
// otherwise documented, the returned message aliases the byte slice
// provided to Recv and thus the message is only as good as that
// buffer, which is up to the caller.
type ReceivedMessage interface {
	msg()
}

// ReceivedPacket is a ReceivedMessage representing an incoming packet.
type ReceivedPacket struct {
	Source key.Public
	// Data is the received packet bytes. It aliases the memory
	// passed to Client.Recv.
	Data []byte
}

func (ReceivedPacket) msg() {}

// PeerGoneMessage is a ReceivedMessage that indicates that the client
// identified by the underlying public key had previously sent you a
// packet but has now disconnected from the server.
type PeerGoneMessage key.Public

func (PeerGoneMessage) msg() {}

// PeerPresentMessage is a ReceivedMessage that indicates that the client
// is connected to the server. (Only used by trusted mesh clients)
type PeerPresentMessage key.Public

func (PeerPresentMessage) msg() {}

// ServerInfoMessage is sent by the server upon first connect.
type ServerInfoMessage struct {
	// TokenBucketBytesPerSecond is how many bytes per second the
	// server says it will accept, including all framing bytes.
	//
	// Zero means unspecified. There might be a limit, but the
	// client need not try to respect it.
	TokenBucketBytesPerSecond int

	// TokenBucketBytesBurst is how many bytes the server will
	// allow to burst, temporarily violating
	// TokenBucketBytesPerSecond.
	//
	// Zero means unspecified. There might be a limit, but the
	// client need not try to respect it.
	TokenBucketBytesBurst int
}

func (ServerInfoMessage) msg() {}

// PingMessage is a request from a client or server to reply to the
// other side with a PongMessage with the given payload.
type PingMessage [8]byte

func (PingMessage) msg() {}

// KeepAliveMessage is a one-way empty message from server to client, just to
// keep the connection alive. It's like a PingMessage, but doesn't solicit
// a reply from the client.
type KeepAliveMessage struct{}

func (KeepAliveMessage) msg() {}

// HealthMessage is a one-way message from server to client, declaring the
// connection health state.
type HealthMessage struct {
	// Problem, if non-empty, is a description of why the connection
	// is unhealthy.
	//
	// The empty string means the connection is healthy again.
	//
	// The default condition is healthy, so the server doesn't
	// broadcast a HealthMessage until a problem exists.
	Problem string
}

func (HealthMessage) msg() {}

// ServerRestartingMessage is a one-way message from server to client,
// advertising that the server is restarting.
type ServerRestartingMessage struct {
	// ReconnectIn is an advisory duration that the client should wait
	// before attempting to reconnect. It might be zero.
	// It exists for the server to smear out the reconnects.
	ReconnectIn time.Duration

	// TryFor is an advisory duration for how long the client
	// should attempt to reconnect before giving up and proceeding
	// with its normal connection failure logic. The interval
	// between retries is undefined for now.
	// A server should not send a TryFor duration more than a few
	// seconds.
	TryFor time.Duration
}

func (ServerRestartingMessage) msg() {}

// Recv reads a message from the DERP server.
//
// The returned message may alias memory owned by the Client; it
// should only be accessed until the next call to Client.
//
// Once Recv returns an error, the Client is dead forever.
func (c *Client) Recv() (m ReceivedMessage, err error) {
	return c.recvTimeout(120 * time.Second)
}

func (c *Client) recvTimeout(timeout time.Duration) (m ReceivedMessage, err error) {
	if c.readErr != nil {
		return nil, c.readErr
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("derp.Recv: %w", err)
			c.readErr = err
		}
	}()

	for {
		c.nc.SetReadDeadline(time.Now().Add(timeout))

		// Discard any peeked bytes from a previous Recv call.
		if c.peeked != 0 {
			if n, err := c.br.Discard(c.peeked); err != nil || n != c.peeked {
				// Documented to never fail, but might as well check.
				return nil, fmt.Errorf("bufio.Reader.Discard(%d bytes): got %v, %v", c.peeked, n, err)
			}
			c.peeked = 0
		}

		t, n, err := readFrameHeader(c.br)
		if err != nil {
			return nil, err
		}
		if n > 1<<20 {
			return nil, fmt.Errorf("unexpectedly large frame of %d bytes returned", n)
		}

		var b []byte // frame payload (past the 5 byte header)

		// If the frame fits in our bufio.Reader buffer, just use it.
		// In practice it's 4KB (from derphttp.Client's bufio.NewReader(httpConn)) and
		// in practive, WireGuard packets (and thus DERP frames) are under 1.5KB.
		// So this is the common path.
		if int(n) <= c.br.Size() {
			b, err = c.br.Peek(int(n))
			c.peeked = int(n)
		} else {
			// But if for some reason we read a large DERP message (which isn't necessarily
			// a Wireguard packet), then just allocate memory for it.
			// TODO(bradfitz): use a pool if large frames ever happen in practice.
			b = make([]byte, n)
			_, err = io.ReadFull(c.br, b)
		}
		if err != nil {
			return nil, err
		}

		switch t {
		default:
			continue
		case frameServerInfo:
			// Server sends this at start-up. Currently unused.
			// Just has a JSON message saying "version: 2",
			// but the protocol seems extensible enough as-is without
			// needing to wait an RTT to discover the version at startup.
			// We'd prefer to give the connection to the client (magicsock)
			// to start writing as soon as possible.
			si, err := c.parseServerInfo(b)
			if err != nil {
				return nil, fmt.Errorf("invalid server info frame: %v", err)
			}
			sm := ServerInfoMessage{
				TokenBucketBytesPerSecond: si.TokenBucketBytesPerSecond,
				TokenBucketBytesBurst:     si.TokenBucketBytesBurst,
			}
			c.setSendRateLimiter(sm)
			return sm, nil
		case frameKeepAlive:
			// A one-way keep-alive message that doesn't require an acknowledgement.
			// This predated framePing/framePong.
			return KeepAliveMessage{}, nil
		case framePeerGone:
			if n < keyLen {
				c.logf("[unexpected] dropping short peerGone frame from DERP server")
				continue
			}
			var pg PeerGoneMessage
			copy(pg[:], b[:keyLen])
			return pg, nil

		case framePeerPresent:
			if n < keyLen {
				c.logf("[unexpected] dropping short peerPresent frame from DERP server")
				continue
			}
			var pg PeerPresentMessage
			copy(pg[:], b[:keyLen])
			return pg, nil

		case frameRecvPacket:
			var rp ReceivedPacket
			if n < keyLen {
				c.logf("[unexpected] dropping short packet from DERP server")
				continue
			}
			copy(rp.Source[:], b[:keyLen])
			rp.Data = b[keyLen:n]
			return rp, nil

		case framePing:
			var pm PingMessage
			if n < 8 {
				c.logf("[unexpected] dropping short ping frame")
				continue
			}
			copy(pm[:], b[:])
			return pm, nil

		case frameHealth:
			return HealthMessage{Problem: string(b[:])}, nil

		case frameRestarting:
			var m ServerRestartingMessage
			if n < 8 {
				c.logf("[unexpected] dropping short server restarting frame")
				continue
			}
			m.ReconnectIn = time.Duration(binary.BigEndian.Uint32(b[0:4])) * time.Millisecond
			m.TryFor = time.Duration(binary.BigEndian.Uint32(b[4:8])) * time.Millisecond
			return m, nil
		}
	}
}

func (c *Client) setSendRateLimiter(sm ServerInfoMessage) {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	c.rate = nil
	if sm.TokenBucketBytesPerSecond == 0 {
		return
	}
	c.rate = rate.NewLimiter(rate.Every(time.Second/time.Duration(sm.TokenBucketBytesPerSecond)),
		sm.TokenBucketBytesBurst)
}
