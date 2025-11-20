package common

import (
	"encoding/binary"
	"net"
)

type AddrKey struct {
	ip   uint32
	port uint16
}

func AddrToKey(addr net.Addr) AddrKey {
	if addr == nil {
		//return AddrKey{0, 0}
		panic("addr == nil")
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		panic("addr is not a tcp address")
	}
	ip4 := tcpAddr.IP.To4()
	if ip4 == nil {
		//it is ipv6, just support ipv4 for now
		//return AddrKey{0, 0}
		panic("just support ipv4 for now")
	}
	return AddrKey{binary.BigEndian.Uint32(ip4), uint16(tcpAddr.Port)}
}
