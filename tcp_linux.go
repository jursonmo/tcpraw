// The MIT License (MIT)
//
// Copyright (c) 2019 xtaci
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

//go:build linux

package tcpraw

import (
	"container/list"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

var BatchSize int = 4
var MaxPayloadSize int = 52048 // 2048 //如果对方不会发送大报文，建议设置为2048
var (
	errOpNotImplemented = errors.New("operation not implemented") // Error for unimplemented operations
	errTimeout          = errors.New("timeout")                   // Error for operation timeout
	expire              = time.Minute
	ErrFlowNotFound     = errors.New("flow not found") // Duration to define expiration time for flows
)

var (
	connList   list.List
	connListMu sync.Mutex
)

// a message from NIC
type message struct {
	bts  []byte
	addr net.Addr
}

// a tcp flow information of a connection pair
type tcpFlow struct {
	sync.Mutex
	conn         *net.TCPConn               // the related system TCP connection of this flow
	handle       *net.IPConn                // the handle to send packets
	seq          uint32                     // TCP sequence number
	ack          uint32                     // TCP acknowledge number
	networkLayer gopacket.SerializableLayer // network layer header for tx
	ts           time.Time                  // last packet incoming time
	buf          gopacket.SerializeBuffer   // a buffer for write
	tcpHeader    layers.TCP
}

// TCPConn
type TCPConn struct {
	// a wrapper for tcpconn for gc purpose
	*tcpConn
}

// tcpConn defines a TCP-packet oriented connection
type tcpConn struct {
	elem    *list.Element // elem in the list
	die     chan struct{}
	dieOnce sync.Once

	// the main golang sockets
	tcpconn      *net.TCPConn     // from net.Dial
	listener     *net.TCPListener // from net.Listen
	pc           *ipv4.PacketConn
	reuseportNum int

	// handles
	handles []*net.IPConn

	// packets captured from all related NICs will be delivered to this channel
	chMessage chan message

	// all TCP flows
	sharding   int
	flowTables []map[addrKey]*tcpFlow
	flowsLocks []sync.RWMutex

	// iptables
	iptables  *iptables.IPTables // Handle for IPv4 iptables rules
	iprule    []string           // IPv4 iptables rule associated with the connection
	ip6tables *iptables.IPTables // Handle for IPv6 iptables rules
	ip6rule   []string           // IPv6 iptables rule associated with the connection

	// deadlines
	readDeadline  atomic.Value // Atomic value for read deadline
	writeDeadline atomic.Value // Atomic value for write deadline

	// serialization
	opts gopacket.SerializeOptions

	// fingerprints
	tcpFingerPrint fingerPrint
}

func (conn *tcpConn) initFlowTable(sharding int) {
	conn.sharding = sharding
	conn.flowsLocks = make([]sync.RWMutex, sharding)
	conn.flowTables = make([]map[addrKey]*tcpFlow, sharding)
	for i := 0; i < sharding; i++ {
		conn.flowTables[i] = make(map[addrKey]*tcpFlow)
	}
}

func addrToHashNumber(addr net.Addr) uint64 {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		panic("addr is not a tcp address")
	}
	ip4 := tcpAddr.IP.To4()
	if ip4 == nil {
		//it is ipv6, just support ipv4 for now
		return 0
	}
	ip := binary.BigEndian.Uint32(ip4)
	ip ^= ip >> 20
	ip ^= ip >> 14
	ip ^= ip >> 9
	return uint64(ip) + uint64(tcpAddr.Port) //TODO:优化算法，让低三位更加分散。因为我们要& (conn.sharding - 1)
}

func addrSplit(addr net.Addr) (ip uint32, port uint16) {
	if addr == nil {
		return 0, 0
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		panic("addr is not a tcp address")
	}
	ip4 := tcpAddr.IP.To4()
	if ip4 == nil {
		//it is ipv6, just support ipv4 for now
		return 0, 0
	}
	ip = binary.BigEndian.Uint32(ip4)
	return ip, uint16(tcpAddr.Port)
}

// 4 tuple key for flow table
type addrKey struct {
	//sip   uint32
	dip uint32
	//sport uint16
	dport uint16
}

func (key *addrKey) HashNumber() int {
	ip := key.dip
	port := key.dport
	ip ^= ip >> 20
	ip ^= ip >> 14
	ip ^= ip >> 9
	return int(ip) + int(port)
}

// add by mo: 发送数据时，获取flow表项，如果flow表项不存在，则返回错误。可以认为底层连接断开了。
// 多个任务同时发送数据WriteTo时，可以避免锁竞争。
func (conn *tcpConn) getflow(key addrKey, f func(e *tcpFlow, shardIndex int)) {
	// addrNum := addrToHashNumber(addr)
	// shardIndex := int(addrNum) & (conn.sharding - 1)
	// key := addr.String()                // Use the string representation of the address as the key
	shardIndex := key.HashNumber() & (conn.sharding - 1)
	conn.flowsLocks[shardIndex].RLock() // Lock the flowTable for safe access
	e, ok := conn.flowTables[shardIndex][key]
	if !ok {
		fmt.Printf("getflow not found: %v, shardIndex: %d\n", key, shardIndex)
	}
	conn.flowsLocks[shardIndex].RUnlock() // Unlock the flowTable
	f(e, shardIndex)
}

// lockflow locks the flow table and apply function `f` to the entry, and create one if not exist
func (conn *tcpConn) lockflow(key addrKey, f func(e *tcpFlow, shardIndex int)) {
	// addrNum := addrToHashNumber(addr)
	// shardIndex := int(addrNum) & (conn.sharding - 1)
	// key := addr.String()  // Use the string representation of the address as the key //mo: 这种方式会产生很多小对象，不利于gc
	shardIndex := key.HashNumber() & (conn.sharding - 1)
	conn.flowsLocks[shardIndex].Lock() // Lock the flowTable for safe access
	e := conn.flowTables[shardIndex][key]
	if e == nil { // entry first visit
		e = new(tcpFlow)                      // Create a new flow if it doesn't exist
		e.ts = time.Now()                     // Set the timestamp to the current time
		e.buf = gopacket.NewSerializeBuffer() // Initialize the serialization buffer
		//add by mo: 打印下是client 或server 创建的flow
		if conn.tcpconn != nil {
			log.Println("conn:", conn.tcpconn.LocalAddr().String(), "new flow:", key, "shardIndex:", shardIndex)
		} else {
			log.Println("listener:", conn.listener.Addr().String(), "new flow:", key, "shardIndex:", shardIndex)
		}
		//end by mo
	}
	conn.flowTables[shardIndex][key] = e // Store the modified flow entry back into the table
	conn.flowsLocks[shardIndex].Unlock() // Unlock the flowTable
	f(e, shardIndex)                     // Apply the function to the flow entry
}

// clean expired flows
func (conn *tcpConn) cleaner() {
	ticker := time.NewTicker(time.Second * 33) // Create a ticker to trigger flow cleanup every minute
	defer ticker.Stop()
	for { //fix by mo: 需要for来重复执行, 否则执行一次会退出循环。
		select {
		case <-conn.die: // Exit if the connection is closed
			return
		case <-ticker.C: // On each tick, clean up expired flows
			log.Println("check expired flows, now: ", time.Now())
			for i := 0; i < conn.sharding; i++ {
				conn.flowsLocks[i].Lock()
				for k, v := range conn.flowTables[i] {
					if time.Now().Sub(v.ts) > expire {
						log.Printf("clean expired shard %d flow: %v, ts: %v\n", i, k, v.ts)
						if v.conn != nil {
							setTTL(v.conn, 64)
							v.conn.Close()
						}
						delete(conn.flowTables[i], k)
					}
				}
				conn.flowsLocks[i].Unlock()
			}
			// conn.flowsLock.Lock()
			// for k, v := range conn.flowTable {
			// 	if time.Now().Sub(v.ts) > expire { // Check if the flow has expired
			// 		if v.conn != nil {
			// 			setTTL(v.conn, 64) // Set TTL before closing the connection
			// 			v.conn.Close()
			// 		}
			// 		delete(conn.flowTable, k) // Remove the flow from the table
			// 	}
			// }
			// conn.flowsLock.Unlock()
		}
	}
}

func (conn *tcpConn) captureFlow(pc *ipv4.PacketConn, handleId int, handle *net.IPConn, port int) {
	var buf []byte
	var msgs []ipv4.Message
	if BatchSize < 2 {
		buf = make([]byte, MaxPayloadSize)
	} else {
		log.Println("use read batch, batch size:", BatchSize)
		msgs = make([]ipv4.Message, BatchSize)
		for i := 0; i < BatchSize; i++ {
			buf := make([]byte, MaxPayloadSize)
			msgs[i] = ipv4.Message{Buffers: [][]byte{buf}}
		}
	}

	//设置gopacket解码选项：NoCopy=true表示不复制payload，Lazy=true表示延迟解码
	opt := gopacket.DecodeOptions{NoCopy: true, Lazy: true}
	for {
		if pc == nil || BatchSize < 2 {
			// 从IP层读取数据到buf，返回读取到的字节数n，发送方地址addr，以及错误信息err
			n, addr, err := handle.ReadFromIP(buf) //去掉buf中ipv4头部再返回，涉及到copy，性能不如ReadBatch
			if err != nil {
				// 发生错误时退出循环，结束函数
				return
			}
			//fmt.Printf("handle read from ip:%s, len:%d\n", addr.String(), n)
			conn.decodeTCPPacket(buf[:n], addr, handle, port, opt)
		} else {
			n, err := pc.ReadBatch(msgs, 0) //recvmmsg
			if err != nil {
				// 发生错误时退出循环，结束函数
				panic(err)
			}
			//log.Printf("handleId:%d read batch n:%d\n", handleId, n)
			for i, msg := range msgs[:n] {
				_ = i
				buf := msg.Buffers[0]
				addr := msg.Addr.(*net.IPAddr)
				iphlen := getIPv4HeaderLen(buf)
				//fmt.Printf("batch:%d read from ip:%s, len:%d, ipv4 header len:%d\n", i, addr.String(), msg.N, iphlen)
				conn.decodeTCPPacket(buf[iphlen:msg.N], addr, handle, port, opt)
			}
		}
	}
}

func getIPv4HeaderLen(b []byte) int {
	if len(b) < 20 {
		return 0
	}
	if b[0]>>4 != 4 {
		return 0 //not ipv4 packet
	}
	l := int(b[0]&0x0f) << 2
	if 20 > l || l > len(b) {
		return 0
	}
	return l
}

func (conn *tcpConn) decodeTCPPacket(buf []byte, addr *net.IPAddr, handle *net.IPConn, targetPort int, opt gopacket.DecodeOptions) (err error) {
	n := len(buf)
	// 尝试把收到的buf[:n]数据解析为TCP包
	packet := gopacket.NewPacket(buf[:n], layers.LayerTypeTCP, opt)
	// 获取TransportLayer层（传输层）
	transport := packet.TransportLayer()
	// 尝试将transport（接口）断言为TCP层对象
	tcp, ok := transport.(*layers.TCP)
	if !ok {
		// 如果不是TCP包，跳过本次循环
		log.Printf("captureFlow: not TCP packet, handle local:%v:%d, remote: %v, len: %d\n", handle.LocalAddr().String(), targetPort, addr.String(), n)
		panic("not TCP packet, never happen")
	}

	// 端口过滤，只处理目标端口等于port的TCP包.
	// mo:对于client 而已，应该再过滤下数据报文的目的ip是否是本机ip，如果不是本机ip，则丢弃。
	//			如果在创建handle 时，指定了源地址，那么就不需要维护多flow了， client handle 在Dial 时，应该自动绑定了源地址？
	//      确实,对于server 而已，可能侦听了0.0.0.0:port， listen 时，创建多个handle，每个handle指定了一个本地ip作为listen源地址，handle 抓到的报文的目的ip肯定是handle 绑定的侦听ip，所以也只需要过滤port。
	//
	if int(tcp.DstPort) != targetPort { //TODO: 可以在创建handle 时，传入相关参数过滤掉非该端口的tcp报文吗，这样在内核层过滤可以提高性功能
		log.Printf("captureFlow: not target port:%d packet, local: %v, received tcp data, src port: %d, dst port: %d, remote: %v, len: %d\n",
			targetPort, handle.LocalAddr().String(), int(tcp.SrcPort), int(tcp.DstPort), addr.String(), n)
		//panic("not target port packet, never happen") //setBpf, so here never happen; 但是有一种情况是client同时向同一个服务器发起连接，同时创建多个handle，还没来得及设置bpf，数据已经到达本地并存放handle 对应的队列中, 就收到了非目标端口的报文, 这种情况应该如何处理？这里暂时不panic.
		return fmt.Errorf("packet dst port:%d is not target port:%d , never happen", tcp.DstPort, targetPort)
	}

	// 组装源地址，将收到包的源IP和源端口号构建为TCPAddr结构
	var src net.TCPAddr
	src.IP = addr.IP
	src.Port = int(tcp.SrcPort)

	key := addrKey{
		dip:   binary.BigEndian.Uint32(src.IP.To4()),
		dport: uint16(src.Port),
	}

	var shardIndex int
	var orphan bool // 标记该流是否“孤立”, 即有没有关联到真是的tcp连接
	// 流表维护。 通过对端ip和端口，找到对应的tcpFlow， 然后更新tcpFlow的状态。
	// TODO: bug, 以对端的信息作为key, 不需要本地ip和端口吗? 那么如果服务端侦听本地多地址， 对方用同一个地址来连接，冲突怎么处理? conn.flowtable 是包含了所有handle 生成的flow表项的， 是有可能冲突的。
	conn.lockflow(key, func(e *tcpFlow, sharding int) {
		e.Lock()
		defer e.Unlock()
		shardIndex = sharding
		// 如果e.conn为nil，说明这个流还未关联底层net.TCPConn，则标记为孤立
		// 如果是client, 在dial 时，会建立一个真实的tcp连接， 当时就绑定了真实tcp conn，所以e.conn不为nil
		// 如果是server, 在真实tcp listen accept时，会建立一个真实的tcp连接， 这时才绑定了真实tcp conn，业务都是真实tcp dial成功后才发送数据，这时抓得到的数据的flow 可能不是孤立的了。
		if e.conn == nil {
			orphan = true
		}

		// 记录当前流的最近活动时间为当前时间
		e.ts = time.Now()
		// 如果收到ACK包，则记录序号
		if tcp.ACK {
			e.seq = tcp.Ack
		}
		// 如果收到SYN包，则记录下一个期待的ack值
		if tcp.SYN {
			log.Printf("captureFlow, shardIndex:%d, SYN packet local: %v:%d, remote: %v, seq:%d ack:%d \n", shardIndex, handle.LocalAddr().String(), tcp.DstPort, src.String(), tcp.Seq, tcp.Ack)
			e.ack = tcp.Seq + 1 //mo:可以认为syn是报文一个字节长度的数据，所以ack = seq + 1。后面发送的数据，ack = seq + len(data)。
		}
		// 如果收到PSH包，且ack与当前序号相等，则更新ack为收到的数据长度之后
		if tcp.PSH {
			//mo: 如果ack与当前序号相等，才更新ack, 如果对方发送还没来得及收到本端的ack, 还是会继续发送原来seq但是是新内容的数据
			// 这对于真实是tcp socket 来说, 是重传? 但是对于tcpraw来说, 是正常行为,只需要正常处理接受的新数据就行。
			// 但是这有个问题，真实的tcp接受到数据后，是会自动回应ack, 收到超前的seq,也会认为有报文丢失，也会发送旧ack,
			// 这样跟tcpraw的ack不一致的话，没有问题吗? 一直不一致的话，真实socket 会不会断开连接?
			//还是说，由于数据都是只从tcpraw发送出去的，tcpraw只有e.ack == tcp.seq 才更新ack, 如果对方发送还没来得及收到本端的ack,
			// 还是会继续发送原来seq但是新内容的数据, 这样真实socket 只会认为重传,不会断开，而且它回应的ack 没有push标志位，不会导致tcpraw 更新ack,
			// 但是真实tcp socket 的回应的ack 还有可能超过tcpraw 发出ack吗？不可能, 比如对方tcpraw发送了seq=50的200个字节, 然后又发送seq=5的100个字节,
			// 本端真实tcp socket 已经更新ack=250,会认为seq=5的100个字节是重传, 但是tcpraw也更新本地ack=50+200=250, 再次收到seq=5的100个字节,不会更新ack
			// 但是依然把seq=5的100个字节数据发供上层应用读取，在本端没有把ack=250发送给对方前，对方依然用seq=50来说发送数据，可能发送seq=50 80字节数据， 没有影响。
			// 但是有一种情况可能会导致真实tcp socket 断开连接: tcpraw 更新了ack, 但是本地真实socket 没有更新ack, 即tcpraw ack 超过了真实socket ack, 后续的报文，真实socket 会认为数据丢失，这样会导致真实socket 断开连接。
			// 同时真正的tcp socket 被设置ttl, 以便iptables DROP 丢弃，也就是无法发送到对方的。但是关闭真实tcp socket时，设置ttl 不为1, 这样可以关闭真实socket？ 但是本端发送fin时， 对方收到后，回应的ack ttl 也是1吗， 那对方还是发不出去啊， TODO:测试下tcp 关闭是否异常?。
			if e.ack == tcp.Seq { //mo: 由于真实socket是不发送数据的，那么更新ack肯定是对方tcpraw 发送的.也就是导致ack更新的因素是单一的，这样保证ack的更新是正确的。
				e.ack = tcp.Seq + uint32(len(tcp.Payload))
			}
		}
		// 记录当前的网络句柄
		e.handle = handle
	})

	//mo: 如果此流不是孤立的，并收到PSH（说明有数据负载），则把数据推送出去, 三次握手的tcp数据不会包含在PSH, 不会推送到上层
	if !orphan && tcp.PSH {
		// 拷贝TCP负载内容到新的切片
		payload := make([]byte, len(tcp.Payload))
		if len(tcp.Payload) > 10000 { //分片重组后, 可能会超过10000字节
			log.Printf("tcp payload len > 10000, len:%d", len(tcp.Payload))
		}
		//test, 限制tcp payload 不能超过或等于MaxPayloadSize, 这样说明有截断, 这里先panic
		if len(tcp.Payload) >= MaxPayloadSize {
			panic("tcp payload len > MaxPayloadSize")
		}
		copy(payload, tcp.Payload)
		_ = shardIndex
		//log.Println("captureFlow, shardIndex:", shardIndex, "push bytes:", len(payload), "data:", string(payload))
		// 通过通道chMessage把数据发送出来，或监听到conn.die关闭返回
		select {
		case conn.chMessage <- message{payload, &src}: //mo: 不是孤立才将数据发送出来，供上层应用读取, 也就是上层读取到的数据一定不是孤立的()
		case <-conn.die:
			return
		}
	}
	return
}

// ReadFrom implements the PacketConn ReadFrom method.
func (conn *tcpConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := conn.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-deadline:
		return 0, nil, errTimeout
	case <-conn.die:
		return 0, nil, io.EOF
	case packet := <-conn.chMessage:
		n = copy(p, packet.bts)
		return n, packet.addr, nil
	}
}

// WriteTo implements the PacketConn WriteTo method.
func (conn *tcpConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	var deadline <-chan time.Time
	if d, ok := conn.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-deadline:
		return 0, errTimeout
	case <-conn.die:
		return 0, io.EOF
	default:
		//raddr, err := net.ResolveTCPAddr("tcp", addr.String())
		// var raddr *net.TCPAddr
		// raddr, err = net.ResolveTCPAddr("tcp", addr.String())
		// if err != nil {
		// 	return 0, err
		// }
		raddr, ok := addr.(*net.TCPAddr)
		if !ok {
			return 0, fmt.Errorf("addr is not a tcp address")
		}

		var lport int
		if conn.tcpconn != nil {
			lport = conn.tcpconn.LocalAddr().(*net.TCPAddr).Port
		} else {
			lport = conn.listener.Addr().(*net.TCPAddr).Port
		}

		ip, port := addrSplit(addr)
		key := addrKey{
			dip:   ip,
			dport: port,
		}
		//conn.lockflow(addr, func(e *tcpFlow) {
		conn.getflow(key, func(e *tcpFlow, shardIndex int) {
			if e == nil {
				err = ErrFlowNotFound
				return
			}
			e.Lock()
			defer e.Unlock() //下面的Write发送操作是阻塞式的,这可能会影响接受此流数据时由于锁竞争而阻塞。 TODO: 多个任务同时发送同一个流数据, 会有问题吗？有,同时操作e.buf

			// if the flow doesn't have handle , assume this packet has lost, without notification
			if e.handle == nil { //mo: 经过captureFlow 捕获过的flow 都有handle, 没有handle的flow说明是本端主动发送一个全新的包， 理论上不应该发送， 应该返回错误。
				//n = len(p) //mo: 这里不返回n = len(p)，而是返回 n=0， 这样上层应用可以根据返回的n=0，来判断是否重新发送数据。
				return
			}

			// build tcp header with local and remote port
			e.tcpHeader.SrcPort = layers.TCPPort(lport)
			e.tcpHeader.DstPort = layers.TCPPort(raddr.Port)
			binary.Read(rand.Reader, binary.LittleEndian, &e.tcpHeader.Window)
			e.tcpHeader.Window = conn.tcpFingerPrint.Window
			e.tcpHeader.Ack = e.ack
			e.tcpHeader.Seq = e.seq
			e.tcpHeader.PSH = true
			e.tcpHeader.ACK = true
			e.tcpHeader.Options = conn.tcpFingerPrint.Options
			makeOption(conn.tcpFingerPrint.Type, e.tcpHeader.Options)

			// build IP header with src & dst ip for TCP checksum
			if raddr.IP.To4() != nil {
				ip := &layers.IPv4{
					Protocol: layers.IPProtocolTCP,
					SrcIP:    e.handle.LocalAddr().(*net.IPAddr).IP.To4(),
					DstIP:    raddr.IP.To4(),
				}
				e.tcpHeader.SetNetworkLayerForChecksum(ip)
			} else {
				ip := &layers.IPv6{
					NextHeader: layers.IPProtocolTCP,
					SrcIP:      e.handle.LocalAddr().(*net.IPAddr).IP.To16(),
					DstIP:      raddr.IP.To16(),
				}
				e.tcpHeader.SetNetworkLayerForChecksum(ip)
			}

			e.buf.Clear()
			gopacket.SerializeLayers(e.buf, conn.opts, &e.tcpHeader, gopacket.Payload(p))
			//e.Unlock() //在发送前解锁。这样就不能保证多个任务同时Write的问题。
			if conn.tcpconn != nil {
				n, err = e.handle.Write(e.buf.Bytes()) //mo: 说明是client端，发送数据到对方, client dialIp 是指定了目的ip, 所以发送数据是直接发送给对方。
			} else {
				n, err = e.handle.WriteToIP(e.buf.Bytes(), &net.IPAddr{IP: raddr.IP})
			}
			if err != nil {
				return
			}
			//理论上，如果发送成功，n 应该等于 e.buf.Bytes() 的长度。不会出现只写了部分数据。
			if n != len(e.buf.Bytes()) {
				panic(fmt.Sprintf("tcpraw write len != payload len, write len:%d, payload len:%d", n, len(p)))
			}
			//e.Lock()
			// increase seq in flow
			e.seq += uint32(len(p))
			//mo: 就算当前发送的这个报文丢失了,也没关系, 收到对方的ack, 就会无条件更新成对方的ack, 这样这里seq 又回到之前的值， 即对方告诉你重新用这个seq 发送数据，
			// 这个跟tcp 的重传机制不一样, tcp 的重传机制是，如果收到对方的ack 跟本地的seq 不相等,会认为丢包，会重传之前的报文，而这里不会， 这里会用对方ack 重置本地的seq, 并用这个新seq 发送新的报文。
			// 比如，当前本端seq=1,ack=2, 对方ack=1, seq=2, 本端连续发生三个报文，第一个报文：seq=1,len(p)=10, 第二个报文：seq=11,len(p)=10, 第三个报文：seq=21,len(p)=10, 这时本地seq=31
			// 假设第一个报文丢失了， 第二个、第三个报文到达对方, 对方不会更新ack, 因为seq 不等于1, 但是对方会把第二、三个数据照常往上送，对方返回ack=1的数据时, 本地无条件更新seq=1, 回到之前的状态。
			// 也就说，如果本端发送的报文丢失, 不管后面继续发送了多少数据，那么本地seq总会在某个时间点被对方的ack 改回丢包前的seq, 然后又以这个seq 开始发送数据，没有丢包话，seq 正常累加。
			// 有些不丢包的情况下，seq 也会被回退。比如连续发送了三个报文，但是第三个报文还没到达对方时，对方就回应了数据，那么对方收到了两个报文，对方就以的ack=21 来回应数据，那么本端的seq从31又变回21。
			// 对方接受第三个报文后，对方才更新ack=31. 这个时候，本地已seq=21来发送新的数据，对方就不会更新ack了。
			n = len(p)
			//e.Unlock()
		})
	}
	return
}

// Close closes the connection.
func (conn *tcpConn) Close() error {
	var err error

	conn.dieOnce.Do(func() {
		// signal closing
		close(conn.die)

		// close all established tcp connections
		if conn.tcpconn != nil { // client
			setTTL(conn.tcpconn, 64)
			err = conn.tcpconn.Close()
		} else if conn.listener != nil {
			err = conn.listener.Close() // server
			for i := 0; i < conn.sharding; i++ {
				conn.flowsLocks[i].Lock()
				for k, v := range conn.flowTables[i] {
					if v.conn != nil {
						setTTL(v.conn, 64)
						v.conn.Close()
					}
					delete(conn.flowTables[i], k)
				}
				conn.flowsLocks[i].Unlock()
			}
			// conn.flowsLock.Lock()
			// for k, v := range conn.flowTable {
			// 	if v.conn != nil {
			// 		setTTL(v.conn, 64)
			// 		v.conn.Close()
			// 	}
			// 	delete(conn.flowTable, k)
			// }
			// conn.flowsLock.Unlock()
		}

		// close handles
		for k := range conn.handles {
			conn.handles[k].Close()
		}

		// delete iptable
		if conn.iptables != nil {
			conn.iptables.Delete("filter", "OUTPUT", conn.iprule...)
		}
		if conn.ip6tables != nil {
			conn.ip6tables.Delete("filter", "OUTPUT", conn.ip6rule...)
		}

		// remove from the global list
		connListMu.Lock()
		connList.Remove(conn.elem)
		connListMu.Unlock()
	})
	return err
}

// LocalAddr returns the local network address.
func (conn *tcpConn) LocalAddr() net.Addr {
	if conn.tcpconn != nil {
		return conn.tcpconn.LocalAddr()
	} else if conn.listener != nil {
		return conn.listener.Addr()
	}
	return nil
}

// add by mo: 只有client 才会返回remote addr
func (conn *tcpConn) RemoteAddr() net.Addr {
	if conn.tcpconn != nil {
		return conn.tcpconn.RemoteAddr() //即使tcpconn 被关闭，remote addr 还是有效的。
	}
	return nil
}

// SetDeadline implements the Conn SetDeadline method.
func (conn *tcpConn) SetDeadline(t time.Time) error {
	if err := conn.SetReadDeadline(t); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// SetReadDeadline implements the Conn SetReadDeadline method.
func (conn *tcpConn) SetReadDeadline(t time.Time) error {
	conn.readDeadline.Store(t)
	return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (conn *tcpConn) SetWriteDeadline(t time.Time) error {
	conn.writeDeadline.Store(t)
	return nil
}

// SetDSCP sets the 6bit DSCP field in IPv4 header, or 8bit Traffic Class in IPv6 header.
func (conn *tcpConn) SetDSCP(dscp int) error {
	for k := range conn.handles {
		if err := setDSCP(conn.handles[k], dscp); err != nil {
			return err
		}
	}
	return nil
}

// SetReadBuffer sets the size of the operating system's receive buffer associated with the connection.
func (conn *tcpConn) SetReadBuffer(bytes int) error {
	var err error
	for k := range conn.handles {
		if err := conn.handles[k].SetReadBuffer(bytes); err != nil {
			return err
		}
	}
	return err
}

// SetWriteBuffer sets the size of the operating system's transmit buffer associated with the connection.
func (conn *tcpConn) SetWriteBuffer(bytes int) error {
	var err error
	for k := range conn.handles {
		if err := conn.handles[k].SetWriteBuffer(bytes); err != nil {
			return err
		}
	}
	return err
}

// Dial 负责建立到远端 TCP 端口的“包级别”连接，并返回一个 TCPConn 对象。
// 函数中必须建立一个真实的 TCP 连接，它不仅仅是“占位”，还起到了核心作用：
// 1. 用真实的 net.DialTCP 建立连接后，本地系统协议栈会分配端口，并建立完整的连接状态。
//    这保证了后续构造“伪造 TCP 包”时能够获得合法的本地 IP/端口信息（如五元组），
//    并能通过 conn.tcpconn 对象查到 local addr 用于转发和标识本机的 TCP socket。
// 2. 建立真实连接还能方便利用内核路由决策、接收回包（例如被动接收 SYN/ACK 等），
//    同时通过修改 TTL 和支持 iptables DROP，可以实现仅流量探测但实际不收包的用例。
// 3. 还为了维持内核的 socket 状态，防止端口在 NAT 或路由设备上被清理/超时失效，
//    所以 io.Copy(ioutil.Discard, tcpconn) 保持连接“活跃”，即便实际数据被丢弃。
// 真实建立连接还能让应用对等端看到一个真的连接存在，有时对探测、旁路等需求至关重要。
// "占位"只是它的部分作用，实际上是确保模拟 TCP 包传输的上下文环境和连接之所有必要状态。

func Dial(network, address string) (*TCPConn, error) {
	// 解析远端地址
	raddr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}

	// 使用原始 IP 层建立包捕获/发送句柄, 抓到的数据是tcp 协议的包是tcp层的数据, 包括tcp头和tcp负载，不带ip层数据(其实golang net.IPConn readfrom() 自动去掉ipv4头部而已，底层socket 读到的数据是带ipv4头部的))，
	// 发送数据也是只发送tcp层的数据, 不包括ip层头部, 内核会自动添加ip层头部。
	handle, err := net.DialIP("ip:tcp", nil, &net.IPAddr{IP: raddr.IP}) //mo:抓取的是所有tcp包，多个client都这么做，是不是性能会下降? 指定了目的ip, 应该只抓取目的ip的tcp包.
	//mo:handle 会自动绑定一个本地ip地址(不包含端口)，可以通过 handle.LocalAddr() 获取。在writeTo 时，需要使用这个本地地址来生成tcp校验头部, 源端口只能通过真实tcp socket 的本地端口。
	if err != nil {
		return nil, err
	}

	// 关键：建立一个真实的 TCP socket 完全建立连接
	// 这能保证 NAT、协议栈等分配资源，且本地端口、路由、五元组都正确
	tcpconn, err := net.DialTCP(network, nil, raddr)
	if err != nil {
		//add by mo: 真实tcp dial 失败，需要关闭handle
		// 能不能把handle的创建放在真是tcp成功之后呢, 这样tcp dial 失败就不需要关闭handle? 不行
		// 这样handle就抓不到真实tcp socket的三次握手了, flow的handle 就为空,这时业务层发送数据时,WriteTo函数里flow就找不handle,发送不出去, 等接受到该flow数据并绑定handle后才能发送数据。
		handle.Close() //fix by mo:真实tcp dial 失败，需要关闭handle
		return nil, err
	}

	// 解析本地分配的 ip 和端口
	laddr, lport, err := net.SplitHostPort(tcpconn.LocalAddr().String())
	if err != nil {
		return nil, err
	}

	//add by mo: check handle 和 tcpconn 绑定的本地地址是否相同
	if handle.LocalAddr().String() != laddr {
		panic(fmt.Sprintf("handle and tcpconn bound local address are not the same: handle: %v, tcpconn: %v", handle.LocalAddr(), laddr))
	}

	// 初始化 tcpConn 对象及核心字段
	conn := new(tcpConn)
	conn.die = make(chan struct{})
	conn.initFlowTable(1)
	conn.tcpconn = tcpconn
	conn.chMessage = make(chan message)
	ip, port := addrSplit(tcpconn.RemoteAddr())
	key := addrKey{
		dip:   ip,
		dport: port,
	}
	conn.lockflow(key, func(e *tcpFlow, _ int) { e.conn = tcpconn }) //mo: 创建flow表项，并关联底层net.TCPConn
	conn.handles = append(conn.handles, handle)
	conn.opts = gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	conn.tcpFingerPrint = fingerPrintLinux

	// add by mo: 在handle 创建时，设置BPF过滤器，只抓取指定端口的tcp报文，提高抓包效率。但是在创建handle 和 setBpf 之间, 可能会收到非目标端口的报文。
	conn.pc = ipv4.NewPacketConn(handle)
	err = SetBPFFilterPortByPacketConn(conn.pc, uint32(tcpconn.LocalAddr().(*net.TCPAddr).Port))
	// err = SetBPFFilterPort(handle, uint32(tcpconn.LocalAddr().(*net.TCPAddr).Port))
	if err != nil {
		return nil, err
	}
	go conn.captureFlow(conn.pc, 0, handle, tcpconn.LocalAddr().(*net.TCPAddr).Port)
	go conn.cleaner()

	// 初始化 iptables 规则，保证 TTL=1 的包来自该 socket 会被立即丢弃。
	err = setTTL(tcpconn, 1)
	if err != nil {
		return nil, err
	}

	// mo: 那么真实tcp 如何保持连接呢? 真实tcp 连接被设置ttl=1, 会被立即丢弃, 构造的数据能让真实tcp 保持连接吗? 还是说真实的tcp是否是连接状态也无所谓。经过验证，真实tcp断开了也没关系, 依然能正常用tcpraw 收发数据。
	// 设置 IPv4 iptables 规则
	// if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4); err == nil {
	// 	rule := []string{"-m", "ttl", "--ttl-eq", "1", "-p", "tcp", "-s", laddr, "--sport", lport, "-d", raddr.IP.String(), "--dport", fmt.Sprint(raddr.Port), "-j", "DROP"}
	// 	if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
	// 		if !exists {
	// 			if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
	// 				conn.iprule = rule
	// 				conn.iptables = ipt
	// 			}
	// 		}
	// 	}
	// }
	// 设置 IPv4 iptables 规则, 丢弃所有本地发出的TTL=1 的TCP 包。这样不管客户端dial多少次，都只有一条规则。
	if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4); err == nil {
		rule := []string{"-m", "ttl", "--ttl-eq", "1", "-p", "tcp", "-j", "DROP"}
		if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
			if !exists {
				if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
					conn.iprule = rule
					conn.iptables = ipt
				}
			}
		}
	}
	// 设置 IPv6 iptables 规则
	if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv6); err == nil {
		rule := []string{"-m", "hl", "--hl-eq", "1", "-p", "tcp", "-s", laddr, "--sport", lport, "-d", raddr.IP.String(), "--dport", fmt.Sprint(raddr.Port), "-j", "DROP"}
		if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
			if !exists {
				if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
					conn.ip6rule = rule
					conn.ip6tables = ipt
				}
			}
		}
	}

	go io.Copy(ioutil.Discard, tcpconn)

	// 维护全局链表（便于统一管理和 GC）
	connListMu.Lock()
	conn.elem = connList.PushBack(conn)
	connListMu.Unlock()

	return wrapConn(conn), nil
}

// Listen acts like net.ListenTCP,
// and returns a single packet-oriented connection
func Listen(network, address string) (*TCPConn, error) {
	// fields
	conn := new(tcpConn)
	conn.initFlowTable(8)
	conn.die = make(chan struct{})
	conn.chMessage = make(chan message)
	conn.opts = gopacket.SerializeOptions{
		FixLengths:       true,
		ComputeChecksums: true,
	}
	conn.tcpFingerPrint = fingerPrintLinux

	conn.reuseportNum = 1 //原始套接字，使用 SO_REUSEPORT 并不能实现真正的负载均衡，多个原始套接字会收到相同的数据包副本。
	log.Println("reuseport enabled, reuseport num:", conn.reuseportNum)

	// resolve address
	laddr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}

	// AF_INET
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	if laddr.IP == nil || laddr.IP.IsUnspecified() { // if address is not specified, capture on all ifaces
		var lasterr error
		for _, iface := range ifaces {
			if addrs, err := iface.Addrs(); err == nil {
				for _, addr := range addrs {
					if ipaddr, ok := addr.(*net.IPNet); ok {
						if handle, err := net.ListenIP("ip:tcp", &net.IPAddr{IP: ipaddr.IP}); err == nil {
							conn.handles = append(conn.handles, handle)
							go conn.captureFlow(nil, 0, handle, laddr.Port)
						} else {
							lasterr = err
						}
					}
				}
			}
		}
		if len(conn.handles) == 0 {
			return nil, lasterr
		}
	} else {
		//mo: 实验证明,原始套接字，使用 SO_REUSEPORT 并不能实现真正的负载均衡, 这里的reuseportNum==1, 只创建一个原始套接字。
		for i := 0; i < conn.reuseportNum; i++ {
			if handle, err := net.ListenIP("ip:tcp", &net.IPAddr{IP: laddr.IP}); err == nil {
				raw, err := handle.SyscallConn()
				if err != nil {
					return nil, err
				}
				raw.Control(func(fd uintptr) {
					err = syscall.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1) //reuseport flag
				})
				if err != nil {
					panic(err)
					return nil, err
				}
				pc := ipv4.NewPacketConn(handle)
				err = SetBPFFilterPortByPacketConn(pc, uint32(laddr.Port))
				//err = SetBPFFilterPort(handle, uint32(laddr.Port))
				if err != nil {
					return nil, err
				}
				conn.handles = append(conn.handles, handle)
				go conn.captureFlow(pc, i, handle, laddr.Port)
			} else {
				return nil, err
			}
		}
	}

	// start listening
	l, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	conn.listener = l

	// start cleaner
	go conn.cleaner()

	// iptables drop packets marked with TTL = 1
	// TODO: what if iptables is not available, the next hop will send back ICMP Time Exceeded,
	// is this still an acceptable behavior?
	if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv4); err == nil {
		rule := []string{"-m", "ttl", "--ttl-eq", "1", "-p", "tcp", "--sport", fmt.Sprint(laddr.Port), "-j", "DROP"}
		if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
			if !exists {
				if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
					conn.iprule = rule
					conn.iptables = ipt
				}
			}
		}
	}
	if ipt, err := iptables.NewWithProtocol(iptables.ProtocolIPv6); err == nil {
		rule := []string{"-m", "hl", "--hl-eq", "1", "-p", "tcp", "--sport", fmt.Sprint(laddr.Port), "-j", "DROP"}
		if exists, err := ipt.Exists("filter", "OUTPUT", rule...); err == nil {
			if !exists {
				if err = ipt.Append("filter", "OUTPUT", rule...); err == nil {
					conn.ip6rule = rule
					conn.ip6tables = ipt
				}
			}
		}
	}

	// discard everything in original connection
	go func() {
		for {
			tcpconn, err := l.AcceptTCP()
			if err != nil {
				return
			}

			// if we cannot set TTL = 1, the only thing reasonable is panic
			if err := setTTL(tcpconn, 1); err != nil {
				panic(err)
			}

			// record net.Conn
			ip, port := addrSplit(tcpconn.RemoteAddr())
			key := addrKey{
				dip:   ip,
				dport: port,
			}
			conn.lockflow(key, func(e *tcpFlow, _ int) { e.conn = tcpconn })

			// discard everything
			go io.Copy(ioutil.Discard, tcpconn)
		}
	}()

	// push back to the global list and set the elem
	connListMu.Lock()
	conn.elem = connList.PushBack(conn)
	connListMu.Unlock()

	return wrapConn(conn), nil
}

// setTTL 在本项目中用于设置底层 socket 的 TTL（Time-To-Live）字段。
// 这样做的目的是：
//  1. 在客户端建立真实 TCP 连接时，将 TTL 设置为 1，配合 iptables 只丢弃（DROP）本地发出的 TTL=1 的 TCP 包，
//     能确保发往远端的伪造包通过原始接口“真正发送”，但由本地协议栈发出的正常 TCP 包被立即丢弃，避免干扰或被远端接收。
//  2. 实现旁路探测/透明代理等能力时，可以使得正常流量不离开本机，仅通过自定义的原始包发送实现
//     TCP 协议行为的仿真，从而增强流量可控性和隔离性。
func setTTL(c *net.TCPConn, ttl int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	addr := c.LocalAddr().(*net.TCPAddr)

	if addr.IP.To4() == nil {
		raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, ttl)
		})
	} else {
		raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
		})
	}
	return err
}

// setDSCP sets the 6bit DSCP field in IPv4 header, or 8bit Traffic Class in IPv6 header.
func setDSCP(c *net.IPConn, dscp int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	addr := c.LocalAddr().(*net.IPAddr)

	if addr.IP.To4() == nil {
		raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_TCLASS, dscp)
		})
	} else {
		raw.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, dscp<<2)
		})
	}
	return err
}

// wrapConn wraps a tcpConn in a TCPConn.
func wrapConn(conn *tcpConn) *TCPConn {
	// Set up a finalizer to ensure resources are cleaned up when the TCPConn is garbage collected
	wrapper := &TCPConn{tcpConn: conn}
	runtime.SetFinalizer(wrapper, func(wrapper *TCPConn) {
		wrapper.Close()
	})

	return wrapper
}
