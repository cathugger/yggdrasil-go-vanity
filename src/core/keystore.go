package core

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"

	iwt "github.com/Arceliar/ironwood/types"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
)

const keyStoreTimeout = 2 * time.Minute

type keyArray [ed25519.PublicKeySize]byte

type keyStore struct {
	core         *Core
	address      address.Address
	subnet       address.Subnet
	mutex        sync.Mutex
	keyToInfo    map[keyArray]*keyInfo
	addrToInfo   map[address.Address]*keyInfo
	addrBuffer   map[address.Address]*buffer
	subnetToInfo map[address.Subnet]*keyInfo
	subnetBuffer map[address.Subnet]*buffer
	mtu          uint64
}

type keyInfo struct {
	key     keyArray
	address address.Address
	subnet  address.Subnet
	timeout *time.Timer // From calling a time.AfterFunc to do cleanup
}

type buffer struct {
	packet  []byte
	timeout *time.Timer
}

func (k *keyStore) init(core *Core) {
	k.core = core
	k.address = *address.AddrForKey(k.core.secKey.PK[:])
	k.subnet = *address.SubnetForKey(k.core.secKey.PK[:])
	if err := k.core.pc.SetOutOfBandHandler(k.oobHandler); err != nil {
		err = fmt.Errorf("tun.core.SetOutOfBandHander: %w", err)
		panic(err)
	}
	k.keyToInfo = make(map[keyArray]*keyInfo)
	k.addrToInfo = make(map[address.Address]*keyInfo)
	k.addrBuffer = make(map[address.Address]*buffer)
	k.subnetToInfo = make(map[address.Subnet]*keyInfo)
	k.subnetBuffer = make(map[address.Subnet]*buffer)
	k.mtu = 1280 // Default to something safe, expect user to set this
}

func (k *keyStore) sendToAddress(addr address.Address, bs []byte) {
	k.mutex.Lock()
	if info := k.addrToInfo[addr]; info != nil {
		k.resetTimeout(info)
		k.mutex.Unlock()
		_, _ = k.core.pc.WriteTo(bs, iwt.Addr(info.key[:]))
	} else {
		var buf *buffer
		if buf = k.addrBuffer[addr]; buf == nil {
			buf = new(buffer)
			k.addrBuffer[addr] = buf
		}
		msg := append([]byte(nil), bs...)
		buf.packet = msg
		if buf.timeout != nil {
			buf.timeout.Stop()
		}
		buf.timeout = time.AfterFunc(keyStoreTimeout, func() {
			k.mutex.Lock()
			defer k.mutex.Unlock()
			if nbuf := k.addrBuffer[addr]; nbuf == buf {
				delete(k.addrBuffer, addr)
			}
		})
		k.mutex.Unlock()
		k.sendKeyLookup(addr.GetKey())
	}
}

func (k *keyStore) sendToSubnet(subnet address.Subnet, bs []byte) {
	k.mutex.Lock()
	if info := k.subnetToInfo[subnet]; info != nil {
		k.resetTimeout(info)
		k.mutex.Unlock()
		_, _ = k.core.pc.WriteTo(bs, iwt.Addr(info.key[:]))
	} else {
		var buf *buffer
		if buf = k.subnetBuffer[subnet]; buf == nil {
			buf = new(buffer)
			k.subnetBuffer[subnet] = buf
		}
		msg := append([]byte(nil), bs...)
		buf.packet = msg
		if buf.timeout != nil {
			buf.timeout.Stop()
		}
		buf.timeout = time.AfterFunc(keyStoreTimeout, func() {
			k.mutex.Lock()
			defer k.mutex.Unlock()
			if nbuf := k.subnetBuffer[subnet]; nbuf == buf {
				delete(k.subnetBuffer, subnet)
			}
		})
		k.mutex.Unlock()
		k.sendKeyLookup(subnet.GetKey())
	}
}

func (k *keyStore) update(key ed25519.PublicKey) *keyInfo {
	k.mutex.Lock()
	var kArray keyArray
	copy(kArray[:], key)
	var info *keyInfo
	if info = k.keyToInfo[kArray]; info == nil {
		info = new(keyInfo)
		info.key = kArray
		info.address = *address.AddrForKey(ed25519.PublicKey(info.key[:]))
		info.subnet = *address.SubnetForKey(ed25519.PublicKey(info.key[:]))
		k.keyToInfo[info.key] = info
		k.addrToInfo[info.address] = info
		k.subnetToInfo[info.subnet] = info
		k.resetTimeout(info)
		k.mutex.Unlock()
		if buf := k.addrBuffer[info.address]; buf != nil {
			k.core.pc.WriteTo(buf.packet, iwt.Addr(info.key[:]))
			delete(k.addrBuffer, info.address)
		}
		if buf := k.subnetBuffer[info.subnet]; buf != nil {
			k.core.pc.WriteTo(buf.packet, iwt.Addr(info.key[:]))
			delete(k.subnetBuffer, info.subnet)
		}
	} else {
		k.resetTimeout(info)
		k.mutex.Unlock()
	}
	return info
}

func (k *keyStore) resetTimeout(info *keyInfo) {
	if info.timeout != nil {
		info.timeout.Stop()
	}
	info.timeout = time.AfterFunc(keyStoreTimeout, func() {
		k.mutex.Lock()
		defer k.mutex.Unlock()
		if nfo := k.keyToInfo[info.key]; nfo == info {
			delete(k.keyToInfo, info.key)
		}
		if nfo := k.addrToInfo[info.address]; nfo == info {
			delete(k.addrToInfo, info.address)
		}
		if nfo := k.subnetToInfo[info.subnet]; nfo == info {
			delete(k.subnetToInfo, info.subnet)
		}
	})
}

func (k *keyStore) oobHandler(fromKey, toKey ed25519.PublicKey, data []byte) {
	if len(data) != 1+ed25519.SignatureSize {
		return
	}
	sig := data[1:]
	switch data[0] {
	case typeKeyLookup:
		snet := *address.SubnetForKey(toKey)
		if snet == k.subnet && ed25519.Verify(fromKey, toKey[:], sig) {
			// This is looking for at least our subnet (possibly our address)
			// Send a response
			k.sendKeyResponse(fromKey)
		}
	case typeKeyResponse:
		// TODO keep a list of something to match against...
		// Ignore the response if it doesn't match anything of interest...
		if ed25519.Verify(fromKey, toKey[:], sig) {
			k.update(fromKey)
		}
	}
}

func (k *keyStore) sendKeyLookup(partial ed25519.PublicKey) {
	var sig [64]byte
	k.core.secKey.SignED25519(&sig, partial)
	bs := append([]byte{typeKeyLookup}, sig[:]...)
	_ = k.core.pc.SendOutOfBand(partial, bs)
}

func (k *keyStore) sendKeyResponse(dest ed25519.PublicKey) {
	var sig [64]byte
	k.core.secKey.SignED25519(&sig, dest)
	bs := append([]byte{typeKeyResponse}, sig[:]...)
	_ = k.core.pc.SendOutOfBand(dest, bs)
}

func (k *keyStore) maxSessionMTU() uint64 {
	const sessionTypeOverhead = 1
	return k.core.pc.MTU() - sessionTypeOverhead
}

func (k *keyStore) readPC(p []byte) (int, error) {
	buf := make([]byte, k.core.pc.MTU(), 65535)
	for {
		bs := buf
		n, from, err := k.core.pc.ReadFrom(bs)
		if err != nil {
			return n, err
		}
		if n == 0 {
			continue
		}
		switch bs[0] {
		case typeSessionTraffic:
			// This is what we want to handle here
		case typeSessionProto:
			var key keyArray
			copy(key[:], from.(iwt.Addr))
			data := append([]byte(nil), bs[1:n]...)
			k.core.proto.handleProto(nil, key, data)
			continue
		default:
			continue
		}
		bs = bs[1:n]
		if len(bs) == 0 {
			continue
		}
		if bs[0]&0xf0 != 0x60 {
			continue // not IPv6
		}
		if len(bs) < 40 {
			continue
		}
		k.mutex.Lock()
		mtu := int(k.mtu)
		k.mutex.Unlock()
		if len(bs) > mtu {
			// Using bs would make it leak off the stack, so copy to buf
			buf := make([]byte, 40)
			copy(buf, bs)
			ptb := &icmp.PacketTooBig{
				MTU:  mtu,
				Data: buf[:40],
			}
			if packet, err := CreateICMPv6(buf[8:24], buf[24:40], ipv6.ICMPTypePacketTooBig, 0, ptb); err == nil {
				_, _ = k.writePC(packet)
			}
			continue
		}
		var srcAddr, dstAddr address.Address
		var srcSubnet, dstSubnet address.Subnet
		copy(srcAddr[:], bs[8:])
		copy(dstAddr[:], bs[24:])
		copy(srcSubnet[:], bs[8:])
		copy(dstSubnet[:], bs[24:])
		if dstAddr != k.address && dstSubnet != k.subnet {
			continue // bad local address/subnet
		}
		info := k.update(ed25519.PublicKey(from.(iwt.Addr)))
		if srcAddr != info.address && srcSubnet != info.subnet {
			continue // bad remote address/subnet
		}
		n = copy(p, bs)
		return n, nil
	}
}

func (k *keyStore) writePC(bs []byte) (int, error) {
	if bs[0]&0xf0 != 0x60 {
		return 0, errors.New("not an IPv6 packet") // not IPv6
	}
	if len(bs) < 40 {
		strErr := fmt.Sprint("undersized IPv6 packet, length: ", len(bs))
		return 0, errors.New(strErr)
	}
	var srcAddr, dstAddr address.Address
	var srcSubnet, dstSubnet address.Subnet
	copy(srcAddr[:], bs[8:])
	copy(dstAddr[:], bs[24:])
	copy(srcSubnet[:], bs[8:])
	copy(dstSubnet[:], bs[24:])
	if srcAddr != k.address && srcSubnet != k.subnet {
		// This happens all the time due to link-local traffic
		// Don't send back an error, just drop it
		strErr := fmt.Sprint("incorrect source address: ", net.IP(srcAddr[:]).String())
		return 0, errors.New(strErr)
	}
	buf := make([]byte, 1+len(bs), 65535)
	buf[0] = typeSessionTraffic
	copy(buf[1:], bs)
	if dstAddr.IsValid() {
		k.sendToAddress(dstAddr, buf)
	} else if dstSubnet.IsValid() {
		k.sendToSubnet(dstSubnet, buf)
	} else {
		return 0, errors.New("invalid destination address")
	}
	return len(bs), nil
}
