// Copyright (c) 2019 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package magicsock implements a socket that can change its communication path while
// in use, actively searching for the best way to communicate.
package magicsock

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/nacl/box"
	"golang.zx2c4.com/wireguard/conn"
	"inet.af/netaddr"
	"tailscale.com/control/controlclient"
	"tailscale.com/derp"
	"tailscale.com/derp/derphttp"
	"tailscale.com/disco"
	"tailscale.com/health"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/logtail/backoff"
	"tailscale.com/net/dnscache"
	"tailscale.com/net/interfaces"
	"tailscale.com/net/netcheck"
	"tailscale.com/net/netns"
	"tailscale.com/net/portmapper"
	"tailscale.com/net/stun"
	"tailscale.com/syncs"
	"tailscale.com/tailcfg"
	"tailscale.com/tstime"
	"tailscale.com/tstime/mono"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/types/nettype"
	"tailscale.com/types/wgkey"
	"tailscale.com/util/uniq"
	"tailscale.com/version"
	"tailscale.com/wgengine/monitor"
)

// useDerpRoute reports whether magicsock should enable the DERP
// return path optimization (Issue 150).
func useDerpRoute() bool {
	if debugUseDerpRouteEnv != "" {
		return debugUseDerpRoute
	}
	ob := controlclient.DERPRouteFlag()
	if v, ok := ob.Get(); ok {
		return v
	}
	return false
}

// peerInfo is all the information magicsock tracks about a particular
// peer.
type peerInfo struct {
	ep *endpoint // optional, if wireguard-go isn't currently talking to this peer.
	// ipPorts is an inverted version of peerMap.byIPPort (below), so
	// that when we're deleting this node, we can rapidly find out the
	// keys that need deleting from peerMap.byIPPort without having to
	// iterate over every IPPort known for any peer.
	ipPorts map[netaddr.IPPort]bool
}

func newPeerInfo() *peerInfo {
	return &peerInfo{
		ipPorts: map[netaddr.IPPort]bool{},
	}
}

// peerMap is an index of peerInfos by node (WireGuard) key, disco
// key, and discovered ip:port endpoints.
//
// Doesn't do any locking, all access must be done with Conn.mu held.
type peerMap struct {
	byDiscoKey map[tailcfg.DiscoKey]*peerInfo
	byNodeKey  map[tailcfg.NodeKey]*peerInfo
	byIPPort   map[netaddr.IPPort]*peerInfo
}

func newPeerMap() peerMap {
	return peerMap{
		byDiscoKey: map[tailcfg.DiscoKey]*peerInfo{},
		byNodeKey:  map[tailcfg.NodeKey]*peerInfo{},
		byIPPort:   map[netaddr.IPPort]*peerInfo{},
	}
}

// nodeCount returns the number of nodes currently in m.
func (m *peerMap) nodeCount() int {
	return len(m.byNodeKey)
}

// endpointForDiscoKey returns the endpoint for dk, or nil
// if dk is not known to us.
func (m *peerMap) endpointForDiscoKey(dk tailcfg.DiscoKey) (ep *endpoint, ok bool) {
	if dk.IsZero() {
		return nil, false
	}
	if info, ok := m.byDiscoKey[dk]; ok && info.ep != nil {
		return info.ep, true
	}
	return nil, false
}

// endpointForNodeKey returns the endpoint for nk, or nil if
// nk is not known to us.
func (m *peerMap) endpointForNodeKey(nk tailcfg.NodeKey) (ep *endpoint, ok bool) {
	if nk.IsZero() {
		return nil, false
	}
	if info, ok := m.byNodeKey[nk]; ok && info.ep != nil {
		return info.ep, true
	}
	return nil, false
}

// endpointForIPPort returns the endpoint for the peer we
// believe to be at ipp, or nil if we don't know of any such peer.
func (m *peerMap) endpointForIPPort(ipp netaddr.IPPort) (ep *endpoint, ok bool) {
	if info, ok := m.byIPPort[ipp]; ok && info.ep != nil {
		return info.ep, true
	}
	return nil, false
}

// forEachDiscoEndpoint invokes f on every endpoint in m.
func (m *peerMap) forEachDiscoEndpoint(f func(ep *endpoint)) {
	for _, pi := range m.byNodeKey {
		if pi.ep != nil {
			f(pi.ep)
		}
	}
}

// upsertDiscoEndpoint stores endpoint in the peerInfo for
// ep.publicKey, and updates indexes. m must already have a
// tailcfg.Node for ep.publicKey.
func (m *peerMap) upsertDiscoEndpoint(ep *endpoint) {
	pi := m.byNodeKey[ep.publicKey]
	if pi == nil {
		pi = newPeerInfo()
		m.byNodeKey[ep.publicKey] = pi
	}
	old := pi.ep
	pi.ep = ep
	if old != nil && old.discoKey != ep.discoKey {
		delete(m.byDiscoKey, old.discoKey)
	}
	m.byDiscoKey[ep.discoKey] = pi
}

// SetDiscoKeyForIPPort makes future peer lookups by ipp return the
// same peer info as the lookup by dk.
func (m *peerMap) setDiscoKeyForIPPort(ipp netaddr.IPPort, dk tailcfg.DiscoKey) {
	// Check for a prior mapping for ipp, may need to clean it up.
	if pi := m.byIPPort[ipp]; pi != nil {
		delete(pi.ipPorts, ipp)
		delete(m.byIPPort, ipp)
	}
	if pi, ok := m.byDiscoKey[dk]; ok {
		pi.ipPorts[ipp] = true
		m.byIPPort[ipp] = pi
	}
}

// deleteDiscoEndpoint deletes the peerInfo associated with ep, and
// updates indexes.
func (m *peerMap) deleteDiscoEndpoint(ep *endpoint) {
	if ep == nil {
		return
	}
	ep.stopAndReset()
	pi := m.byNodeKey[ep.publicKey]
	delete(m.byDiscoKey, ep.discoKey)
	delete(m.byNodeKey, ep.publicKey)
	if pi == nil {
		// Kneejerk paranoia from earlier issue 2801.
		// Unexpected. But no logger plumbed here to log so.
		return
	}
	for ip := range pi.ipPorts {
		delete(m.byIPPort, ip)
	}
}

// A Conn routes UDP packets and actively manages a list of its endpoints.
// It implements wireguard/conn.Bind.
type Conn struct {
	// This block mirrors the contents and field order of the Options
	// struct. Initialized once at construction, then constant.

	logf                   logger.Logf
	epFunc                 func([]tailcfg.Endpoint)
	derpActiveFunc         func()
	idleFunc               func() time.Duration // nil means unknown
	testOnlyPacketListener nettype.PacketListener
	noteRecvActivity       func(tailcfg.NodeKey) // or nil, see Options.NoteRecvActivity

	// ================================================================
	// No locking required to access these fields, either because
	// they're static after construction, or are wholly owned by a
	// single goroutine.

	connCtx       context.Context // closed on Conn.Close
	connCtxCancel func()          // closes connCtx
	donec         <-chan struct{} // connCtx.Done()'s to avoid context.cancelCtx.Done()'s mutex per call

	// pconn4 and pconn6 are the underlying UDP sockets used to
	// send/receive packets for wireguard and other magicsock
	// protocols.
	pconn4 *RebindingUDPConn
	pconn6 *RebindingUDPConn

	// netChecker is the prober that discovers local network
	// conditions, including the closest DERP relay and NAT mappings.
	netChecker *netcheck.Client

	// portMapper is the NAT-PMP/PCP/UPnP prober/client, for requesting
	// port mappings from NAT devices.
	portMapper *portmapper.Client

	// stunReceiveFunc holds the current STUN packet processing func.
	// Its Loaded value is always non-nil.
	stunReceiveFunc atomic.Value // of func(p []byte, fromAddr *net.UDPAddr)

	// derpRecvCh is used by receiveDERP to read DERP messages.
	derpRecvCh chan derpReadResult

	// bind is the wireguard-go conn.Bind for Conn.
	bind *connBind

	// ippEndpoint4 and ippEndpoint6 are owned by receiveIPv4 and
	// receiveIPv6, respectively, to cache an IPPort->endpoint for
	// hot flows.
	ippEndpoint4, ippEndpoint6 ippEndpointCache

	// ============================================================
	// Fields that must be accessed via atomic load/stores.

	// noV4 and noV6 are whether IPv4 and IPv6 are known to be
	// missing.  They're only used to suppress log spam. The name
	// is named negatively because in early start-up, we don't yet
	// necessarily have a netcheck.Report and don't want to skip
	// logging.
	noV4, noV6 syncs.AtomicBool

	// networkUp is whether the network is up (some interface is up
	// with IPv4 or IPv6). It's used to suppress log spam and prevent
	// new connection that'll fail.
	networkUp syncs.AtomicBool

	// havePrivateKey is whether privateKey is non-zero.
	havePrivateKey syncs.AtomicBool

	// port is the preferred port from opts.Port; 0 means auto.
	port syncs.AtomicUint32

	// ============================================================
	// mu guards all following fields; see userspaceEngine lock ordering rules
	mu     sync.Mutex
	muCond *sync.Cond

	closed bool // Close was called

	// derpCleanupTimer is the timer that fires to occasionally clean
	// up idle DERP connections. It's only used when there is a non-home
	// DERP connection in use.
	derpCleanupTimer *time.Timer

	// derpCleanupTimerArmed is whether derpCleanupTimer is
	// scheduled to fire within derpCleanStaleInterval.
	derpCleanupTimerArmed bool

	// periodicReSTUNTimer, when non-nil, is an AfterFunc timer
	// that will call Conn.doPeriodicSTUN.
	periodicReSTUNTimer *time.Timer

	// endpointsUpdateActive indicates that updateEndpoints is
	// currently running. It's used to deduplicate concurrent endpoint
	// update requests.
	endpointsUpdateActive bool
	// wantEndpointsUpdate, if non-empty, means that a new endpoints
	// update should begin immediately after the currently-running one
	// completes. It can only be non-empty if
	// endpointsUpdateActive==true.
	wantEndpointsUpdate string // true if non-empty; string is reason
	// lastEndpoints records the endpoints found during the previous
	// endpoint discovery. It's used to avoid duplicate endpoint
	// change notifications.
	lastEndpoints []tailcfg.Endpoint

	// lastEndpointsTime is the last time the endpoints were updated,
	// even if there was no change.
	lastEndpointsTime time.Time

	// onEndpointRefreshed are funcs to run (in their own goroutines)
	// when endpoints are refreshed.
	onEndpointRefreshed map[*endpoint]func()

	// peerSet is the set of peers that are currently configured in
	// WireGuard. These are not used to filter inbound or outbound
	// traffic at all, but only to track what state can be cleaned up
	// in other maps below that are keyed by peer public key.
	peerSet map[key.Public]struct{}

	// discoPrivate is the private naclbox key used for active
	// discovery traffic. It's created once near (but not during)
	// construction.
	discoPrivate key.Private
	discoPublic  tailcfg.DiscoKey // public of discoPrivate
	discoShort   string           // ShortString of discoPublic (to save logging work later)
	// nodeOfDisco tracks the networkmap Node entity for each peer
	// discovery key.
	peerMap peerMap
	// sharedDiscoKey is the precomputed nacl/box key for
	// communication with the peer that has the given DiscoKey.
	sharedDiscoKey map[tailcfg.DiscoKey]*[32]byte

	// netInfoFunc is a callback that provides a tailcfg.NetInfo when
	// discovered network conditions change.
	//
	// TODO(danderson): why can't it be set at construction time?
	// There seem to be a few natural places in ipn/local.go to
	// swallow untimely invocations.
	netInfoFunc func(*tailcfg.NetInfo) // nil until set
	// netInfoLast is the NetInfo provided in the last call to
	// netInfoFunc. It's used to deduplicate calls to netInfoFunc.
	//
	// TODO(danderson): should all the deduping happen in
	// ipn/local.go? We seem to be doing dedupe at several layers, and
	// magicsock could do with any complexity reduction it can get.
	netInfoLast *tailcfg.NetInfo

	derpMap     *tailcfg.DERPMap // nil (or zero regions/nodes) means DERP is disabled
	netMap      *netmap.NetworkMap
	privateKey  key.Private        // WireGuard private key for this node
	everHadKey  bool               // whether we ever had a non-zero private key
	myDerp      int                // nearest DERP region ID; 0 means none/unknown
	derpStarted chan struct{}      // closed on first connection to DERP; for tests & cleaner Close
	activeDerp  map[int]activeDerp // DERP regionID -> connection to a node in that region
	prevDerp    map[int]*syncs.WaitGroupChan

	// derpRoute contains optional alternate routes to use as an
	// optimization instead of contacting a peer via their home
	// DERP connection.  If they sent us a message on a different
	// DERP connection (which should really only be on our DERP
	// home connection, or what was once our home), then we
	// remember that route here to optimistically use instead of
	// creating a new DERP connection back to their home.
	derpRoute map[key.Public]derpRoute

	// peerLastDerp tracks which DERP node we last used to speak with a
	// peer. It's only used to quiet logging, so we only log on change.
	peerLastDerp map[key.Public]int
}

// derpRoute is a route entry for a public key, saying that a certain
// peer should be available at DERP node derpID, as long as the
// current connection for that derpID is dc. (but dc should not be
// used to write directly; it's owned by the read/write loops)
type derpRoute struct {
	derpID int
	dc     *derphttp.Client // don't use directly; see comment above
}

// removeDerpPeerRoute removes a DERP route entry previously added by addDerpPeerRoute.
func (c *Conn) removeDerpPeerRoute(peer key.Public, derpID int, dc *derphttp.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r2 := derpRoute{derpID, dc}
	if r, ok := c.derpRoute[peer]; ok && r == r2 {
		delete(c.derpRoute, peer)
	}
}

// addDerpPeerRoute adds a DERP route entry, noting that peer was seen
// on DERP node derpID, at least on the connection identified by dc.
// See issue 150 for details.
func (c *Conn) addDerpPeerRoute(peer key.Public, derpID int, dc *derphttp.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.derpRoute == nil {
		c.derpRoute = make(map[key.Public]derpRoute)
	}
	r := derpRoute{derpID, dc}
	c.derpRoute[peer] = r
}

// DerpMagicIP is a fake WireGuard endpoint IP address that means
// to use DERP. When used, the port number of the WireGuard endpoint
// is the DERP server number to use.
//
// Mnemonic: 3.3.40 are numbers above the keys D, E, R, P.
const DerpMagicIP = "127.3.3.40"

var derpMagicIPAddr = netaddr.MustParseIP(DerpMagicIP)

// activeDerp contains fields for an active DERP connection.
type activeDerp struct {
	c       *derphttp.Client
	cancel  context.CancelFunc
	writeCh chan<- derpWriteRequest
	// lastWrite is the time of the last request for its write
	// channel (currently even if there was no write).
	// It is always non-nil and initialized to a non-zero Time.
	lastWrite  *time.Time
	createTime time.Time
}

// Options contains options for Listen.
type Options struct {
	// Logf optionally provides a log function to use.
	// Must not be nil.
	Logf logger.Logf

	// Port is the port to listen on.
	// Zero means to pick one automatically.
	Port uint16

	// EndpointsFunc optionally provides a func to be called when
	// endpoints change. The called func does not own the slice.
	EndpointsFunc func([]tailcfg.Endpoint)

	// DERPActiveFunc optionally provides a func to be called when
	// a connection is made to a DERP server.
	DERPActiveFunc func()

	// IdleFunc optionally provides a func to return how long
	// it's been since a TUN packet was sent or received.
	IdleFunc func() time.Duration

	// TestOnlyPacketListener optionally specifies how to create PacketConns.
	// Only used by tests.
	TestOnlyPacketListener nettype.PacketListener

	// NoteRecvActivity, if provided, is a func for magicsock to call
	// whenever it receives a packet from a a peer if it's been more
	// than ~10 seconds since the last one. (10 seconds is somewhat
	// arbitrary; the sole user just doesn't need or want it called on
	// every packet, just every minute or two for Wireguard timeouts,
	// and 10 seconds seems like a good trade-off between often enough
	// and not too often.)
	// The provided func is likely to call back into
	// Conn.ParseEndpoint, which acquires Conn.mu. As such, you should
	// not hold Conn.mu while calling it.
	NoteRecvActivity func(tailcfg.NodeKey)

	// LinkMonitor is the link monitor to use.
	// With one, the portmapper won't be used.
	LinkMonitor *monitor.Mon
}

func (o *Options) logf() logger.Logf {
	if o.Logf == nil {
		panic("must provide magicsock.Options.logf")
	}
	return o.Logf
}

func (o *Options) endpointsFunc() func([]tailcfg.Endpoint) {
	if o == nil || o.EndpointsFunc == nil {
		return func([]tailcfg.Endpoint) {}
	}
	return o.EndpointsFunc
}

func (o *Options) derpActiveFunc() func() {
	if o == nil || o.DERPActiveFunc == nil {
		return func() {}
	}
	return o.DERPActiveFunc
}

// newConn is the error-free, network-listening-side-effect-free based
// of NewConn. Mostly for tests.
func newConn() *Conn {
	c := &Conn{
		derpRecvCh:     make(chan derpReadResult),
		derpStarted:    make(chan struct{}),
		peerLastDerp:   make(map[key.Public]int),
		peerMap:        newPeerMap(),
		sharedDiscoKey: make(map[tailcfg.DiscoKey]*[32]byte),
	}
	c.bind = &connBind{Conn: c, closed: true}
	c.muCond = sync.NewCond(&c.mu)
	c.networkUp.Set(true) // assume up until told otherwise
	return c
}

// NewConn creates a magic Conn listening on opts.Port.
// As the set of possible endpoints for a Conn changes, the
// callback opts.EndpointsFunc is called.
//
// It doesn't start doing anything until Start is called.
func NewConn(opts Options) (*Conn, error) {
	c := newConn()
	c.port.Set(uint32(opts.Port))
	c.logf = opts.logf()
	c.epFunc = opts.endpointsFunc()
	c.derpActiveFunc = opts.derpActiveFunc()
	c.idleFunc = opts.IdleFunc
	c.testOnlyPacketListener = opts.TestOnlyPacketListener
	c.noteRecvActivity = opts.NoteRecvActivity
	c.portMapper = portmapper.NewClient(logger.WithPrefix(c.logf, "portmapper: "), c.onPortMapChanged)
	if opts.LinkMonitor != nil {
		c.portMapper.SetGatewayLookupFunc(opts.LinkMonitor.GatewayAndSelfIP)
	}

	if err := c.initialBind(); err != nil {
		return nil, err
	}

	c.connCtx, c.connCtxCancel = context.WithCancel(context.Background())
	c.donec = c.connCtx.Done()
	c.netChecker = &netcheck.Client{
		Logf:                logger.WithPrefix(c.logf, "netcheck: "),
		GetSTUNConn4:        func() netcheck.STUNConn { return c.pconn4 },
		SkipExternalNetwork: inTest(),
		PortMapper:          c.portMapper,
	}

	if c.pconn6 != nil {
		c.netChecker.GetSTUNConn6 = func() netcheck.STUNConn { return c.pconn6 }
	}

	c.ignoreSTUNPackets()

	return c, nil
}

// ignoreSTUNPackets sets a STUN packet processing func that does nothing.
func (c *Conn) ignoreSTUNPackets() {
	c.stunReceiveFunc.Store(func([]byte, netaddr.IPPort) {})
}

// doPeriodicSTUN is called (in a new goroutine) by
// periodicReSTUNTimer when periodic STUNs are active.
func (c *Conn) doPeriodicSTUN() { c.ReSTUN("periodic") }

func (c *Conn) stopPeriodicReSTUNTimerLocked() {
	if t := c.periodicReSTUNTimer; t != nil {
		t.Stop()
		c.periodicReSTUNTimer = nil
	}
}

// c.mu must NOT be held.
func (c *Conn) updateEndpoints(why string) {
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		why := c.wantEndpointsUpdate
		c.wantEndpointsUpdate = ""
		if !c.closed {
			if why != "" {
				go c.updateEndpoints(why)
				return
			}
			if c.shouldDoPeriodicReSTUNLocked() {
				// Pick a random duration between 20
				// and 26 seconds (just under 30s, a
				// common UDP NAT timeout on Linux,
				// etc)
				d := tstime.RandomDurationBetween(20*time.Second, 26*time.Second)
				if t := c.periodicReSTUNTimer; t != nil {
					if debugReSTUNStopOnIdle {
						c.logf("resetting existing periodicSTUN to run in %v", d)
					}
					t.Reset(d)
				} else {
					if debugReSTUNStopOnIdle {
						c.logf("scheduling periodicSTUN to run in %v", d)
					}
					c.periodicReSTUNTimer = time.AfterFunc(d, c.doPeriodicSTUN)
				}
			} else {
				if debugReSTUNStopOnIdle {
					c.logf("periodic STUN idle")
				}
				c.stopPeriodicReSTUNTimerLocked()
			}
		}
		c.endpointsUpdateActive = false
		c.muCond.Broadcast()
	}()
	c.logf("[v1] magicsock: starting endpoint update (%s)", why)

	endpoints, err := c.determineEndpoints(c.connCtx)
	if err != nil {
		c.logf("magicsock: endpoint update (%s) failed: %v", why, err)
		// TODO(crawshaw): are there any conditions under which
		// we should trigger a retry based on the error here?
		return
	}

	if c.setEndpoints(endpoints) {
		c.logEndpointChange(endpoints)
		c.epFunc(endpoints)
	}
}

// setEndpoints records the new endpoints, reporting whether they're changed.
// It takes ownership of the slice.
func (c *Conn) setEndpoints(endpoints []tailcfg.Endpoint) (changed bool) {
	anySTUN := false
	for _, ep := range endpoints {
		if ep.Type == tailcfg.EndpointSTUN {
			anySTUN = true
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !anySTUN && c.derpMap == nil && !inTest() {
		// Don't bother storing or reporting this yet. We
		// don't have a DERP map or any STUN entries, so we're
		// just starting up. A DERP map should arrive shortly
		// and then we'll have more interesting endpoints to
		// report. This saves a map update.
		// TODO(bradfitz): this optimization is currently
		// skipped during the e2e tests because they depend
		// too much on the exact sequence of updates.  Fix the
		// tests. But a protocol rewrite might happen first.
		c.logf("[v1] magicsock: ignoring pre-DERP map, STUN-less endpoint update: %v", endpoints)
		return false
	}

	c.lastEndpointsTime = time.Now()
	for de, fn := range c.onEndpointRefreshed {
		go fn()
		delete(c.onEndpointRefreshed, de)
	}

	if endpointSetsEqual(endpoints, c.lastEndpoints) {
		return false
	}
	c.lastEndpoints = endpoints
	return true
}

// setNetInfoHavePortMap updates NetInfo.HavePortMap to true.
func (c *Conn) setNetInfoHavePortMap() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.netInfoLast == nil {
		// No NetInfo yet. Nothing to update.
		return
	}
	if c.netInfoLast.HavePortMap {
		// No change.
		return
	}
	ni := c.netInfoLast.Clone()
	ni.HavePortMap = true
	c.callNetInfoCallbackLocked(ni)
}

func (c *Conn) updateNetInfo(ctx context.Context) (*netcheck.Report, error) {
	c.mu.Lock()
	dm := c.derpMap
	c.mu.Unlock()

	if dm == nil || c.networkDown() {
		return new(netcheck.Report), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	c.stunReceiveFunc.Store(c.netChecker.ReceiveSTUNPacket)
	defer c.ignoreSTUNPackets()

	report, err := c.netChecker.GetReport(ctx, dm)
	if err != nil {
		return nil, err
	}

	c.noV4.Set(!report.IPv4)
	c.noV6.Set(!report.IPv6)

	ni := &tailcfg.NetInfo{
		DERPLatency:           map[string]float64{},
		MappingVariesByDestIP: report.MappingVariesByDestIP,
		HairPinning:           report.HairPinning,
		UPnP:                  report.UPnP,
		PMP:                   report.PMP,
		PCP:                   report.PCP,
		HavePortMap:           c.portMapper.HaveMapping(),
	}
	for rid, d := range report.RegionV4Latency {
		ni.DERPLatency[fmt.Sprintf("%d-v4", rid)] = d.Seconds()
	}
	for rid, d := range report.RegionV6Latency {
		ni.DERPLatency[fmt.Sprintf("%d-v6", rid)] = d.Seconds()
	}
	ni.WorkingIPv6.Set(report.IPv6)
	ni.WorkingUDP.Set(report.UDP)
	ni.PreferredDERP = report.PreferredDERP

	if ni.PreferredDERP == 0 {
		// Perhaps UDP is blocked. Pick a deterministic but arbitrary
		// one.
		ni.PreferredDERP = c.pickDERPFallback()
	}
	if !c.setNearestDERP(ni.PreferredDERP) {
		ni.PreferredDERP = 0
	}

	// TODO: set link type

	c.callNetInfoCallback(ni)
	return report, nil
}

var processStartUnixNano = time.Now().UnixNano()

// pickDERPFallback returns a non-zero but deterministic DERP node to
// connect to.  This is only used if netcheck couldn't find the
// nearest one (for instance, if UDP is blocked and thus STUN latency
// checks aren't working).
//
// c.mu must NOT be held.
func (c *Conn) pickDERPFallback() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.wantDerpLocked() {
		return 0
	}
	ids := c.derpMap.RegionIDs()
	if len(ids) == 0 {
		// No DERP regions in non-nil map.
		return 0
	}

	// TODO: figure out which DERP region most of our peers are using,
	// and use that region as our fallback.
	//
	// If we already had selected something in the past and it has any
	// peers, we want to stay on it. If there are no peers at all,
	// stay on whatever DERP we previously picked. If we need to pick
	// one and have no peer info, pick a region randomly.
	//
	// We used to do the above for legacy clients, but never updated
	// it for disco.

	if c.myDerp != 0 {
		return c.myDerp
	}

	h := fnv.New64()
	h.Write([]byte(fmt.Sprintf("%p/%d", c, processStartUnixNano))) // arbitrary
	return ids[rand.New(rand.NewSource(int64(h.Sum64()))).Intn(len(ids))]
}

// callNetInfoCallback calls the NetInfo callback (if previously
// registered with SetNetInfoCallback) if ni has substantially changed
// since the last state.
//
// callNetInfoCallback takes ownership of ni.
//
// c.mu must NOT be held.
func (c *Conn) callNetInfoCallback(ni *tailcfg.NetInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ni.BasicallyEqual(c.netInfoLast) {
		return
	}
	c.callNetInfoCallbackLocked(ni)
}

func (c *Conn) callNetInfoCallbackLocked(ni *tailcfg.NetInfo) {
	c.netInfoLast = ni
	if c.netInfoFunc != nil {
		c.logf("[v1] magicsock: netInfo update: %+v", ni)
		go c.netInfoFunc(ni)
	}
}

// addValidDiscoPathForTest makes addr a validated disco address for
// discoKey. It's used in tests to enable receiving of packets from
// addr without having to spin up the entire active discovery
// machinery.
func (c *Conn) addValidDiscoPathForTest(discoKey tailcfg.DiscoKey, addr netaddr.IPPort) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peerMap.setDiscoKeyForIPPort(addr, discoKey)
}

func (c *Conn) SetNetInfoCallback(fn func(*tailcfg.NetInfo)) {
	if fn == nil {
		panic("nil NetInfoCallback")
	}
	c.mu.Lock()
	last := c.netInfoLast
	c.netInfoFunc = fn
	c.mu.Unlock()

	if last != nil {
		fn(last)
	}
}

// LastRecvActivityOfDisco describes the time we last got traffic from
// this endpoint (updated every ~10 seconds).
func (c *Conn) LastRecvActivityOfDisco(dk tailcfg.DiscoKey) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	de, ok := c.peerMap.endpointForDiscoKey(dk)
	if !ok {
		return "never"
	}
	saw := de.lastRecv.LoadAtomic()
	if saw == 0 {
		return "never"
	}
	return mono.Since(saw).Round(time.Second).String()
}

// Ping handles a "tailscale ping" CLI query.
func (c *Conn) Ping(peer *tailcfg.Node, res *ipnstate.PingResult, cb func(*ipnstate.PingResult)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.privateKey.IsZero() {
		res.Err = "local tailscaled stopped"
		cb(res)
		return
	}
	if len(peer.Addresses) > 0 {
		res.NodeIP = peer.Addresses[0].IP().String()
	}
	res.NodeName = peer.Name // prefer DNS name
	if res.NodeName == "" {
		res.NodeName = peer.Hostinfo.Hostname // else hostname
	} else {
		if i := strings.Index(res.NodeName, "."); i != -1 {
			res.NodeName = res.NodeName[:i]
		}
	}

	ep, ok := c.peerMap.endpointForNodeKey(peer.Key)
	if !ok {
		res.Err = "unknown peer"
		cb(res)
		return
	}
	ep.cliPing(res, cb)
}

// c.mu must be held
func (c *Conn) populateCLIPingResponseLocked(res *ipnstate.PingResult, latency time.Duration, ep netaddr.IPPort) {
	res.LatencySeconds = latency.Seconds()
	if ep.IP() != derpMagicIPAddr {
		res.Endpoint = ep.String()
		return
	}
	regionID := int(ep.Port())
	res.DERPRegionID = regionID
	res.DERPRegionCode = c.derpRegionCodeLocked(regionID)
}

func (c *Conn) derpRegionCodeLocked(regionID int) string {
	if c.derpMap == nil {
		return ""
	}
	if dr, ok := c.derpMap.Regions[regionID]; ok {
		return dr.RegionCode
	}
	return ""
}

// DiscoPublicKey returns the discovery public key.
func (c *Conn) DiscoPublicKey() tailcfg.DiscoKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.discoPrivate.IsZero() {
		priv := key.NewPrivate()
		c.discoPrivate = priv
		c.discoPublic = tailcfg.DiscoKey(priv.Public())
		c.discoShort = c.discoPublic.ShortString()
		c.logf("magicsock: disco key = %v", c.discoShort)
	}
	return c.discoPublic
}

// PeerHasDiscoKey reports whether peer k supports discovery keys (client version 0.100.0+).
func (c *Conn) PeerHasDiscoKey(k tailcfg.NodeKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ep, ok := c.peerMap.endpointForNodeKey(k); ok {
		return ep.discoKey.IsZero()
	}
	return false
}

// c.mu must NOT be held.
func (c *Conn) setNearestDERP(derpNum int) (wantDERP bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.wantDerpLocked() {
		c.myDerp = 0
		health.SetMagicSockDERPHome(0)
		return false
	}
	if derpNum == c.myDerp {
		// No change.
		return true
	}
	c.myDerp = derpNum
	health.SetMagicSockDERPHome(derpNum)

	if c.privateKey.IsZero() {
		// No private key yet, so DERP connections won't come up anyway.
		// Return early rather than ultimately log a couple lines of noise.
		return true
	}

	// On change, notify all currently connected DERP servers and
	// start connecting to our home DERP if we are not already.
	dr := c.derpMap.Regions[derpNum]
	if dr == nil {
		c.logf("[unexpected] magicsock: derpMap.Regions[%v] is nil", derpNum)
	} else {
		c.logf("magicsock: home is now derp-%v (%v)", derpNum, c.derpMap.Regions[derpNum].RegionCode)
	}
	for i, ad := range c.activeDerp {
		go ad.c.NotePreferred(i == c.myDerp)
	}
	c.goDerpConnect(derpNum)
	return true
}

// startDerpHomeConnectLocked starts connecting to our DERP home, if any.
//
// c.mu must be held.
func (c *Conn) startDerpHomeConnectLocked() {
	c.goDerpConnect(c.myDerp)
}

// goDerpConnect starts a goroutine to start connecting to the given
// DERP node.
//
// c.mu may be held, but does not need to be.
func (c *Conn) goDerpConnect(node int) {
	if node == 0 {
		return
	}
	go c.derpWriteChanOfAddr(netaddr.IPPortFrom(derpMagicIPAddr, uint16(node)), key.Public{})
}

// determineEndpoints returns the machine's endpoint addresses. It
// does a STUN lookup (via netcheck) to determine its public address.
//
// c.mu must NOT be held.
func (c *Conn) determineEndpoints(ctx context.Context) ([]tailcfg.Endpoint, error) {
	portmapExt, havePortmap := c.portMapper.GetCachedMappingOrStartCreatingOne()

	nr, err := c.updateNetInfo(ctx)
	if err != nil {
		c.logf("magicsock.Conn.determineEndpoints: updateNetInfo: %v", err)
		return nil, err
	}

	already := make(map[netaddr.IPPort]tailcfg.EndpointType) // endpoint -> how it was found
	var eps []tailcfg.Endpoint                               // unique endpoints

	ipp := func(s string) (ipp netaddr.IPPort) {
		ipp, _ = netaddr.ParseIPPort(s)
		return
	}
	addAddr := func(ipp netaddr.IPPort, et tailcfg.EndpointType) {
		if ipp.IsZero() || (debugOmitLocalAddresses && et == tailcfg.EndpointLocal) {
			return
		}
		if _, ok := already[ipp]; !ok {
			already[ipp] = et
			eps = append(eps, tailcfg.Endpoint{Addr: ipp, Type: et})
		}
	}

	// If we didn't have a portmap earlier, maybe it's done by now.
	if !havePortmap {
		portmapExt, havePortmap = c.portMapper.GetCachedMappingOrStartCreatingOne()
	}
	if havePortmap {
		addAddr(portmapExt, tailcfg.EndpointPortmapped)
		c.setNetInfoHavePortMap()
	}

	if nr.GlobalV4 != "" {
		addAddr(ipp(nr.GlobalV4), tailcfg.EndpointSTUN)

		// If they're behind a hard NAT and are using a fixed
		// port locally, assume they might've added a static
		// port mapping on their router to the same explicit
		// port that tailscaled is running with. Worst case
		// it's an invalid candidate mapping.
		if port := c.port.Get(); nr.MappingVariesByDestIP.EqualBool(true) && port != 0 {
			if ip, _, err := net.SplitHostPort(nr.GlobalV4); err == nil {
				addAddr(ipp(net.JoinHostPort(ip, strconv.Itoa(int(port)))), tailcfg.EndpointSTUN4LocalPort)
			}
		}
	}
	if nr.GlobalV6 != "" {
		addAddr(ipp(nr.GlobalV6), tailcfg.EndpointSTUN)
	}

	c.ignoreSTUNPackets()

	if localAddr := c.pconn4.LocalAddr(); localAddr.IP.IsUnspecified() {
		ips, loopback, err := interfaces.LocalAddresses()
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 && len(eps) == 0 {
			// Only include loopback addresses if we have no
			// interfaces at all to use as endpoints and don't
			// have a public IPv4 or IPv6 address. This allows
			// for localhost testing when you're on a plane and
			// offline, for example.
			ips = loopback
		}
		for _, ip := range ips {
			addAddr(netaddr.IPPortFrom(ip, uint16(localAddr.Port)), tailcfg.EndpointLocal)
		}
	} else {
		// Our local endpoint is bound to a particular address.
		// Do not offer addresses on other local interfaces.
		addAddr(ipp(localAddr.String()), tailcfg.EndpointLocal)
	}

	// Note: the endpoints are intentionally returned in priority order,
	// from "farthest but most reliable" to "closest but least
	// reliable." Addresses returned from STUN should be globally
	// addressable, but might go farther on the network than necessary.
	// Local interface addresses might have lower latency, but not be
	// globally addressable.
	//
	// The STUN address(es) are always first so that legacy wireguard
	// can use eps[0] as its only known endpoint address (although that's
	// obviously non-ideal).
	//
	// Despite this sorting, though, clients since 0.100 haven't relied
	// on the sorting order for any decisions.
	return eps, nil
}

// endpointSetsEqual reports whether x and y represent the same set of
// endpoints. The order doesn't matter.
//
// It does not mutate the slices.
func endpointSetsEqual(x, y []tailcfg.Endpoint) bool {
	if len(x) == len(y) {
		orderMatches := true
		for i := range x {
			if x[i] != y[i] {
				orderMatches = false
				break
			}
		}
		if orderMatches {
			return true
		}
	}
	m := map[tailcfg.Endpoint]int{}
	for _, v := range x {
		m[v] |= 1
	}
	for _, v := range y {
		m[v] |= 2
	}
	for _, n := range m {
		if n != 3 {
			return false
		}
	}
	return true
}

// LocalPort returns the current IPv4 listener's port number.
func (c *Conn) LocalPort() uint16 {
	laddr := c.pconn4.LocalAddr()
	return uint16(laddr.Port)
}

var errNetworkDown = errors.New("magicsock: network down")

func (c *Conn) networkDown() bool { return !c.networkUp.Get() }

func (c *Conn) Send(b []byte, ep conn.Endpoint) error {
	if c.networkDown() {
		return errNetworkDown
	}
	return ep.(*endpoint).send(b)
}

var errConnClosed = errors.New("Conn closed")

var errDropDerpPacket = errors.New("too many DERP packets queued; dropping")

var udpAddrPool = &sync.Pool{
	New: func() interface{} { return new(net.UDPAddr) },
}

// sendUDP sends UDP packet b to ipp.
// See sendAddr's docs on the return value meanings.
func (c *Conn) sendUDP(ipp netaddr.IPPort, b []byte) (sent bool, err error) {
	ua := udpAddrPool.Get().(*net.UDPAddr)
	defer udpAddrPool.Put(ua)
	return c.sendUDPStd(ipp.UDPAddrAt(ua), b)
}

// sendUDP sends UDP packet b to addr.
// See sendAddr's docs on the return value meanings.
func (c *Conn) sendUDPStd(addr *net.UDPAddr, b []byte) (sent bool, err error) {
	switch {
	case addr.IP.To4() != nil:
		_, err = c.pconn4.WriteTo(b, addr)
		if err != nil && c.noV4.Get() {
			return false, nil
		}
	case len(addr.IP) == net.IPv6len:
		if c.pconn6 == nil {
			// ignore IPv6 dest if we don't have an IPv6 address.
			return false, nil
		}
		_, err = c.pconn6.WriteTo(b, addr)
		if err != nil && c.noV6.Get() {
			return false, nil
		}
	default:
		panic("bogus sendUDPStd addr type")
	}
	return err == nil, err
}

// sendAddr sends packet b to addr, which is either a real UDP address
// or a fake UDP address representing a DERP server (see derpmap.go).
// The provided public key identifies the recipient.
//
// The returned err is whether there was an error writing when it
// should've worked.
// The returned sent is whether a packet went out at all.
// An example of when they might be different: sending to an
// IPv6 address when the local machine doesn't have IPv6 support
// returns (false, nil); it's not an error, but nothing was sent.
func (c *Conn) sendAddr(addr netaddr.IPPort, pubKey key.Public, b []byte) (sent bool, err error) {
	if addr.IP() != derpMagicIPAddr {
		return c.sendUDP(addr, b)
	}

	ch := c.derpWriteChanOfAddr(addr, pubKey)
	if ch == nil {
		return false, nil
	}

	// TODO(bradfitz): this makes garbage for now; we could use a
	// buffer pool later.  Previously we passed ownership of this
	// to derpWriteRequest and waited for derphttp.Client.Send to
	// complete, but that's too slow while holding wireguard-go
	// internal locks.
	pkt := make([]byte, len(b))
	copy(pkt, b)

	select {
	case <-c.donec:
		return false, errConnClosed
	case ch <- derpWriteRequest{addr, pubKey, pkt}:
		return true, nil
	default:
		// Too many writes queued. Drop packet.
		return false, errDropDerpPacket
	}
}

// bufferedDerpWritesBeforeDrop is how many packets writes can be
// queued up the DERP client to write on the wire before we start
// dropping.
//
// TODO: this is currently arbitrary. Figure out something better?
const bufferedDerpWritesBeforeDrop = 32

// derpWriteChanOfAddr returns a DERP client for fake UDP addresses that
// represent DERP servers, creating them as necessary. For real UDP
// addresses, it returns nil.
//
// If peer is non-zero, it can be used to find an active reverse
// path, without using addr.
func (c *Conn) derpWriteChanOfAddr(addr netaddr.IPPort, peer key.Public) chan<- derpWriteRequest {
	if addr.IP() != derpMagicIPAddr {
		return nil
	}
	regionID := int(addr.Port())

	if c.networkDown() {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.wantDerpLocked() || c.closed {
		return nil
	}
	if c.privateKey.IsZero() {
		c.logf("magicsock: DERP lookup of %v with no private key; ignoring", addr)
		return nil
	}

	// See if we have a connection open to that DERP node ID
	// first. If so, might as well use it. (It's a little
	// arbitrary whether we use this one vs. the reverse route
	// below when we have both.)
	ad, ok := c.activeDerp[regionID]
	if ok {
		*ad.lastWrite = time.Now()
		c.setPeerLastDerpLocked(peer, regionID, regionID)
		return ad.writeCh
	}

	// If we don't have an open connection to the peer's home DERP
	// node, see if we have an open connection to a DERP node
	// where we'd heard from that peer already. For instance,
	// perhaps peer's home is Frankfurt, but they dialed our home DERP
	// node in SF to reach us, so we can reply to them using our
	// SF connection rather than dialing Frankfurt. (Issue 150)
	if !peer.IsZero() && useDerpRoute() {
		if r, ok := c.derpRoute[peer]; ok {
			if ad, ok := c.activeDerp[r.derpID]; ok && ad.c == r.dc {
				c.setPeerLastDerpLocked(peer, r.derpID, regionID)
				*ad.lastWrite = time.Now()
				return ad.writeCh
			}
		}
	}

	why := "home-keep-alive"
	if !peer.IsZero() {
		why = peerShort(peer)
	}
	c.logf("magicsock: adding connection to derp-%v for %v", regionID, why)

	firstDerp := false
	if c.activeDerp == nil {
		firstDerp = true
		c.activeDerp = make(map[int]activeDerp)
		c.prevDerp = make(map[int]*syncs.WaitGroupChan)
	}
	if c.derpMap == nil || c.derpMap.Regions[regionID] == nil {
		return nil
	}

	// Note that derphttp.NewRegionClient does not dial the server
	// so it is safe to do under the mu lock.
	dc := derphttp.NewRegionClient(c.privateKey, c.logf, func() *tailcfg.DERPRegion {
		if c.connCtx.Err() != nil {
			// If we're closing, don't try to acquire the lock.
			// We might already be in Conn.Close and the Lock would deadlock.
			return nil
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.derpMap == nil {
			return nil
		}
		return c.derpMap.Regions[regionID]
	})

	dc.SetCanAckPings(true)
	dc.NotePreferred(c.myDerp == regionID)
	dc.DNSCache = dnscache.Get()

	ctx, cancel := context.WithCancel(c.connCtx)
	ch := make(chan derpWriteRequest, bufferedDerpWritesBeforeDrop)

	ad.c = dc
	ad.writeCh = ch
	ad.cancel = cancel
	ad.lastWrite = new(time.Time)
	*ad.lastWrite = time.Now()
	ad.createTime = time.Now()
	c.activeDerp[regionID] = ad
	c.logActiveDerpLocked()
	c.setPeerLastDerpLocked(peer, regionID, regionID)
	c.scheduleCleanStaleDerpLocked()

	// Build a startGate for the derp reader+writer
	// goroutines, so they don't start running until any
	// previous generation is closed.
	startGate := syncs.ClosedChan()
	if prev := c.prevDerp[regionID]; prev != nil {
		startGate = prev.DoneChan()
	}
	// And register a WaitGroup(Chan) for this generation.
	wg := syncs.NewWaitGroupChan()
	wg.Add(2)
	c.prevDerp[regionID] = wg

	if firstDerp {
		startGate = c.derpStarted
		go func() {
			dc.Connect(ctx)
			close(c.derpStarted)
			c.muCond.Broadcast()
		}()
	}

	go c.runDerpReader(ctx, addr, dc, wg, startGate)
	go c.runDerpWriter(ctx, dc, ch, wg, startGate)
	go c.derpActiveFunc()

	return ad.writeCh
}

// setPeerLastDerpLocked notes that peer is now being written to via
// the provided DERP regionID, and that the peer advertises a DERP
// home region ID of homeID.
//
// If there's any change, it logs.
//
// c.mu must be held.
func (c *Conn) setPeerLastDerpLocked(peer key.Public, regionID, homeID int) {
	if peer.IsZero() {
		return
	}
	old := c.peerLastDerp[peer]
	if old == regionID {
		return
	}
	c.peerLastDerp[peer] = regionID

	var newDesc string
	switch {
	case regionID == homeID && regionID == c.myDerp:
		newDesc = "shared home"
	case regionID == homeID:
		newDesc = "their home"
	case regionID == c.myDerp:
		newDesc = "our home"
	case regionID != homeID:
		newDesc = "alt"
	}
	if old == 0 {
		c.logf("[v1] magicsock: derp route for %s set to derp-%d (%s)", peerShort(peer), regionID, newDesc)
	} else {
		c.logf("[v1] magicsock: derp route for %s changed from derp-%d => derp-%d (%s)", peerShort(peer), old, regionID, newDesc)
	}
}

// derpReadResult is the type sent by runDerpClient to ReceiveIPv4
// when a DERP packet is available.
//
// Notably, it doesn't include the derp.ReceivedPacket because we
// don't want to give the receiver access to the aliased []byte.  To
// get at the packet contents they need to call copyBuf to copy it
// out, which also releases the buffer.
type derpReadResult struct {
	regionID int
	n        int        // length of data received
	src      key.Public // may be zero until server deployment if v2+
	// copyBuf is called to copy the data to dst.  It returns how
	// much data was copied, which will be n if dst is large
	// enough. copyBuf can only be called once.
	// If copyBuf is nil, that's a signal from the sender to ignore
	// this message.
	copyBuf func(dst []byte) int
}

// runDerpReader runs in a goroutine for the life of a DERP
// connection, handling received packets.
func (c *Conn) runDerpReader(ctx context.Context, derpFakeAddr netaddr.IPPort, dc *derphttp.Client, wg *syncs.WaitGroupChan, startGate <-chan struct{}) {
	defer wg.Decr()
	defer dc.Close()

	select {
	case <-startGate:
	case <-ctx.Done():
		return
	}

	didCopy := make(chan struct{}, 1)
	regionID := int(derpFakeAddr.Port())
	res := derpReadResult{regionID: regionID}
	var pkt derp.ReceivedPacket
	res.copyBuf = func(dst []byte) int {
		n := copy(dst, pkt.Data)
		didCopy <- struct{}{}
		return n
	}

	defer health.SetDERPRegionConnectedState(regionID, false)
	defer health.SetDERPRegionHealth(regionID, "")

	// peerPresent is the set of senders we know are present on this
	// connection, based on messages we've received from the server.
	peerPresent := map[key.Public]bool{}
	bo := backoff.NewBackoff(fmt.Sprintf("derp-%d", regionID), c.logf, 5*time.Second)
	var lastPacketTime time.Time

	for {
		msg, connGen, err := dc.RecvDetail()
		if err != nil {
			health.SetDERPRegionConnectedState(regionID, false)
			// Forget that all these peers have routes.
			for peer := range peerPresent {
				delete(peerPresent, peer)
				c.removeDerpPeerRoute(peer, regionID, dc)
			}
			if err == derphttp.ErrClientClosed {
				return
			}
			if c.networkDown() {
				c.logf("[v1] magicsock: derp.Recv(derp-%d): network down, closing", regionID)
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}

			c.logf("magicsock: [%p] derp.Recv(derp-%d): %v", dc, regionID, err)

			// If our DERP connection broke, it might be because our network
			// conditions changed. Start that check.
			c.ReSTUN("derp-recv-error")

			// Back off a bit before reconnecting.
			bo.BackOff(ctx, err)
			select {
			case <-ctx.Done():
				return
			default:
			}
			continue
		}
		bo.BackOff(ctx, nil) // reset

		now := time.Now()
		if lastPacketTime.IsZero() || now.Sub(lastPacketTime) > 5*time.Second {
			health.NoteDERPRegionReceivedFrame(regionID)
			lastPacketTime = now
		}

		switch m := msg.(type) {
		case derp.ServerInfoMessage:
			health.SetDERPRegionConnectedState(regionID, true)
			health.SetDERPRegionHealth(regionID, "") // until declared otherwise
			c.logf("magicsock: derp-%d connected; connGen=%v", regionID, connGen)
			continue
		case derp.ReceivedPacket:
			pkt = m
			res.n = len(m.Data)
			res.src = m.Source
			if logDerpVerbose {
				c.logf("magicsock: got derp-%v packet: %q", regionID, m.Data)
			}
			// If this is a new sender we hadn't seen before, remember it and
			// register a route for this peer.
			if _, ok := peerPresent[m.Source]; !ok {
				peerPresent[m.Source] = true
				c.addDerpPeerRoute(m.Source, regionID, dc)
			}
		case derp.PingMessage:
			// Best effort reply to the ping.
			pingData := [8]byte(m)
			go func() {
				if err := dc.SendPong(pingData); err != nil {
					c.logf("magicsock: derp-%d SendPong error: %v", regionID, err)
				}
			}()
			continue
		case derp.HealthMessage:
			health.SetDERPRegionHealth(regionID, m.Problem)
		default:
			// Ignore.
			continue
		}

		select {
		case <-ctx.Done():
			return
		case c.derpRecvCh <- res:
		}

		select {
		case <-ctx.Done():
			return
		case <-didCopy:
			continue
		}
	}
}

type derpWriteRequest struct {
	addr   netaddr.IPPort
	pubKey key.Public
	b      []byte // copied; ownership passed to receiver
}

// runDerpWriter runs in a goroutine for the life of a DERP
// connection, handling received packets.
func (c *Conn) runDerpWriter(ctx context.Context, dc *derphttp.Client, ch <-chan derpWriteRequest, wg *syncs.WaitGroupChan, startGate <-chan struct{}) {
	defer wg.Decr()
	select {
	case <-startGate:
	case <-ctx.Done():
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case wr := <-ch:
			err := dc.Send(wr.pubKey, wr.b)
			if err != nil {
				c.logf("magicsock: derp.Send(%v): %v", wr.addr, err)
			}
		}
	}
}

// receiveIPv6 receives a UDP IPv6 packet. It is called by wireguard-go.
func (c *Conn) receiveIPv6(b []byte) (int, conn.Endpoint, error) {
	health.ReceiveIPv6.Enter()
	defer health.ReceiveIPv6.Exit()
	for {
		n, ipp, err := c.pconn6.ReadFromNetaddr(b)
		if err != nil {
			return 0, nil, err
		}
		if ep, ok := c.receiveIP(b[:n], ipp, &c.ippEndpoint6); ok {
			return n, ep, nil
		}
	}
}

// receiveIPv4 receives a UDP IPv4 packet. It is called by wireguard-go.
func (c *Conn) receiveIPv4(b []byte) (n int, ep conn.Endpoint, err error) {
	health.ReceiveIPv4.Enter()
	defer health.ReceiveIPv4.Exit()
	for {
		n, ipp, err := c.pconn4.ReadFromNetaddr(b)
		if err != nil {
			return 0, nil, err
		}
		if ep, ok := c.receiveIP(b[:n], ipp, &c.ippEndpoint4); ok {
			return n, ep, nil
		}
	}
}

// receiveIP is the shared bits of ReceiveIPv4 and ReceiveIPv6.
//
// ok is whether this read should be reported up to wireguard-go (our
// caller).
func (c *Conn) receiveIP(b []byte, ipp netaddr.IPPort, cache *ippEndpointCache) (ep *endpoint, ok bool) {
	if stun.Is(b) {
		c.stunReceiveFunc.Load().(func([]byte, netaddr.IPPort))(b, ipp)
		return nil, false
	}
	if c.handleDiscoMessage(b, ipp) {
		return nil, false
	}
	if !c.havePrivateKey.Get() {
		// If we have no private key, we're logged out or
		// stopped. Don't try to pass these wireguard packets
		// up to wireguard-go; it'll just complain (issue 1167).
		return nil, false
	}
	if cache.ipp == ipp && cache.de != nil && cache.gen == cache.de.numStopAndReset() {
		ep = cache.de
	} else {
		c.mu.Lock()
		de, ok := c.peerMap.endpointForIPPort(ipp)
		c.mu.Unlock()
		if !ok {
			return nil, false
		}
		cache.ipp = ipp
		cache.de = de
		cache.gen = de.numStopAndReset()
		ep = de
	}
	ep.noteRecvActivity()
	return ep, true
}

// receiveDERP reads a packet from c.derpRecvCh into b and returns the associated endpoint.
// It is called by wireguard-go.
//
// If the packet was a disco message or the peer endpoint wasn't
// found, the returned error is errLoopAgain.
func (c *connBind) receiveDERP(b []byte) (n int, ep conn.Endpoint, err error) {
	health.ReceiveDERP.Enter()
	defer health.ReceiveDERP.Exit()
	for dm := range c.derpRecvCh {
		if c.Closed() {
			break
		}
		n, ep := c.processDERPReadResult(dm, b)
		if n == 0 {
			// No data read occurred. Wait for another packet.
			continue
		}
		return n, ep, nil
	}
	return 0, nil, net.ErrClosed
}

func (c *Conn) processDERPReadResult(dm derpReadResult, b []byte) (n int, ep *endpoint) {
	if dm.copyBuf == nil {
		return 0, nil
	}
	var regionID int
	n, regionID = dm.n, dm.regionID
	ncopy := dm.copyBuf(b)
	if ncopy != n {
		err := fmt.Errorf("received DERP packet of length %d that's too big for WireGuard buf size %d", n, ncopy)
		c.logf("magicsock: %v", err)
		return 0, nil
	}

	ipp := netaddr.IPPortFrom(derpMagicIPAddr, uint16(regionID))
	if c.handleDiscoMessage(b[:n], ipp) {
		return 0, nil
	}

	var ok bool
	c.mu.Lock()
	ep, ok = c.peerMap.endpointForNodeKey(tailcfg.NodeKey(dm.src))
	c.mu.Unlock()
	if !ok {
		// We don't know anything about this node key, nothing to
		// record or process.
		return 0, nil
	}

	ep.noteRecvActivity()
	return n, ep
}

// discoLogLevel controls the verbosity of discovery log messages.
type discoLogLevel int

const (
	// discoLog means that a message should be logged.
	discoLog discoLogLevel = iota

	// discoVerboseLog means that a message should only be logged
	// in TS_DEBUG_DISCO mode.
	discoVerboseLog
)

func (c *Conn) sendDiscoMessage(dst netaddr.IPPort, dstKey tailcfg.NodeKey, dstDisco tailcfg.DiscoKey, m disco.Message, logLevel discoLogLevel) (sent bool, err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false, errConnClosed
	}
	var nonce [disco.NonceLen]byte
	if _, err := crand.Read(nonce[:]); err != nil {
		panic(err) // worth dying for
	}
	pkt := make([]byte, 0, 512) // TODO: size it correctly? pool? if it matters.
	pkt = append(pkt, disco.Magic...)
	pkt = append(pkt, c.discoPublic[:]...)
	pkt = append(pkt, nonce[:]...)
	sharedKey := c.sharedDiscoKeyLocked(dstDisco)
	c.mu.Unlock()

	pkt = box.SealAfterPrecomputation(pkt, m.AppendMarshal(nil), &nonce, sharedKey)
	sent, err = c.sendAddr(dst, key.Public(dstKey), pkt)
	if sent {
		if logLevel == discoLog || (logLevel == discoVerboseLog && debugDisco) {
			c.logf("[v1] magicsock: disco: %v->%v (%v, %v) sent %v", c.discoShort, dstDisco.ShortString(), dstKey.ShortString(), derpStr(dst.String()), disco.MessageSummary(m))
		}
	} else if err == nil {
		// Can't send. (e.g. no IPv6 locally)
	} else {
		if !c.networkDown() {
			c.logf("magicsock: disco: failed to send %T to %v: %v", m, dst, err)
		}
	}
	return sent, err
}

// handleDiscoMessage handles a discovery message and reports whether
// msg was a Tailscale inter-node discovery message.
//
// A discovery message has the form:
//
//  * magic             [6]byte
//  * senderDiscoPubKey [32]byte
//  * nonce             [24]byte
//  * naclbox of payload (see tailscale.com/disco package for inner payload format)
//
// For messages received over DERP, the addr will be derpMagicIP (with
// port being the region)
func (c *Conn) handleDiscoMessage(msg []byte, src netaddr.IPPort) (isDiscoMsg bool) {
	const headerLen = len(disco.Magic) + len(tailcfg.DiscoKey{}) + disco.NonceLen
	if len(msg) < headerLen || string(msg[:len(disco.Magic)]) != disco.Magic {
		return false
	}

	// If the first four parts are the prefix of disco.Magic
	// (0x5453f09f) then it's definitely not a valid Wireguard
	// packet (which starts with little-endian uint32 1, 2, 3, 4).
	// Use naked returns for all following paths.
	isDiscoMsg = true

	var sender tailcfg.DiscoKey
	copy(sender[:], msg[len(disco.Magic):])

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}
	if debugDisco {
		c.logf("magicsock: disco: got disco-looking frame from %v", sender.ShortString())
	}
	if c.privateKey.IsZero() {
		// Ignore disco messages when we're stopped.
		// Still return true, to not pass it down to wireguard.
		return
	}
	if c.discoPrivate.IsZero() {
		if debugDisco {
			c.logf("magicsock: disco: ignoring disco-looking frame, no local key")
		}
		return
	}

	ep, ok := c.peerMap.endpointForDiscoKey(sender)
	if !ok {
		if debugDisco {
			c.logf("magicsock: disco: ignoring disco-looking frame, don't know endpoint for %v", sender.ShortString())
		}
		return
	}
	if !ep.canP2P() {
		// This endpoint allegedly sent us a disco packet, but we know
		// they can't speak disco. Drop.
		return
	}

	// We're now reasonably sure we're expecting communication from
	// this peer, do the heavy crypto lifting to see what they want.
	//
	// From here on, peerNode and de are non-nil.

	var nonce [disco.NonceLen]byte
	copy(nonce[:], msg[len(disco.Magic)+len(key.Public{}):])
	sealedBox := msg[headerLen:]
	payload, ok := box.OpenAfterPrecomputation(nil, sealedBox, &nonce, c.sharedDiscoKeyLocked(sender))
	if !ok {
		// This might be have been intended for a previous
		// disco key.  When we restart we get a new disco key
		// and old packets might've still been in flight (or
		// scheduled). This is particularly the case for LANs
		// or non-NATed endpoints.
		// Don't log in normal case. Pass on to wireguard, in case
		// it's actually a wireguard packet (super unlikely,
		// but).
		if debugDisco {
			c.logf("magicsock: disco: failed to open naclbox from %v (wrong rcpt?)", sender)
		}
		// TODO(bradfitz): add some counter for this that logs rarely
		return
	}

	dm, err := disco.Parse(payload)
	if debugDisco {
		c.logf("magicsock: disco: disco.Parse = %T, %v", dm, err)
	}
	if err != nil {
		// Couldn't parse it, but it was inside a correctly
		// signed box, so just ignore it, assuming it's from a
		// newer version of Tailscale that we don't
		// understand. Not even worth logging about, lest it
		// be too spammy for old clients.
		// TODO(bradfitz): add some counter for this that logs rarely
		return
	}

	switch dm := dm.(type) {
	case *disco.Ping:
		c.handlePingLocked(dm, ep, src, sender)
	case *disco.Pong:
		ep.handlePongConnLocked(dm, src)
	case *disco.CallMeMaybe:
		if src.IP() != derpMagicIPAddr {
			// CallMeMaybe messages should only come via DERP.
			c.logf("[unexpected] CallMeMaybe packets should only come via DERP")
			return
		}
		c.logf("[v1] magicsock: disco: %v<-%v (%v, %v)  got call-me-maybe, %d endpoints",
			c.discoShort, ep.discoShort,
			ep.publicKey.ShortString(), derpStr(src.String()),
			len(dm.MyNumber))
		go ep.handleCallMeMaybe(dm)
	}
	return
}

func (c *Conn) handlePingLocked(dm *disco.Ping, de *endpoint, src netaddr.IPPort, sender tailcfg.DiscoKey) {
	likelyHeartBeat := src == de.lastPingFrom && time.Since(de.lastPingTime) < 5*time.Second
	de.lastPingFrom = src
	de.lastPingTime = time.Now()
	if !likelyHeartBeat || debugDisco {
		c.logf("[v1] magicsock: disco: %v<-%v (%v, %v)  got ping tx=%x", c.discoShort, de.discoShort, de.publicKey.ShortString(), src, dm.TxID[:6])
	}

	// Remember this route if not present.
	c.setAddrToDiscoLocked(src, sender)
	de.addCandidateEndpoint(src)

	ipDst := src
	discoDest := sender
	go c.sendDiscoMessage(ipDst, de.publicKey, discoDest, &disco.Pong{
		TxID: dm.TxID,
		Src:  src,
	}, discoVerboseLog)
}

// enqueueCallMeMaybe schedules a send of disco.CallMeMaybe to de via derpAddr
// once we know that our STUN endpoint is fresh.
//
// derpAddr is de.derpAddr at the time of send. It's assumed the peer won't be
// flipping primary DERPs in the 0-30ms it takes to confirm our STUN endpoint.
// If they do, traffic will just go over DERP for a bit longer until the next
// discovery round.
func (c *Conn) enqueueCallMeMaybe(derpAddr netaddr.IPPort, de *endpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.lastEndpointsTime.After(time.Now().Add(-endpointsFreshEnoughDuration)) {
		c.logf("magicsock: want call-me-maybe but endpoints stale; restunning")
		if c.onEndpointRefreshed == nil {
			c.onEndpointRefreshed = map[*endpoint]func(){}
		}
		c.onEndpointRefreshed[de] = func() {
			c.logf("magicsock: STUN done; sending call-me-maybe to %v %v", de.discoShort, de.publicKey.ShortString())
			c.enqueueCallMeMaybe(derpAddr, de)
		}
		// TODO(bradfitz): make a new 'reSTUNQuickly' method
		// that passes down a do-a-lite-netcheck flag down to
		// netcheck that does 1 (or 2 max) STUN queries
		// (UDP-only, not HTTPs) to find our port mapping to
		// our home DERP and maybe one other. For now we do a
		// "full" ReSTUN which may or may not be a full one
		// (depending on age) and may do HTTPS timing queries
		// (if UDP is blocked). Good enough for now.
		go c.ReSTUN("refresh-for-peering")
		return
	}

	eps := make([]netaddr.IPPort, 0, len(c.lastEndpoints))
	for _, ep := range c.lastEndpoints {
		eps = append(eps, ep.Addr)
	}
	go de.sendDiscoMessage(derpAddr, &disco.CallMeMaybe{MyNumber: eps}, discoLog)
}

// setAddrToDiscoLocked records that newk is at src.
//
// c.mu must be held.
func (c *Conn) setAddrToDiscoLocked(src netaddr.IPPort, newk tailcfg.DiscoKey) {
	oldEp, ok := c.peerMap.endpointForIPPort(src)
	if !ok {
		c.logf("[v1] magicsock: disco: adding mapping of %v to %v", src, newk.ShortString())
	} else if oldEp.discoKey != newk {
		c.logf("[v1] magicsock: disco: changing mapping of %v from %x=>%x", src, oldEp.discoKey.ShortString(), newk.ShortString())
	} else {
		// No change
		return
	}
	c.peerMap.setDiscoKeyForIPPort(src, newk)
}

func (c *Conn) sharedDiscoKeyLocked(k tailcfg.DiscoKey) *[32]byte {
	if v, ok := c.sharedDiscoKey[k]; ok {
		return v
	}
	shared := new([32]byte)
	box.Precompute(shared, key.Public(k).B32(), c.discoPrivate.B32())
	c.sharedDiscoKey[k] = shared
	return shared
}

func (c *Conn) SetNetworkUp(up bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.networkUp.Get() == up {
		return
	}

	c.logf("magicsock: SetNetworkUp(%v)", up)
	c.networkUp.Set(up)

	if up {
		c.startDerpHomeConnectLocked()
	} else {
		c.portMapper.NoteNetworkDown()
		c.closeAllDerpLocked("network-down")
	}
}

// SetPreferredPort sets the connection's preferred local port.
func (c *Conn) SetPreferredPort(port uint16) {
	if uint16(c.port.Get()) == port {
		return
	}
	c.port.Set(uint32(port))

	if err := c.rebind(dropCurrentPort); err != nil {
		c.logf("%w", err)
		return
	}
	c.resetEndpointStates()
}

// SetPrivateKey sets the connection's private key.
//
// This is only used to be able prove our identity when connecting to
// DERP servers.
//
// If the private key changes, any DERP connections are torn down &
// recreated when needed.
func (c *Conn) SetPrivateKey(privateKey wgkey.Private) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldKey, newKey := c.privateKey, key.Private(privateKey)
	if newKey == oldKey {
		return nil
	}
	c.privateKey = newKey
	c.havePrivateKey.Set(!newKey.IsZero())

	if oldKey.IsZero() {
		c.everHadKey = true
		c.logf("magicsock: SetPrivateKey called (init)")
		go c.ReSTUN("set-private-key")
	} else if newKey.IsZero() {
		c.logf("magicsock: SetPrivateKey called (zeroed)")
		c.closeAllDerpLocked("zero-private-key")
		c.stopPeriodicReSTUNTimerLocked()
		c.onEndpointRefreshed = nil
	} else {
		c.logf("magicsock: SetPrivateKey called (changed)")
		c.closeAllDerpLocked("new-private-key")
	}

	// Key changed. Close existing DERP connections and reconnect to home.
	if c.myDerp != 0 && !newKey.IsZero() {
		c.logf("magicsock: private key changed, reconnecting to home derp-%d", c.myDerp)
		c.startDerpHomeConnectLocked()
	}

	if newKey.IsZero() {
		c.peerMap.forEachDiscoEndpoint(func(ep *endpoint) {
			ep.stopAndReset()
		})
	}

	return nil
}

// UpdatePeers is called when the set of WireGuard peers changes. It
// then removes any state for old peers.
//
// The caller passes ownership of newPeers map to UpdatePeers.
func (c *Conn) UpdatePeers(newPeers map[key.Public]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldPeers := c.peerSet
	c.peerSet = newPeers

	// Clean up any key.Public-keyed maps for peers that no longer
	// exist.
	for peer := range oldPeers {
		if _, ok := newPeers[peer]; !ok {
			delete(c.derpRoute, peer)
			delete(c.peerLastDerp, peer)
		}
	}

	if len(oldPeers) == 0 && len(newPeers) > 0 {
		go c.ReSTUN("non-zero-peers")
	}
}

// SetDERPMap controls which (if any) DERP servers are used.
// A nil value means to disable DERP; it's disabled by default.
func (c *Conn) SetDERPMap(dm *tailcfg.DERPMap) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if reflect.DeepEqual(dm, c.derpMap) {
		return
	}

	c.derpMap = dm
	if dm == nil {
		c.closeAllDerpLocked("derp-disabled")
		return
	}

	go c.ReSTUN("derp-map-update")
}

func nodesEqual(x, y []*tailcfg.Node) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if !x[i].Equal(y[i]) {
			return false
		}
	}
	return true
}

// SetNetworkMap is called when the control client gets a new network
// map from the control server. It must always be non-nil.
//
// It should not use the DERPMap field of NetworkMap; that's
// conditionally sent to SetDERPMap instead.
func (c *Conn) SetNetworkMap(nm *netmap.NetworkMap) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	if c.netMap != nil && nodesEqual(c.netMap.Peers, nm.Peers) {
		return
	}

	// For disco-capable peers, update the disco endpoint's state and
	// check if the disco key migrated to a new node key.
	numNoDisco := 0
	for _, n := range nm.Peers {
		if n.DiscoKey.IsZero() {
			numNoDisco++
			continue
		}
		if ep, ok := c.peerMap.endpointForDiscoKey(n.DiscoKey); ok && ep.publicKey == n.Key {
			ep.updateFromNode(n)
		} else if ep != nil {
			// Endpoint no longer belongs to the same node. We'll
			// create the new endpoint below.
			c.logf("magicsock: disco key %v changed from node key %v to %v", n.DiscoKey, ep.publicKey.ShortString(), n.Key.ShortString())
			ep.stopAndReset()
			c.peerMap.deleteDiscoEndpoint(ep)
		}
	}

	c.logf("[v1] magicsock: got updated network map; %d peers", len(nm.Peers))
	if numNoDisco != 0 {
		c.logf("[v1] magicsock: %d DERP-only peers (no discokey)", numNoDisco)
	}
	c.netMap = nm

	// Try a pass of just upserting nodes and creating missing
	// endpoints. If the set of nodes is the same, this is an
	// efficient alloc-free update. If the set of nodes is different,
	// we'll fall through to the next pass, which allocates but can
	// handle full set updates.
	for _, n := range nm.Peers {
		if ep, ok := c.peerMap.endpointForNodeKey(n.Key); ok {
			ep.updateFromNode(n)
			continue
		}

		ep := &endpoint{
			c:             c,
			publicKey:     n.Key,
			sentPing:      map[stun.TxID]sentPing{},
			endpointState: map[netaddr.IPPort]*endpointState{},
		}
		if !n.DiscoKey.IsZero() {
			ep.discoKey = n.DiscoKey
			ep.discoShort = n.DiscoKey.ShortString()
		}
		ep.wgEndpoint = (wgkey.Key(n.Key)).HexString()
		ep.initFakeUDPAddr()
		c.logf("magicsock: created endpoint key=%s: disco=%s; %v", n.Key.ShortString(), n.DiscoKey.ShortString(), logger.ArgWriter(func(w *bufio.Writer) {
			const derpPrefix = "127.3.3.40:"
			if strings.HasPrefix(n.DERP, derpPrefix) {
				ipp, _ := netaddr.ParseIPPort(n.DERP)
				regionID := int(ipp.Port())
				code := c.derpRegionCodeLocked(regionID)
				if code != "" {
					code = "(" + code + ")"
				}
				fmt.Fprintf(w, "derp=%v%s ", regionID, code)
			}

			for _, a := range n.AllowedIPs {
				if a.IsSingleIP() {
					fmt.Fprintf(w, "aip=%v ", a.IP())
				} else {
					fmt.Fprintf(w, "aip=%v ", a)
				}
			}
			for _, ep := range n.Endpoints {
				fmt.Fprintf(w, "ep=%v ", ep)
			}
		}))
		ep.updateFromNode(n)
		c.peerMap.upsertDiscoEndpoint(ep)
	}

	// If the set of nodes changed since the last SetNetworkMap, the
	// upsert loop just above made c.peerMap contain the union of the
	// old and new peers - which will be larger than the set from the
	// current netmap. If that happens, go through the allocful
	// deletion path to clean up moribund nodes.
	if c.peerMap.nodeCount() != len(nm.Peers) {
		keep := make(map[tailcfg.NodeKey]bool, len(nm.Peers))
		for _, n := range nm.Peers {
			keep[n.Key] = true
		}
		c.peerMap.forEachDiscoEndpoint(func(ep *endpoint) {
			if !keep[ep.publicKey] {
				c.peerMap.deleteDiscoEndpoint(ep)
				if !ep.discoKey.IsZero() {
					delete(c.sharedDiscoKey, ep.discoKey)
				}
			}
		})
	}
}

func (c *Conn) wantDerpLocked() bool { return c.derpMap != nil }

// c.mu must be held.
func (c *Conn) closeAllDerpLocked(why string) {
	if len(c.activeDerp) == 0 {
		return // without the useless log statement
	}
	for i := range c.activeDerp {
		c.closeDerpLocked(i, why)
	}
	c.logActiveDerpLocked()
}

// c.mu must be held.
// It is the responsibility of the caller to call logActiveDerpLocked after any set of closes.
func (c *Conn) closeDerpLocked(node int, why string) {
	if ad, ok := c.activeDerp[node]; ok {
		c.logf("magicsock: closing connection to derp-%v (%v), age %v", node, why, time.Since(ad.createTime).Round(time.Second))
		go ad.c.Close()
		ad.cancel()
		delete(c.activeDerp, node)
	}
}

// c.mu must be held.
func (c *Conn) logActiveDerpLocked() {
	now := time.Now()
	c.logf("magicsock: %v active derp conns%s", len(c.activeDerp), logger.ArgWriter(func(buf *bufio.Writer) {
		if len(c.activeDerp) == 0 {
			return
		}
		buf.WriteString(":")
		c.foreachActiveDerpSortedLocked(func(node int, ad activeDerp) {
			fmt.Fprintf(buf, " derp-%d=cr%v,wr%v", node, simpleDur(now.Sub(ad.createTime)), simpleDur(now.Sub(*ad.lastWrite)))
		})
	}))
}

func (c *Conn) logEndpointChange(endpoints []tailcfg.Endpoint) {
	c.logf("magicsock: endpoints changed: %s", logger.ArgWriter(func(buf *bufio.Writer) {
		for i, ep := range endpoints {
			if i > 0 {
				buf.WriteString(", ")
			}
			fmt.Fprintf(buf, "%s (%s)", ep.Addr, ep.Type)
		}
	}))
}

// c.mu must be held.
func (c *Conn) foreachActiveDerpSortedLocked(fn func(regionID int, ad activeDerp)) {
	if len(c.activeDerp) < 2 {
		for id, ad := range c.activeDerp {
			fn(id, ad)
		}
		return
	}
	ids := make([]int, 0, len(c.activeDerp))
	for id := range c.activeDerp {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		fn(id, c.activeDerp[id])
	}
}

func (c *Conn) cleanStaleDerp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.derpCleanupTimerArmed = false

	tooOld := time.Now().Add(-derpInactiveCleanupTime)
	dirty := false
	someNonHomeOpen := false
	for i, ad := range c.activeDerp {
		if i == c.myDerp {
			continue
		}
		if ad.lastWrite.Before(tooOld) {
			c.closeDerpLocked(i, "idle")
			dirty = true
		} else {
			someNonHomeOpen = true
		}
	}
	if dirty {
		c.logActiveDerpLocked()
	}
	if someNonHomeOpen {
		c.scheduleCleanStaleDerpLocked()
	}
}

func (c *Conn) scheduleCleanStaleDerpLocked() {
	if c.derpCleanupTimerArmed {
		// Already going to fire soon. Let the existing one
		// fire lest it get infinitely delayed by repeated
		// calls to scheduleCleanStaleDerpLocked.
		return
	}
	c.derpCleanupTimerArmed = true
	if c.derpCleanupTimer != nil {
		c.derpCleanupTimer.Reset(derpCleanStaleInterval)
	} else {
		c.derpCleanupTimer = time.AfterFunc(derpCleanStaleInterval, c.cleanStaleDerp)
	}
}

// DERPs reports the number of active DERP connections.
func (c *Conn) DERPs() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.activeDerp)
}

// Bind returns the wireguard-go conn.Bind for c.
func (c *Conn) Bind() conn.Bind {
	return c.bind
}

// connBind is a wireguard-go conn.Bind for a Conn.
// It bridges the behavior of wireguard-go and a Conn.
// wireguard-go calls Close then Open on device.Up.
// That won't work well for a Conn, which is only closed on shutdown.
// The subsequent Close is a real close.
type connBind struct {
	*Conn
	mu     sync.Mutex
	closed bool
}

// Open is called by WireGuard to create a UDP binding.
// The ignoredPort comes from wireguard-go, via the wgcfg config.
// We ignore that port value here, since we have the local port available easily.
func (c *connBind) Open(ignoredPort uint16) ([]conn.ReceiveFunc, uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		return nil, 0, errors.New("magicsock: connBind already open")
	}
	c.closed = false
	fns := []conn.ReceiveFunc{c.receiveIPv4, c.receiveIPv6, c.receiveDERP}
	// TODO: Combine receiveIPv4 and receiveIPv6 and receiveIP into a single
	// closure that closes over a *RebindingUDPConn?
	return fns, c.LocalPort(), nil
}

// SetMark is used by wireguard-go to set a mark bit for packets to avoid routing loops.
// We handle that ourselves elsewhere.
func (c *connBind) SetMark(value uint32) error {
	return nil
}

// Close closes the connBind, unless it is already closed.
func (c *connBind) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	// Unblock all outstanding receives.
	c.pconn4.Close()
	c.pconn6.Close()
	// Send an empty read result to unblock receiveDERP,
	// which will then check connBind.Closed.
	c.derpRecvCh <- derpReadResult{}
	return nil
}

// Closed reports whether c is closed.
func (c *connBind) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Close closes the connection.
//
// Only the first close does anything. Any later closes return nil.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if c.derpCleanupTimerArmed {
		c.derpCleanupTimer.Stop()
	}
	c.stopPeriodicReSTUNTimerLocked()
	c.portMapper.Close()

	c.peerMap.forEachDiscoEndpoint(func(ep *endpoint) {
		ep.stopAndReset()
	})

	c.closed = true
	c.connCtxCancel()
	c.closeAllDerpLocked("conn-close")
	// Ignore errors from c.pconnN.Close.
	// They will frequently have been closed already by a call to connBind.Close.
	if c.pconn6 != nil {
		c.pconn6.Close()
	}
	c.pconn4.Close()

	// Wait on goroutines updating right at the end, once everything is
	// already closed. We want everything else in the Conn to be
	// consistently in the closed state before we release mu to wait
	// on the endpoint updater & derphttp.Connect.
	for c.goroutinesRunningLocked() {
		c.muCond.Wait()
	}
	return nil
}

func (c *Conn) goroutinesRunningLocked() bool {
	if c.endpointsUpdateActive {
		return true
	}
	// The goroutine running dc.Connect in derpWriteChanOfAddr may linger
	// and appear to leak, as observed in https://github.com/tailscale/tailscale/issues/554.
	// This is despite the underlying context being cancelled by connCtxCancel above.
	// To avoid this condition, we must wait on derpStarted here
	// to ensure that this goroutine has exited by the time Close returns.
	// We only do this if derpWriteChanOfAddr has executed at least once:
	// on the first run, it sets firstDerp := true and spawns the aforementioned goroutine.
	// To detect this, we check activeDerp, which is initialized to non-nil on the first run.
	if c.activeDerp != nil {
		select {
		case <-c.derpStarted:
			break
		default:
			return true
		}
	}
	return false
}

func maxIdleBeforeSTUNShutdown() time.Duration {
	if debugReSTUNStopOnIdle {
		return 45 * time.Second
	}
	return sessionActiveTimeout
}

func (c *Conn) shouldDoPeriodicReSTUNLocked() bool {
	if c.networkDown() {
		return false
	}
	if len(c.peerSet) == 0 || c.privateKey.IsZero() {
		// If no peers, not worth doing.
		// Also don't if there's no key (not running).
		return false
	}
	if f := c.idleFunc; f != nil {
		idleFor := f()
		if debugReSTUNStopOnIdle {
			c.logf("magicsock: periodicReSTUN: idle for %v", idleFor.Round(time.Second))
		}
		if idleFor > maxIdleBeforeSTUNShutdown() {
			if c.netMap != nil && c.netMap.Debug != nil && c.netMap.Debug.ForceBackgroundSTUN {
				// Overridden by control.
				return true
			}
			return false
		}
	}
	return true
}

func (c *Conn) onPortMapChanged() { c.ReSTUN("portmap-changed") }

// ReSTUN triggers an address discovery.
// The provided why string is for debug logging only.
func (c *Conn) ReSTUN(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		// raced with a shutdown.
		return
	}

	// If the user stopped the app, stop doing work. (When the
	// user stops Tailscale via the GUI apps, ipn/local.go
	// reconfigures the engine with a zero private key.)
	//
	// This used to just check c.privateKey.IsZero, but that broke
	// some end-to-end tests tests that didn't ever set a private
	// key somehow. So for now, only stop doing work if we ever
	// had a key, which helps real users, but appeases tests for
	// now. TODO: rewrite those tests to be less brittle or more
	// realistic.
	if c.privateKey.IsZero() && c.everHadKey {
		c.logf("magicsock: ReSTUN(%q) ignored; stopped, no private key", why)
		return
	}

	if c.endpointsUpdateActive {
		if c.wantEndpointsUpdate != why {
			c.logf("[v1] magicsock: ReSTUN: endpoint update active, need another later (%q)", why)
			c.wantEndpointsUpdate = why
		}
	} else {
		c.endpointsUpdateActive = true
		go c.updateEndpoints(why)
	}
}

func (c *Conn) initialBind() error {
	if err := c.bindSocket(&c.pconn4, "udp4", keepCurrentPort); err != nil {
		return fmt.Errorf("magicsock: initialBind IPv4 failed: %w", err)
	}
	c.portMapper.SetLocalPort(c.LocalPort())
	if err := c.bindSocket(&c.pconn6, "udp6", keepCurrentPort); err != nil {
		c.logf("magicsock: ignoring IPv6 bind failure: %v", err)
	}
	return nil
}

// listenPacket opens a packet listener.
// The network must be "udp4" or "udp6".
func (c *Conn) listenPacket(network string, port uint16) (net.PacketConn, error) {
	ctx := context.Background() // unused without DNS name to resolve
	addr := net.JoinHostPort("", fmt.Sprint(port))
	if c.testOnlyPacketListener != nil {
		return c.testOnlyPacketListener.ListenPacket(ctx, network, addr)
	}
	return netns.Listener().ListenPacket(ctx, network, addr)
}

// bindSocket initializes rucPtr if necessary and binds a UDP socket to it.
// Network indicates the UDP socket type; it must be "udp4" or "udp6".
// If rucPtr had an existing UDP socket bound, it closes that socket.
// The caller is responsible for informing the portMapper of any changes.
// If curPortFate is set to dropCurrentPort, no attempt is made to reuse
// the current port.
func (c *Conn) bindSocket(rucPtr **RebindingUDPConn, network string, curPortFate currentPortFate) error {
	if *rucPtr == nil {
		*rucPtr = new(RebindingUDPConn)
	}
	ruc := *rucPtr

	// Hold the ruc lock the entire time, so that the close+bind is atomic
	// from the perspective of ruc receive functions.
	ruc.mu.Lock()
	defer ruc.mu.Unlock()

	if debugAlwaysDERP {
		c.logf("disabled %v per TS_DEBUG_ALWAYS_USE_DERP", network)
		ruc.pconn = newBlockForeverConn()
		return nil
	}

	// Build a list of preferred ports.
	// Best is the port that the user requested.
	// Second best is the port that is currently in use.
	// If those fail, fall back to 0.
	var ports []uint16
	if port := uint16(c.port.Get()); port != 0 {
		ports = append(ports, port)
	}
	if ruc.pconn != nil && curPortFate == keepCurrentPort {
		curPort := uint16(ruc.localAddrLocked().Port)
		ports = append(ports, curPort)
	}
	ports = append(ports, 0)
	// Remove duplicates. (All duplicates are consecutive.)
	uniq.ModifySlice(&ports, func(i, j int) bool { return ports[i] == ports[j] })

	var pconn net.PacketConn
	for _, port := range ports {
		// Close the existing conn, in case it is sitting on the port we want.
		err := ruc.closeLocked()
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, errNilPConn) {
			c.logf("magicsock: bindSocket %v close failed: %v", network, err)
		}
		// Open a new one with the desired port.
		pconn, err = c.listenPacket(network, port)
		if err != nil {
			c.logf("magicsock: unable to bind %v port %d: %v", network, port, err)
			continue
		}
		// Success.
		ruc.pconn = pconn
		if network == "udp4" {
			health.SetUDP4Unbound(false)
		}
		return nil
	}

	// Failed to bind, including on port 0 (!).
	// Set pconn to a dummy conn whose reads block until closed.
	// This keeps the receive funcs alive for a future in which
	// we get a link change and we can try binding again.
	ruc.pconn = newBlockForeverConn()
	if network == "udp4" {
		health.SetUDP4Unbound(true)
	}
	return fmt.Errorf("failed to bind any ports (tried %v)", ports)
}

type currentPortFate uint8

const (
	keepCurrentPort = currentPortFate(0)
	dropCurrentPort = currentPortFate(1)
)

// rebind closes and re-binds the UDP sockets.
// We consider it successful if we manage to bind the IPv4 socket.
func (c *Conn) rebind(curPortFate currentPortFate) error {
	if err := c.bindSocket(&c.pconn4, "udp4", curPortFate); err != nil {
		return fmt.Errorf("magicsock: Rebind IPv4 failed: %w", err)
	}
	c.portMapper.SetLocalPort(c.LocalPort())
	if err := c.bindSocket(&c.pconn6, "udp6", curPortFate); err != nil {
		c.logf("magicsock: Rebind ignoring IPv6 bind failure: %v", err)
	}
	return nil
}

// Rebind closes and re-binds the UDP sockets and resets the DERP connection.
// It should be followed by a call to ReSTUN.
func (c *Conn) Rebind() {
	if err := c.rebind(keepCurrentPort); err != nil {
		c.logf("%w", err)
		return
	}

	c.mu.Lock()
	c.closeAllDerpLocked("rebind")
	if !c.privateKey.IsZero() {
		c.startDerpHomeConnectLocked()
	}
	c.mu.Unlock()

	c.resetEndpointStates()
}

// resetEndpointStates resets the preferred address for all peers.
// This is called when connectivity changes enough that we no longer
// trust the old routes.
func (c *Conn) resetEndpointStates() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peerMap.forEachDiscoEndpoint(func(ep *endpoint) {
		ep.noteConnectivityChange()
	})
}

// packIPPort packs an IPPort into the form wanted by WireGuard.
func packIPPort(ua netaddr.IPPort) []byte {
	ip := ua.IP().Unmap()
	a := ip.As16()
	ipb := a[:]
	if ip.Is4() {
		ipb = ipb[12:]
	}
	b := make([]byte, 0, len(ipb)+2)
	b = append(b, ipb...)
	b = append(b, byte(ua.Port()))
	b = append(b, byte(ua.Port()>>8))
	return b
}

// ParseEndpoint is called by WireGuard to connect to an endpoint.
func (c *Conn) ParseEndpoint(nodeKeyStr string) (conn.Endpoint, error) {
	k, err := wgkey.ParseHex(nodeKeyStr)
	if err != nil {
		return nil, fmt.Errorf("magicsock: ParseEndpoint: parse failed on %q: %w", nodeKeyStr, err)
	}
	pk := tailcfg.NodeKey(k)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errConnClosed
	}
	ep, ok := c.peerMap.endpointForNodeKey(tailcfg.NodeKey(pk))
	if !ok {
		// We should never be telling WireGuard about a new peer
		// before magicsock knows about it.
		c.logf("[unexpected] magicsock: ParseEndpoint: unknown node key=%s", pk.ShortString())
		return nil, fmt.Errorf("magicsock: ParseEndpoint: unknown peer %q", pk.ShortString())
	}

	return ep, nil
}

// RebindingUDPConn is a UDP socket that can be re-bound.
// Unix has no notion of re-binding a socket, so we swap it out for a new one.
type RebindingUDPConn struct {
	mu    sync.Mutex
	pconn net.PacketConn
}

// currentConn returns c's current pconn.
func (c *RebindingUDPConn) currentConn() net.PacketConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pconn
}

// ReadFrom reads a packet from c into b.
// It returns the number of bytes copied and the source address.
func (c *RebindingUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		pconn := c.currentConn()
		n, addr, err := pconn.ReadFrom(b)
		if err != nil && pconn != c.currentConn() {
			continue
		}
		return n, addr, err
	}
}

// ReadFromNetaddr reads a packet from c into b.
// It returns the number of bytes copied and the return address.
// It is identical to c.ReadFrom, except that it returns a netaddr.IPPort instead of a net.Addr.
// ReadFromNetaddr is designed to work with specific underlying connection types.
// If c's underlying connection returns a non-*net.UPDAddr return address, ReadFromNetaddr will return an error.
// ReadFromNetaddr exists because it removes an allocation per read,
// when c's underlying connection is a net.UDPConn.
func (c *RebindingUDPConn) ReadFromNetaddr(b []byte) (n int, ipp netaddr.IPPort, err error) {
	for {
		pconn := c.currentConn()

		// Optimization: Treat *net.UDPConn specially.
		// ReadFromUDP gets partially inlined, avoiding allocating a *net.UDPAddr,
		// as long as pAddr itself doesn't escape.
		// The non-*net.UDPConn case works, but it allocates.
		var pAddr *net.UDPAddr
		if udpConn, ok := pconn.(*net.UDPConn); ok {
			n, pAddr, err = udpConn.ReadFromUDP(b)
		} else {
			var addr net.Addr
			n, addr, err = pconn.ReadFrom(b)
			if addr != nil {
				pAddr, ok = addr.(*net.UDPAddr)
				if !ok {
					return 0, netaddr.IPPort{}, fmt.Errorf("RebindingUDPConn.ReadFromNetaddr: underlying connection returned address of type %T, want *netaddr.UDPAddr", addr)
				}
			}
		}

		if err != nil {
			if pconn != c.currentConn() {
				continue
			}
		} else {
			// Convert pAddr to a netaddr.IPPort.
			// This prevents pAddr from escaping.
			var ok bool
			ipp, ok = netaddr.FromStdAddr(pAddr.IP, pAddr.Port, pAddr.Zone)
			if !ok {
				return 0, netaddr.IPPort{}, errors.New("netaddr.FromStdAddr failed")
			}
		}
		return n, ipp, err
	}
}

func (c *RebindingUDPConn) LocalAddr() *net.UDPAddr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localAddrLocked()
}

func (c *RebindingUDPConn) localAddrLocked() *net.UDPAddr {
	return c.pconn.LocalAddr().(*net.UDPAddr)
}

// errNilPConn is returned by RebindingUDPConn.Close when there is no current pconn.
// It is for internal use only and should not be returned to users.
var errNilPConn = errors.New("nil pconn")

func (c *RebindingUDPConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *RebindingUDPConn) closeLocked() error {
	if c.pconn == nil {
		return errNilPConn
	}
	return c.pconn.Close()
}

func (c *RebindingUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	for {
		c.mu.Lock()
		pconn := c.pconn
		c.mu.Unlock()

		n, err := pconn.WriteTo(b, addr)
		if err != nil {
			c.mu.Lock()
			pconn2 := c.pconn
			c.mu.Unlock()

			if pconn != pconn2 {
				continue
			}
		}
		return n, err
	}
}

func newBlockForeverConn() *blockForeverConn {
	c := new(blockForeverConn)
	c.cond = sync.NewCond(&c.mu)
	return c
}

// blockForeverConn is a net.PacketConn whose reads block until it is closed.
type blockForeverConn struct {
	mu     sync.Mutex
	cond   *sync.Cond
	closed bool
}

func (c *blockForeverConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	c.mu.Lock()
	for !c.closed {
		c.cond.Wait()
	}
	c.mu.Unlock()
	return 0, nil, net.ErrClosed
}

func (c *blockForeverConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	// Silently drop writes.
	return len(p), nil
}

func (c *blockForeverConn) LocalAddr() net.Addr {
	// Return a *net.UDPAddr because lots of code assumes that it will.
	return new(net.UDPAddr)
}

func (c *blockForeverConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.closed = true
	return nil
}

func (c *blockForeverConn) SetDeadline(t time.Time) error      { return errors.New("unimplemented") }
func (c *blockForeverConn) SetReadDeadline(t time.Time) error  { return errors.New("unimplemented") }
func (c *blockForeverConn) SetWriteDeadline(t time.Time) error { return errors.New("unimplemented") }

// simpleDur rounds d such that it stringifies to something short.
func simpleDur(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	if d < time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(time.Minute)
}

func peerShort(k key.Public) string {
	k2 := wgkey.Key(k)
	return k2.ShortString()
}

func sbPrintAddr(sb *strings.Builder, a netaddr.IPPort) {
	is6 := a.IP().Is6()
	if is6 {
		sb.WriteByte('[')
	}
	fmt.Fprintf(sb, "%s", a.IP())
	if is6 {
		sb.WriteByte(']')
	}
	fmt.Fprintf(sb, ":%d", a.Port())
}

func (c *Conn) derpRegionCodeOfAddrLocked(ipPort string) string {
	_, portStr, err := net.SplitHostPort(ipPort)
	if err != nil {
		return ""
	}
	regionID, err := strconv.Atoi(portStr)
	if err != nil {
		return ""
	}
	return c.derpRegionCodeOfIDLocked(regionID)
}

func (c *Conn) derpRegionCodeOfIDLocked(regionID int) string {
	if c.derpMap == nil {
		return ""
	}
	if r, ok := c.derpMap.Regions[regionID]; ok {
		return r.RegionCode
	}
	return ""
}

func (c *Conn) UpdateStatus(sb *ipnstate.StatusBuilder) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var tailAddr4 string
	var tailscaleIPs []netaddr.IP
	if c.netMap != nil {
		tailscaleIPs = make([]netaddr.IP, 0, len(c.netMap.Addresses))
		for _, addr := range c.netMap.Addresses {
			if !addr.IsSingleIP() {
				continue
			}
			sb.AddTailscaleIP(addr.IP())
			// TailAddr previously only allowed for a
			// single Tailscale IP. For compatibility for
			// a couple releases starting with 1.8, keep
			// that field pulled out separately.
			if addr.IP().Is4() {
				tailAddr4 = addr.IP().String()
			}
			tailscaleIPs = append(tailscaleIPs, addr.IP())
		}
	}

	sb.MutateSelfStatus(func(ss *ipnstate.PeerStatus) {
		ss.PublicKey = c.privateKey.Public()
		ss.Addrs = make([]string, 0, len(c.lastEndpoints))
		for _, ep := range c.lastEndpoints {
			ss.Addrs = append(ss.Addrs, ep.Addr.String())
		}
		ss.OS = version.OS()
		if c.netMap != nil {
			ss.HostName = c.netMap.Hostinfo.Hostname
			ss.DNSName = c.netMap.Name
			ss.UserID = c.netMap.User
			if c.netMap.SelfNode != nil {
				if c := c.netMap.SelfNode.Capabilities; len(c) > 0 {
					ss.Capabilities = append([]string(nil), c...)
				}
			}
		} else {
			ss.HostName, _ = os.Hostname()
		}
		if c.derpMap != nil {
			derpRegion, ok := c.derpMap.Regions[c.myDerp]
			if ok {
				ss.Relay = derpRegion.RegionCode
			}
		}
		ss.TailscaleIPs = tailscaleIPs
		ss.TailAddrDeprecated = tailAddr4
	})

	c.peerMap.forEachDiscoEndpoint(func(ep *endpoint) {
		ps := &ipnstate.PeerStatus{InMagicSock: true}
		//ps.Addrs = append(ps.Addrs, n.Endpoints...)
		ep.populatePeerStatus(ps)
		sb.AddPeer(key.Public(ep.publicKey), ps)
	})

	c.foreachActiveDerpSortedLocked(func(node int, ad activeDerp) {
		// TODO(bradfitz): add to ipnstate.StatusBuilder
		//f("<li><b>derp-%v</b>: cr%v,wr%v</li>", node, simpleDur(now.Sub(ad.createTime)), simpleDur(now.Sub(*ad.lastWrite)))
	})
}

func ippDebugString(ua netaddr.IPPort) string {
	if ua.IP() == derpMagicIPAddr {
		return fmt.Sprintf("derp-%d", ua.Port())
	}
	return ua.String()
}

// discoEndpoint is a wireguard/conn.Endpoint that picks the best
// available path to communicate with a peer, based on network
// conditions and what the peer supports.
type endpoint struct {
	// atomically accessed; declared first for alignment reasons
	lastRecv              mono.Time
	numStopAndResetAtomic int64

	// These fields are initialized once and never modified.
	c          *Conn
	publicKey  tailcfg.NodeKey  // peer public key (for WireGuard + DERP)
	discoKey   tailcfg.DiscoKey // for discovery messages. IsZero() if peer can't disco.
	discoShort string           // ShortString of discoKey. Empty if peer can't disco.
	fakeWGAddr netaddr.IPPort   // the UDP address we tell wireguard-go we're using
	wgEndpoint string           // string from ParseEndpoint, holds a JSON-serialized wgcfg.Endpoints

	// Owned by Conn.mu:
	lastPingFrom netaddr.IPPort
	lastPingTime time.Time

	// mu protects all following fields.
	mu sync.Mutex // Lock ordering: Conn.mu, then endpoint.mu

	heartBeatTimer *time.Timer    // nil when idle
	lastSend       mono.Time      // last time there was outgoing packets sent to this peer (from wireguard-go)
	lastFullPing   mono.Time      // last time we pinged all endpoints
	derpAddr       netaddr.IPPort // fallback/bootstrap path, if non-zero (non-zero for well-behaved clients)

	bestAddr           addrLatency // best non-DERP path; zero if none
	bestAddrAt         mono.Time   // time best address re-confirmed
	trustBestAddrUntil mono.Time   // time when bestAddr expires
	sentPing           map[stun.TxID]sentPing
	endpointState      map[netaddr.IPPort]*endpointState
	isCallMeMaybeEP    map[netaddr.IPPort]bool

	pendingCLIPings []pendingCLIPing // any outstanding "tailscale ping" commands running
}

type pendingCLIPing struct {
	res *ipnstate.PingResult
	cb  func(*ipnstate.PingResult)
}

const (
	// sessionActiveTimeout is how long since the last activity we
	// try to keep an established endpoint peering alive.
	// It's also the idle time at which we stop doing STUN queries to
	// keep NAT mappings alive.
	sessionActiveTimeout = 2 * time.Minute

	// upgradeInterval is how often we try to upgrade to a better path
	// even if we have some non-DERP route that works.
	upgradeInterval = 1 * time.Minute

	// heartbeatInterval is how often pings to the best UDP address
	// are sent.
	heartbeatInterval = 2 * time.Second

	// discoPingInterval is the minimum time between pings
	// to an endpoint. (Except in the case of CallMeMaybe frames
	// resetting the counter, as the first pings likely didn't through
	// the firewall)
	discoPingInterval = 5 * time.Second

	// pingTimeoutDuration is how long we wait for a pong reply before
	// assuming it's never coming.
	pingTimeoutDuration = 5 * time.Second

	// trustUDPAddrDuration is how long we trust a UDP address as the exclusive
	// path (without using DERP) without having heard a Pong reply.
	trustUDPAddrDuration = 5 * time.Second

	// goodEnoughLatency is the latency at or under which we don't
	// try to upgrade to a better path.
	goodEnoughLatency = 5 * time.Millisecond

	// derpInactiveCleanupTime is how long a non-home DERP connection
	// needs to be idle (last written to) before we close it.
	derpInactiveCleanupTime = 60 * time.Second

	// derpCleanStaleInterval is how often cleanStaleDerp runs when there
	// are potentially-stale DERP connections to close.
	derpCleanStaleInterval = 15 * time.Second

	// endpointsFreshEnoughDuration is how long we consider a
	// STUN-derived endpoint valid for. UDP NAT mappings typically
	// expire at 30 seconds, so this is a few seconds shy of that.
	endpointsFreshEnoughDuration = 27 * time.Second
)

// endpointState is some state and history for a specific endpoint of
// a endpoint. (The subject is the endpoint.endpointState
// map key)
type endpointState struct {
	// all fields guarded by endpoint.mu

	// lastPing is the last (outgoing) ping time.
	lastPing mono.Time

	// lastGotPing, if non-zero, means that this was an endpoint
	// that we learned about at runtime (from an incoming ping)
	// and that is not in the network map. If so, we keep the time
	// updated and use it to discard old candidates.
	lastGotPing time.Time

	// callMeMaybeTime, if non-zero, is the time this endpoint
	// was advertised last via a call-me-maybe disco message.
	callMeMaybeTime time.Time

	recentPongs []pongReply // ring buffer up to pongHistoryCount entries
	recentPong  uint16      // index into recentPongs of most recent; older before, wrapped

	index int16 // index in nodecfg.Node.Endpoints; meaningless if lastGotPing non-zero
}

// indexSentinelDeleted is the temporary value that endpointState.index takes while
// a endpoint's endpoints are being updated from a new network map.
const indexSentinelDeleted = -1

// shouldDeleteLocked reports whether we should delete this endpoint.
func (st *endpointState) shouldDeleteLocked() bool {
	switch {
	case !st.callMeMaybeTime.IsZero():
		return false
	case st.lastGotPing.IsZero():
		// This was an endpoint from the network map. Is it still in the network map?
		return st.index == indexSentinelDeleted
	default:
		// This was an endpoint discovered at runtime.
		return time.Since(st.lastGotPing) > sessionActiveTimeout
	}
}

func (de *endpoint) deleteEndpointLocked(ep netaddr.IPPort) {
	delete(de.endpointState, ep)
	if de.bestAddr.IPPort == ep {
		de.bestAddr = addrLatency{}
	}
}

// pongHistoryCount is how many pongReply values we keep per endpointState
const pongHistoryCount = 64

type pongReply struct {
	latency time.Duration
	pongAt  mono.Time      // when we received the pong
	from    netaddr.IPPort // the pong's src (usually same as endpoint map key)
	pongSrc netaddr.IPPort // what they reported they heard
}

type sentPing struct {
	to      netaddr.IPPort
	at      mono.Time
	timer   *time.Timer // timeout timer
	purpose discoPingPurpose
}

// initFakeUDPAddr populates fakeWGAddr with a globally unique fake UDPAddr.
// The current implementation just uses the pointer value of de jammed into an IPv6
// address, but it could also be, say, a counter.
func (de *endpoint) initFakeUDPAddr() {
	var addr [16]byte
	addr[0] = 0xfd
	addr[1] = 0x00
	binary.BigEndian.PutUint64(addr[2:], uint64(reflect.ValueOf(de).Pointer()))
	de.fakeWGAddr = netaddr.IPPortFrom(netaddr.IPFrom16(addr), 12345)
}

// noteRecvActivity records receive activity on de, and invokes
// Conn.noteRecvActivity no more than once every 10s.
func (de *endpoint) noteRecvActivity() {
	if de.c.noteRecvActivity == nil {
		return
	}
	now := mono.Now()
	elapsed := now.Sub(de.lastRecv.LoadAtomic())
	if elapsed > 10*time.Second {
		de.lastRecv.StoreAtomic(now)
		de.c.noteRecvActivity(de.publicKey)
	}
}

// String exists purely so wireguard-go internals can log.Printf("%v")
// its internal conn.Endpoints and we don't end up with data races
// from fmt (via log) reading mutex fields and such.
func (de *endpoint) String() string {
	return fmt.Sprintf("magicsock.endpoint{%v, %v}", de.publicKey.ShortString(), de.discoShort)
}

func (de *endpoint) ClearSrc()           {}
func (de *endpoint) SrcToString() string { panic("unused") } // unused by wireguard-go
func (de *endpoint) SrcIP() net.IP       { panic("unused") } // unused by wireguard-go
func (de *endpoint) DstToString() string { return de.wgEndpoint }
func (de *endpoint) DstIP() net.IP       { panic("unused") }
func (de *endpoint) DstToBytes() []byte  { return packIPPort(de.fakeWGAddr) }

// canP2P reports whether this endpoint understands the disco protocol
// and is expected to speak it.
//
// As of 2021-08-25, only a few hundred pre-0.100 clients understand
// DERP but not disco, so this returns false very rarely.
func (de *endpoint) canP2P() bool {
	return !de.discoKey.IsZero()
}

// addrForSendLocked returns the address(es) that should be used for
// sending the next packet. Zero, one, or both of UDP address and DERP
// addr may be non-zero.
//
// de.mu must be held.
func (de *endpoint) addrForSendLocked(now mono.Time) (udpAddr, derpAddr netaddr.IPPort) {
	udpAddr = de.bestAddr.IPPort
	if udpAddr.IsZero() || now.After(de.trustBestAddrUntil) {
		// We had a bestAddr but it expired so send both to it
		// and DERP.
		derpAddr = de.derpAddr
	}
	return
}

// heartbeat is called every heartbeatInterval to keep the best UDP path alive,
// or kick off discovery of other paths.
func (de *endpoint) heartbeat() {
	de.mu.Lock()
	defer de.mu.Unlock()

	de.heartBeatTimer = nil

	if !de.canP2P() {
		// Cannot form p2p connections, no heartbeating necessary.
		return
	}

	if de.lastSend.IsZero() {
		// Shouldn't happen.
		return
	}

	if mono.Since(de.lastSend) > sessionActiveTimeout {
		// Session's idle. Stop heartbeating.
		de.c.logf("[v1] magicsock: disco: ending heartbeats for idle session to %v (%v)", de.publicKey.ShortString(), de.discoShort)
		return
	}

	now := mono.Now()
	udpAddr, _ := de.addrForSendLocked(now)
	if !udpAddr.IsZero() {
		// We have a preferred path. Ping that every 2 seconds.
		de.startPingLocked(udpAddr, now, pingHeartbeat)
	}

	if de.wantFullPingLocked(now) {
		de.sendPingsLocked(now, true)
	}

	de.heartBeatTimer = time.AfterFunc(heartbeatInterval, de.heartbeat)
}

// wantFullPingLocked reports whether we should ping to all our peers looking for
// a better path.
//
// de.mu must be held.
func (de *endpoint) wantFullPingLocked(now mono.Time) bool {
	if !de.canP2P() {
		return false
	}
	if de.bestAddr.IsZero() || de.lastFullPing.IsZero() {
		return true
	}
	if now.After(de.trustBestAddrUntil) {
		return true
	}
	if de.bestAddr.latency <= goodEnoughLatency {
		return false
	}
	if now.Sub(de.lastFullPing) >= upgradeInterval {
		return true
	}
	return false
}

func (de *endpoint) noteActiveLocked() {
	de.lastSend = mono.Now()
	if de.heartBeatTimer == nil && de.canP2P() {
		de.heartBeatTimer = time.AfterFunc(heartbeatInterval, de.heartbeat)
	}
}

// cliPing starts a ping for the "tailscale ping" command. res is value to call cb with,
// already partially filled.
func (de *endpoint) cliPing(res *ipnstate.PingResult, cb func(*ipnstate.PingResult)) {
	de.mu.Lock()
	defer de.mu.Unlock()

	de.pendingCLIPings = append(de.pendingCLIPings, pendingCLIPing{res, cb})

	now := mono.Now()
	udpAddr, derpAddr := de.addrForSendLocked(now)
	if !derpAddr.IsZero() {
		de.startPingLocked(derpAddr, now, pingCLI)
	}
	if !udpAddr.IsZero() && now.Before(de.trustBestAddrUntil) {
		// Already have an active session, so just ping the address we're using.
		// Otherwise "tailscale ping" results to a node on the local network
		// can look like they're bouncing between, say 10.0.0.0/9 and the peer's
		// IPv6 address, both 1ms away, and it's random who replies first.
		de.startPingLocked(udpAddr, now, pingCLI)
	} else if de.canP2P() {
		for ep := range de.endpointState {
			de.startPingLocked(ep, now, pingCLI)
		}
	}
	de.noteActiveLocked()
}

func (de *endpoint) send(b []byte) error {
	now := mono.Now()

	de.mu.Lock()
	udpAddr, derpAddr := de.addrForSendLocked(now)
	if de.canP2P() && (udpAddr.IsZero() || now.After(de.trustBestAddrUntil)) {
		de.sendPingsLocked(now, true)
	}
	de.noteActiveLocked()
	de.mu.Unlock()

	if udpAddr.IsZero() && derpAddr.IsZero() {
		return errors.New("no UDP or DERP addr")
	}
	var err error
	if !udpAddr.IsZero() {
		_, err = de.c.sendAddr(udpAddr, key.Public(de.publicKey), b)
	}
	if !derpAddr.IsZero() {
		if ok, _ := de.c.sendAddr(derpAddr, key.Public(de.publicKey), b); ok && err != nil {
			// UDP failed but DERP worked, so good enough:
			return nil
		}
	}
	return err
}

func (de *endpoint) pingTimeout(txid stun.TxID) {
	de.mu.Lock()
	defer de.mu.Unlock()
	sp, ok := de.sentPing[txid]
	if !ok {
		return
	}
	if debugDisco || de.bestAddr.IsZero() || mono.Now().After(de.trustBestAddrUntil) {
		de.c.logf("[v1] magicsock: disco: timeout waiting for pong %x from %v (%v, %v)", txid[:6], sp.to, de.publicKey.ShortString(), de.discoShort)
	}
	de.removeSentPingLocked(txid, sp)
}

// forgetPing is called by a timer when a ping either fails to send or
// has taken too long to get a pong reply.
func (de *endpoint) forgetPing(txid stun.TxID) {
	de.mu.Lock()
	defer de.mu.Unlock()
	if sp, ok := de.sentPing[txid]; ok {
		de.removeSentPingLocked(txid, sp)
	}
}

func (de *endpoint) removeSentPingLocked(txid stun.TxID, sp sentPing) {
	// Stop the timer for the case where sendPing failed to write to UDP.
	// In the case of a timer already having fired, this is a no-op:
	sp.timer.Stop()
	delete(de.sentPing, txid)
}

// sendDiscoPing sends a ping with the provided txid to ep.
//
// The caller (startPingLocked) should've already been recorded the ping in
// sentPing and set up the timer.
func (de *endpoint) sendDiscoPing(ep netaddr.IPPort, txid stun.TxID, logLevel discoLogLevel) {
	sent, _ := de.sendDiscoMessage(ep, &disco.Ping{TxID: [12]byte(txid)}, logLevel)
	if !sent {
		de.forgetPing(txid)
	}
}

// discoPingPurpose is the reason why a discovery ping message was sent.
type discoPingPurpose int

//go:generate go run tailscale.com/cmd/addlicense -year 2020 -file discopingpurpose_string.go go run golang.org/x/tools/cmd/stringer -type=discoPingPurpose -trimprefix=ping
const (
	// pingDiscovery means that purpose of a ping was to see if a
	// path was valid.
	pingDiscovery discoPingPurpose = iota

	// pingHeartbeat means that purpose of a ping was whether a
	// peer was still there.
	pingHeartbeat

	// pingCLI means that the user is running "tailscale ping"
	// from the CLI. These types of pings can go over DERP.
	pingCLI
)

func (de *endpoint) startPingLocked(ep netaddr.IPPort, now mono.Time, purpose discoPingPurpose) {
	if !de.canP2P() {
		panic("tried to disco ping a peer that can't disco")
	}
	if purpose != pingCLI {
		st, ok := de.endpointState[ep]
		if !ok {
			// Shouldn't happen. But don't ping an endpoint that's
			// not active for us.
			de.c.logf("magicsock: disco: [unexpected] attempt to ping no longer live endpoint %v", ep)
			return
		}
		st.lastPing = now
	}

	txid := stun.NewTxID()
	de.sentPing[txid] = sentPing{
		to:      ep,
		at:      now,
		timer:   time.AfterFunc(pingTimeoutDuration, func() { de.pingTimeout(txid) }),
		purpose: purpose,
	}
	logLevel := discoLog
	if purpose == pingHeartbeat {
		logLevel = discoVerboseLog
	}
	go de.sendDiscoPing(ep, txid, logLevel)
}

func (de *endpoint) sendPingsLocked(now mono.Time, sendCallMeMaybe bool) {
	de.lastFullPing = now
	var sentAny bool
	for ep, st := range de.endpointState {
		if st.shouldDeleteLocked() {
			de.deleteEndpointLocked(ep)
			continue
		}
		if !st.lastPing.IsZero() && now.Sub(st.lastPing) < discoPingInterval {
			continue
		}

		firstPing := !sentAny
		sentAny = true

		if firstPing && sendCallMeMaybe {
			de.c.logf("[v1] magicsock: disco: send, starting discovery for %v (%v)", de.publicKey.ShortString(), de.discoShort)
		}

		de.startPingLocked(ep, now, pingDiscovery)
	}
	derpAddr := de.derpAddr
	if sentAny && sendCallMeMaybe && !derpAddr.IsZero() {
		// Have our magicsock.Conn figure out its STUN endpoint (if
		// it doesn't know already) and then send a CallMeMaybe
		// message to our peer via DERP informing them that we've
		// sent so our firewall ports are probably open and now
		// would be a good time for them to connect.
		go de.c.enqueueCallMeMaybe(derpAddr, de)
	}
}

func (de *endpoint) sendDiscoMessage(dst netaddr.IPPort, dm disco.Message, logLevel discoLogLevel) (sent bool, err error) {
	return de.c.sendDiscoMessage(dst, de.publicKey, de.discoKey, dm, logLevel)
}

func (de *endpoint) updateFromNode(n *tailcfg.Node) {
	if n == nil {
		panic("nil node when updating disco ep")
	}
	de.mu.Lock()
	defer de.mu.Unlock()

	if n.DERP == "" {
		de.derpAddr = netaddr.IPPort{}
	} else {
		de.derpAddr, _ = netaddr.ParseIPPort(n.DERP)
	}

	for _, st := range de.endpointState {
		st.index = indexSentinelDeleted // assume deleted until updated in next loop
	}
	for i, epStr := range n.Endpoints {
		if i > math.MaxInt16 {
			// Seems unlikely.
			continue
		}
		ipp, err := netaddr.ParseIPPort(epStr)
		if err != nil {
			de.c.logf("magicsock: bogus netmap endpoint %q", epStr)
			continue
		}
		if st, ok := de.endpointState[ipp]; ok {
			st.index = int16(i)
		} else {
			de.endpointState[ipp] = &endpointState{index: int16(i)}
		}
	}

	// Now delete anything unless it's still in the network map or
	// was a recently discovered endpoint.
	for ep, st := range de.endpointState {
		if st.shouldDeleteLocked() {
			de.deleteEndpointLocked(ep)
		}
	}
}

// addCandidateEndpoint adds ep as an endpoint to which we should send
// future pings.
//
// This is called once we've already verified that we got a valid
// discovery message from de via ep.
func (de *endpoint) addCandidateEndpoint(ep netaddr.IPPort) {
	de.mu.Lock()
	defer de.mu.Unlock()

	if st, ok := de.endpointState[ep]; ok {
		if st.lastGotPing.IsZero() {
			// Already-known endpoint from the network map.
			return
		}
		st.lastGotPing = time.Now()
		return
	}

	// Newly discovered endpoint. Exciting!
	de.c.logf("[v1] magicsock: disco: adding %v as candidate endpoint for %v (%s)", ep, de.discoShort, de.publicKey.ShortString())
	de.endpointState[ep] = &endpointState{
		lastGotPing: time.Now(),
	}

	// If for some reason this gets very large, do some cleanup.
	if size := len(de.endpointState); size > 100 {
		for ep, st := range de.endpointState {
			if st.shouldDeleteLocked() {
				de.deleteEndpointLocked(ep)
			}
		}
		size2 := len(de.endpointState)
		de.c.logf("[v1] magicsock: disco: addCandidateEndpoint pruned %v candidate set from %v to %v entries", size, size2)
	}
}

// noteConnectivityChange is called when connectivity changes enough
// that we should question our earlier assumptions about which paths
// work.
func (de *endpoint) noteConnectivityChange() {
	de.mu.Lock()
	defer de.mu.Unlock()

	de.trustBestAddrUntil = 0
}

// handlePongConnLocked handles a Pong message (a reply to an earlier ping).
// It should be called with the Conn.mu held.
func (de *endpoint) handlePongConnLocked(m *disco.Pong, src netaddr.IPPort) {
	de.mu.Lock()
	defer de.mu.Unlock()

	isDerp := src.IP() == derpMagicIPAddr

	sp, ok := de.sentPing[m.TxID]
	if !ok {
		// This is not a pong for a ping we sent. Ignore.
		return
	}
	de.removeSentPingLocked(m.TxID, sp)

	now := mono.Now()
	latency := now.Sub(sp.at)

	if !isDerp {
		st, ok := de.endpointState[sp.to]
		if !ok {
			// This is no longer an endpoint we care about.
			return
		}

		de.c.setAddrToDiscoLocked(src, de.discoKey)

		st.addPongReplyLocked(pongReply{
			latency: latency,
			pongAt:  now,
			from:    src,
			pongSrc: m.Src,
		})
	}

	if sp.purpose != pingHeartbeat {
		de.c.logf("[v1] magicsock: disco: %v<-%v (%v, %v)  got pong tx=%x latency=%v pong.src=%v%v", de.c.discoShort, de.discoShort, de.publicKey.ShortString(), src, m.TxID[:6], latency.Round(time.Millisecond), m.Src, logger.ArgWriter(func(bw *bufio.Writer) {
			if sp.to != src {
				fmt.Fprintf(bw, " ping.to=%v", sp.to)
			}
		}))
	}

	for _, pp := range de.pendingCLIPings {
		de.c.populateCLIPingResponseLocked(pp.res, latency, sp.to)
		go pp.cb(pp.res)
	}
	de.pendingCLIPings = nil

	// Promote this pong response to our current best address if it's lower latency.
	// TODO(bradfitz): decide how latency vs. preference order affects decision
	if !isDerp {
		thisPong := addrLatency{sp.to, latency}
		if betterAddr(thisPong, de.bestAddr) {
			de.c.logf("magicsock: disco: node %v %v now using %v", de.publicKey.ShortString(), de.discoShort, sp.to)
			de.bestAddr = thisPong
		}
		if de.bestAddr.IPPort == thisPong.IPPort {
			de.bestAddr.latency = latency
			de.bestAddrAt = now
			de.trustBestAddrUntil = now.Add(trustUDPAddrDuration)
		}
	}
}

// addrLatency is an IPPort with an associated latency.
type addrLatency struct {
	netaddr.IPPort
	latency time.Duration
}

// betterAddr reports whether a is a better addr to use than b.
func betterAddr(a, b addrLatency) bool {
	if a.IPPort == b.IPPort {
		return false
	}
	if b.IsZero() {
		return true
	}
	if a.IsZero() {
		return false
	}
	if a.IP().Is6() && b.IP().Is4() {
		// Prefer IPv6 for being a bit more robust, as long as
		// the latencies are roughly equivalent.
		if a.latency/10*9 < b.latency {
			return true
		}
	} else if a.IP().Is4() && b.IP().Is6() {
		if betterAddr(b, a) {
			return false
		}
	}
	return a.latency < b.latency
}

// endpoint.mu must be held.
func (st *endpointState) addPongReplyLocked(r pongReply) {
	if n := len(st.recentPongs); n < pongHistoryCount {
		st.recentPong = uint16(n)
		st.recentPongs = append(st.recentPongs, r)
		return
	}
	i := st.recentPong + 1
	if i == pongHistoryCount {
		i = 0
	}
	st.recentPongs[i] = r
	st.recentPong = i
}

// handleCallMeMaybe handles a CallMeMaybe discovery message via
// DERP. The contract for use of this message is that the peer has
// already sent to us via UDP, so their stateful firewall should be
// open. Now we can Ping back and make it through.
func (de *endpoint) handleCallMeMaybe(m *disco.CallMeMaybe) {
	if !de.canP2P() {
		// How did we receive a disco message from a peer that can't disco?
		panic("got call-me-maybe from peer with no discokey")
	}
	de.mu.Lock()
	defer de.mu.Unlock()

	now := time.Now()
	for ep := range de.isCallMeMaybeEP {
		de.isCallMeMaybeEP[ep] = false // mark for deletion
	}
	if de.isCallMeMaybeEP == nil {
		de.isCallMeMaybeEP = map[netaddr.IPPort]bool{}
	}
	var newEPs []netaddr.IPPort
	for _, ep := range m.MyNumber {
		if ep.IP().Is6() && ep.IP().IsLinkLocalUnicast() {
			// We send these out, but ignore them for now.
			// TODO: teach the ping code to ping on all interfaces
			// for these.
			continue
		}
		de.isCallMeMaybeEP[ep] = true
		if es, ok := de.endpointState[ep]; ok {
			es.callMeMaybeTime = now
		} else {
			de.endpointState[ep] = &endpointState{callMeMaybeTime: now}
			newEPs = append(newEPs, ep)
		}
	}
	if len(newEPs) > 0 {
		de.c.logf("[v1] magicsock: disco: call-me-maybe from %v %v added new endpoints: %v",
			de.publicKey.ShortString(), de.discoShort,
			logger.ArgWriter(func(w *bufio.Writer) {
				for i, ep := range newEPs {
					if i > 0 {
						w.WriteString(", ")
					}
					w.WriteString(ep.String())
				}
			}))
	}

	// Delete any prior CalllMeMaybe endpoints that weren't included
	// in this message.
	for ep, want := range de.isCallMeMaybeEP {
		if !want {
			delete(de.isCallMeMaybeEP, ep)
			de.deleteEndpointLocked(ep)
		}
	}

	// Zero out all the lastPing times to force sendPingsLocked to send new ones,
	// even if it's been less than 5 seconds ago.
	for _, st := range de.endpointState {
		st.lastPing = 0
	}
	de.sendPingsLocked(mono.Now(), false)
}

func (de *endpoint) populatePeerStatus(ps *ipnstate.PeerStatus) {
	de.mu.Lock()
	defer de.mu.Unlock()

	ps.Relay = de.c.derpRegionCodeOfIDLocked(int(de.derpAddr.Port()))

	if de.lastSend.IsZero() {
		return
	}

	now := mono.Now()
	ps.LastWrite = de.lastSend.WallTime()
	ps.Active = now.Sub(de.lastSend) < sessionActiveTimeout

	if udpAddr, derpAddr := de.addrForSendLocked(now); !udpAddr.IsZero() && derpAddr.IsZero() {
		ps.CurAddr = udpAddr.String()
	}
}

// stopAndReset stops timers associated with de and resets its state back to zero.
// It's called when a discovery endpoint is no longer present in the
// NetworkMap, or when magicsock is transitioning from running to
// stopped state (via SetPrivateKey(zero))
func (de *endpoint) stopAndReset() {
	atomic.AddInt64(&de.numStopAndResetAtomic, 1)
	de.mu.Lock()
	defer de.mu.Unlock()

	de.c.logf("[v1] magicsock: doing cleanup for discovery key %x", de.discoKey[:])

	// Zero these fields so if the user re-starts the network, the discovery
	// state isn't a mix of before & after two sessions.
	de.lastSend = 0
	de.lastFullPing = 0
	de.bestAddr = addrLatency{}
	de.bestAddrAt = 0
	de.trustBestAddrUntil = 0
	for _, es := range de.endpointState {
		es.lastPing = 0
	}

	for txid, sp := range de.sentPing {
		de.removeSentPingLocked(txid, sp)
	}
	if de.heartBeatTimer != nil {
		de.heartBeatTimer.Stop()
		de.heartBeatTimer = nil
	}
	de.pendingCLIPings = nil
}

func (de *endpoint) numStopAndReset() int64 {
	return atomic.LoadInt64(&de.numStopAndResetAtomic)
}

// derpStr replaces DERP IPs in s with "derp-".
func derpStr(s string) string { return strings.ReplaceAll(s, "127.3.3.40:", "derp-") }

// ippEndpointCache is a mutex-free single-element cache, mapping from
// a single netaddr.IPPort to a single endpoint.
type ippEndpointCache struct {
	ipp netaddr.IPPort
	gen int64
	de  *endpoint
}
