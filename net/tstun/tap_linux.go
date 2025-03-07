// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tstun

import (
	"fmt"
	"net"
	"os"
	"os/exec"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/tun"
	"inet.af/netaddr"
	"inet.af/netstack/tcpip"
	"inet.af/netstack/tcpip/buffer"
	"inet.af/netstack/tcpip/header"
	"inet.af/netstack/tcpip/network/ipv4"
	"inet.af/netstack/tcpip/transport/udp"
	"tailscale.com/net/packet"
	"tailscale.com/types/ipproto"
)

// TODO: this was randomly generated once. Maybe do it per process start? But
// then an upgraded tailscaled would be visible to devices behind it. So
// maybe instead make it a function of the tailscaled's wireguard public key?
// For now just hard code it.
var ourMAC = net.HardwareAddr{0x30, 0x2D, 0x66, 0xEC, 0x7A, 0x93}

func init() { createTAP = createTAPLinux }

func createTAPLinux(tapName, bridgeName string) (tun.Device, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return nil, err
	}

	dev, err := openDevice(fd, tapName, bridgeName)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}

	return dev, nil
}

func openDevice(fd int, tapName, bridgeName string) (tun.Device, error) {
	ifr, err := unix.NewIfreq(tapName)
	if err != nil {
		return nil, err
	}

	// Flags are stored as a uint16 in the ifreq union.
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return nil, err
	}

	if err := run("ip", "link", "set", "dev", tapName, "up"); err != nil {
		return nil, err
	}
	if bridgeName != "" {
		if err := run("brctl", "addif", bridgeName, tapName); err != nil {
			return nil, err
		}
	}

	// Also sets non-blocking I/O on fd when creating tun.Device.
	dev, _, err := tun.CreateUnmonitoredTUNFromFD(fd) // TODO: MTU
	if err != nil {
		return nil, err
	}

	return dev, nil
}

type etherType [2]byte

var (
	etherTypeARP  = etherType{0x08, 0x06}
	etherTypeIPv4 = etherType{0x08, 0x00}
	etherTypeIPv6 = etherType{0x86, 0xDD}
)

const ipv4HeaderLen = 20

const (
	consumePacket = true
	passOnPacket  = false
)

// handleTAPFrame handles receiving a raw TAP ethernet frame and reports whether
// it's been handled (that is, whether it should NOT be passed to wireguard).
func (t *Wrapper) handleTAPFrame(ethBuf []byte) bool {

	if len(ethBuf) < ethernetFrameSize {
		// Corrupt. Ignore.
		if tapDebug {
			t.logf("tap: short TAP frame")
		}
		return consumePacket
	}
	ethDstMAC, ethSrcMAC := ethBuf[:6], ethBuf[6:12]
	_ = ethDstMAC
	et := etherType{ethBuf[12], ethBuf[13]}
	switch et {
	default:
		if tapDebug {
			t.logf("tap: ignoring etherType %v", et)
		}
		return consumePacket // filter out packet we should ignore
	case etherTypeIPv6:
		// TODO: support DHCPv6/ND/etc later. For now pass all to WireGuard.
		if tapDebug {
			t.logf("tap: ignoring IPv6 %v", et)
		}
		return passOnPacket
	case etherTypeIPv4:
		if len(ethBuf) < ethernetFrameSize+ipv4HeaderLen {
			// Bogus IPv4. Eat.
			if tapDebug {
				t.logf("tap: short ipv4")
			}
			return consumePacket
		}
		return t.handleDHCPRequest(ethBuf)
	case etherTypeARP:
		arpPacket := header.ARP(ethBuf[ethernetFrameSize:])
		if !arpPacket.IsValid() {
			// Bogus ARP. Eat.
			return consumePacket
		}
		switch arpPacket.Op() {
		case header.ARPRequest:
			req := arpPacket // better name at this point
			buf := make([]byte, header.EthernetMinimumSize+header.ARPSize)

			// Our ARP "Table" of one:
			var srcMAC [6]byte
			copy(srcMAC[:], ethSrcMAC)
			if old := t.destMAC(); old != srcMAC {
				t.destMACAtomic.Store(srcMAC)
			}

			eth := header.Ethernet(buf)
			eth.Encode(&header.EthernetFields{
				SrcAddr: tcpip.LinkAddress(ourMAC[:]),
				DstAddr: tcpip.LinkAddress(ethSrcMAC),
				Type:    0x0806, // arp
			})
			res := header.ARP(buf[header.EthernetMinimumSize:])
			res.SetIPv4OverEthernet()
			res.SetOp(header.ARPReply)

			// If the client's asking about their own IP, tell them it's
			// their own MAC. TODO(bradfitz): remove String allocs.
			if net.IP(req.ProtocolAddressTarget()).String() == theClientIP {
				copy(res.HardwareAddressSender(), ethSrcMAC)
			} else {
				copy(res.HardwareAddressSender(), ourMAC[:])
			}

			copy(res.ProtocolAddressSender(), req.ProtocolAddressTarget())
			copy(res.HardwareAddressTarget(), req.HardwareAddressSender())
			copy(res.ProtocolAddressTarget(), req.ProtocolAddressSender())

			n, err := t.tdev.Write(buf, 0)
			if tapDebug {
				t.logf("tap: wrote ARP reply %v, %v", n, err)
			}
		}

		return consumePacket
	}
}

// TODO(bradfitz): remove these hard-coded values and move from a /24 to a /10 CGNAT as the range.
const theClientIP = "100.70.145.3" // TODO: make dynamic from netmap
const routerIP = "100.70.145.1"    // must be in same netmask (currently hack at /24) as theClientIP

// handleDHCPRequest handles receiving a raw TAP ethernet frame and reports whether
// it's been handled as a DHCP request. That is, it reports whether the frame should
// be ignored by the caller and not passed on.
func (t *Wrapper) handleDHCPRequest(ethBuf []byte) bool {
	const udpHeader = 8
	if len(ethBuf) < ethernetFrameSize+ipv4HeaderLen+udpHeader {
		if tapDebug {
			t.logf("tap: DHCP short")
		}
		return passOnPacket
	}
	ethDstMAC, ethSrcMAC := ethBuf[:6], ethBuf[6:12]

	if string(ethDstMAC) != "\xff\xff\xff\xff\xff\xff" {
		// Not a broadcast
		if tapDebug {
			t.logf("tap: dhcp no broadcast")
		}
		return passOnPacket
	}

	p := parsedPacketPool.Get().(*packet.Parsed)
	defer parsedPacketPool.Put(p)
	p.Decode(ethBuf[ethernetFrameSize:])

	if p.IPProto != ipproto.UDP || p.Src.Port() != 68 || p.Dst.Port() != 67 {
		// Not a DHCP request.
		if tapDebug {
			t.logf("tap: DHCP wrong meta")
		}
		return passOnPacket
	}

	dp, err := dhcpv4.FromBytes(ethBuf[ethernetFrameSize+ipv4HeaderLen+udpHeader:])
	if err != nil {
		// Bogus. Trash it.
		if tapDebug {
			t.logf("tap: DHCP FromBytes bad")
		}
		return consumePacket
	}
	if tapDebug {
		t.logf("tap: DHCP request: %+v", dp)
	}
	switch dp.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		offer, err := dhcpv4.New(
			dhcpv4.WithReply(dp),
			dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
			dhcpv4.WithRouter(net.ParseIP(routerIP)), // the default route
			dhcpv4.WithDNS(net.ParseIP("100.100.100.100")),
			dhcpv4.WithServerIP(net.ParseIP("100.100.100.100")), // TODO: what is this?
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("100.100.100.100"))),
			dhcpv4.WithYourIP(net.ParseIP(theClientIP)),
			dhcpv4.WithLeaseTime(3600), // hour works
			//dhcpv4.WithHwAddr(ethSrcMAC),
			dhcpv4.WithNetmask(net.IPMask(net.ParseIP("255.255.255.0").To4())), // TODO: wrong
			//dhcpv4.WithTransactionID(dp.TransactionID),
		)
		if err != nil {
			t.logf("error building DHCP offer: %v", err)
			return consumePacket
		}
		// Make a layer 2 packet to write out:
		pkt := packLayer2UDP(
			offer.ToBytes(),
			ourMAC, ethSrcMAC,
			netaddr.IPPortFrom(netaddr.IPv4(100, 100, 100, 100), 67), // src
			netaddr.IPPortFrom(netaddr.IPv4(255, 255, 255, 255), 68), // dst
		)
		n, err := t.tdev.Write(pkt, 0)
		if tapDebug {
			t.logf("tap: wrote DHCP OFFER %v, %v", n, err)
		}
	case dhcpv4.MessageTypeRequest:
		ack, err := dhcpv4.New(
			dhcpv4.WithReply(dp),
			dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
			dhcpv4.WithDNS(net.ParseIP("100.100.100.100")),
			dhcpv4.WithRouter(net.ParseIP(routerIP)),            // the default route
			dhcpv4.WithServerIP(net.ParseIP("100.100.100.100")), // TODO: what is this?
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("100.100.100.100"))),
			dhcpv4.WithYourIP(net.ParseIP(theClientIP)), // Hello world
			dhcpv4.WithLeaseTime(3600),                  // hour works
			dhcpv4.WithNetmask(net.IPMask(net.ParseIP("255.255.255.0").To4())),
		)
		if err != nil {
			t.logf("error building DHCP ack: %v", err)
			return consumePacket
		}
		// Make a layer 2 packet to write out:
		pkt := packLayer2UDP(
			ack.ToBytes(),
			ourMAC, ethSrcMAC,
			netaddr.IPPortFrom(netaddr.IPv4(100, 100, 100, 100), 67), // src
			netaddr.IPPortFrom(netaddr.IPv4(255, 255, 255, 255), 68), // dst
		)
		n, err := t.tdev.Write(pkt, 0)
		if tapDebug {
			t.logf("tap: wrote DHCP ACK %v, %v", n, err)
		}
	default:
		if tapDebug {
			t.logf("tap: unknown DHCP type")
		}
	}
	return consumePacket
}

func packLayer2UDP(payload []byte, srcMAC, dstMAC net.HardwareAddr, src, dst netaddr.IPPort) []byte {
	buf := buffer.NewView(header.EthernetMinimumSize + header.UDPMinimumSize + header.IPv4MinimumSize + len(payload))
	payloadStart := len(buf) - len(payload)
	copy(buf[payloadStart:], payload)
	srcB := src.IP().As4()
	srcIP := tcpip.Address(srcB[:])
	dstB := dst.IP().As4()
	dstIP := tcpip.Address(dstB[:])
	// Ethernet header
	eth := header.Ethernet(buf)
	eth.Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(srcMAC),
		DstAddr: tcpip.LinkAddress(dstMAC),
		Type:    ipv4.ProtocolNumber,
	})
	// IP header
	ipbuf := buf[header.EthernetMinimumSize:]
	ip := header.IPv4(ipbuf)
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(ipbuf)),
		TTL:         65,
		Protocol:    uint8(udp.ProtocolNumber),
		SrcAddr:     srcIP,
		DstAddr:     dstIP,
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	// UDP header
	u := header.UDP(buf[header.EthernetMinimumSize+header.IPv4MinimumSize:])
	u.Encode(&header.UDPFields{
		SrcPort: src.Port(),
		DstPort: dst.Port(),
		Length:  uint16(header.UDPMinimumSize + len(payload)),
	})
	// Calculate the UDP pseudo-header checksum.
	xsum := header.PseudoHeaderChecksum(udp.ProtocolNumber, srcIP, dstIP, uint16(len(u)))
	// Calculate the UDP checksum and set it.
	xsum = header.Checksum(payload, xsum)
	u.SetChecksum(^u.CalculateChecksum(xsum))
	return []byte(buf)
}

func run(prog string, args ...string) error {
	cmd := exec.Command(prog, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error running %v: %v", cmd, err)
	}
	return nil
}

func (t *Wrapper) destMAC() [6]byte {
	mac, _ := t.destMACAtomic.Load().([6]byte)
	return mac
}

func (t *Wrapper) tapWrite(buf []byte, offset int) (int, error) {
	if offset < ethernetFrameSize {
		return 0, fmt.Errorf("[unexpected] weird offset %d for TAP write", offset)
	}
	eth := buf[offset-ethernetFrameSize:]
	dst := t.destMAC()
	copy(eth[:6], dst[:])
	copy(eth[6:12], ourMAC[:])
	et := etherTypeIPv4
	if buf[offset]>>4 == 6 {
		et = etherTypeIPv6
	}
	eth[12], eth[13] = et[0], et[1]
	if tapDebug {
		t.logf("tap: tapWrite off=%v % x", offset, buf)
	}
	return t.tdev.Write(buf, offset-ethernetFrameSize)
}
