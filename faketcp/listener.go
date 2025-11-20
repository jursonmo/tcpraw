package faketcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jursonmo/tcpraw"
	"github.com/jursonmo/tcpraw/common"
)

type FakeConn struct {
	sync.Mutex
	readLock sync.Mutex

	ctx      context.Context
	cancel   context.CancelFunc
	listener *Listener
	laddr    *net.TCPAddr
	addr     net.Addr
	fakeConn *tcpraw.TCPConn
	recvch   chan []byte
	recvLeft []byte
	sendch   chan []byte

	ts          time.Time
	txBytes     uint64
	rxBytes     uint64
	dropRxBytes uint64

	fec *Fec

	closed bool
}

func (f *FakeConn) String() string {
	return fmt.Sprintf("fakeconn: %s, laddr: %s, addr: %s, txBytes: %d, rxBytes: %d, dropRxBytes: %d, fec:%d-%d", f.fakeConn.LocalAddr().String(), f.laddr.String(), f.addr.String(), f.txBytes, f.rxBytes, f.dropRxBytes, f.listener.fecDataShards, f.listener.fecParityShards)
}

func (f *FakeConn) GetTxBytes() uint64 {
	return atomic.LoadUint64(&f.txBytes)
}

func (f *FakeConn) GetRxBytes() uint64 {
	return f.rxBytes
}

func (f *FakeConn) GetDropRxBytes() uint64 {
	return f.dropRxBytes
}

var fecTest bool = false
var test_num int = 0

// 业务层ReadFull, 不一定一次读取完, 先返回recvLeft, 再从recvch 中读取数据
func (f *FakeConn) Read(b []byte) (int, error) {
	f.readLock.Lock()
	defer f.readLock.Unlock()
	// 先返回recvLeft
	if len(f.recvLeft) > 0 {
		n := copy(b, f.recvLeft)
		f.recvLeft = f.recvLeft[n:]
		f.rxBytes += uint64(n)
		return n, nil
	}

	var data []byte
	if f.fec != nil {
		var err error
		data, err = f.fec.DecodeData(func() ([]byte, error) {
			for {
				data, ok := <-f.recvch
				if !ok {
					return nil, fmt.Errorf("%v, recvch closed", f)
				}
				if len(data) == 0 {
					continue
				}

				if fecTest {
					test_num++
					log.Printf("recv data len: %d, test_num: %d", len(data), test_num)
					//在不丢包的环境里模拟丢包, 测试配置项 dataShards 10, parityShards 2, 所以每12个包， 模拟丢包一次 或者 模拟丢包2次
					if test_num%12 == 5 || test_num%12 == 6 {
						log.Printf("simulate drop packet for fec test recover, test_num: %d", test_num)
						continue //模拟丢包
					}
				}
				return data, nil
			}
		})
		if err != nil {
			return 0, err
		}
	} else {
		var ok bool
		data, ok = <-f.recvch
		if !ok {
			return 0, errors.New("recvch closed")
		}
	}

	n := copy(b, data)
	// 剩余数据保存到recvLeft
	if n < len(data) {
		f.recvLeft = data[n:]
	}
	f.rxBytes += uint64(n)
	return n, nil
}

func (f *FakeConn) Push(b []byte) {
	select {
	case f.recvch <- b:
		f.ts = time.Now()
	default:
		f.dropRxBytes += uint64(len(b))
		return
	}
}
func (f *FakeConn) Write(b []byte) (int, error) {
	return f.WriteTo(b, f.addr)
}

func (f *FakeConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if f.fec != nil {
		err := f.fec.EncodeData(b, func(p []byte) error {
			n, err := f.fakeConn.WriteTo(p, addr)
			if err != nil {
				return err
			}
			atomic.AddUint64(&f.txBytes, uint64(n))
			return nil
		})
		if err != nil {
			return 0, err
		}
		return len(b), nil
	}

	n, err := f.fakeConn.WriteTo(b, addr)
	if err != nil {
		return 0, err
	}
	atomic.AddUint64(&f.txBytes, uint64(n))
	return n, nil
}

func (f *FakeConn) Close() error {
	return f.close(false)
}

func (f *FakeConn) close(immediately bool) error {
	f.Lock()
	defer f.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true

	//必须要从listener 的connMap 中删除，否则后面收到相同地址的报文，就不会再创建fakeconn，即Accept 不会返回该fakeconn。
	//但是为了避免这个流还有残留的数据,导致错误认为新的连接，是不是需要延迟删除？TODO: 最好关联底层flow 来删除。
	if immediately {
		f.listener.deleteFakeConn(f)
	} else {
		f.listener.deleteFakeConnDelay(f, time.Now().Add(time.Second*10))
	}

	f.cancel()
	return nil
}

func (f *FakeConn) LocalAddr() net.Addr {
	return f.laddr
}

func (f *FakeConn) RemoteAddr() net.Addr {
	return f.addr
}

func (f *FakeConn) SetDeadline(t time.Time) error {
	return nil
}

func (f *FakeConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (f *FakeConn) SetWriteDeadline(t time.Time) error {
	return nil
}

var _ net.Listener = (*Listener)(nil)

type Listener struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	fakeConn *tcpraw.TCPConn
	laddr    *net.TCPAddr
	mu       sync.RWMutex
	connMap  map[common.AddrKey]*FakeConn
	delayMap map[*FakeConn]time.Time

	connChan chan *FakeConn

	fecEnable       bool
	fecDataShards   int
	fecParityShards int
}

type ListenerOption func(*Listener)

func WithListenerFec(dataShards, parityShards int) ListenerOption {
	return func(l *Listener) {
		if dataShards <= 0 || parityShards <= 0 {
			panic("fec data shards or parity shards must be greater than 0")
		}
		l.fecEnable = true
		l.fecDataShards = dataShards
		l.fecParityShards = parityShards
	}
}

// FakeTcpListen 其实是合并 l :=NewFakeTcpListener 和 l.Listen(), 更加方便使用。
func FakeTcpListen(ctx context.Context, addr string, opts ...ListenerOption) (*Listener, error) {
	l := NewFakeTcpListener(ctx)
	for _, opt := range opts {
		opt(l)
	}
	if err := l.Listen(addr); err != nil {
		return nil, err
	}
	return l, nil
}

func NewFakeTcpListener(ctx context.Context) *Listener {
	ctx, cancel := context.WithCancel(ctx)
	return &Listener{
		ctx:      ctx,
		cancel:   cancel,
		connMap:  make(map[common.AddrKey]*FakeConn, 128),
		connChan: make(chan *FakeConn, 1024),
		delayMap: make(map[*FakeConn]time.Time, 128),
	}
}

func (l *Listener) String() string {
	return fmt.Sprintf("listener: %s, connMap: %d", l.laddr.String(), len(l.connMap))
}

// 实现 net.Listener 接口Addr 方法
func (l *Listener) Addr() net.Addr {
	return l.laddr
}

/*
服务端设备上:
listener 启动并接受连接后，netstat -anlp|grep 9191 可以看到两个socket:
# netstat -anlp|grep 9191
tcp        0      0 192.168.4.208:9191      0.0.0.0:*               LISTEN      3132762/./server
tcp        0      0 192.168.4.208:9191      192.168.4.202:52220     ESTABLISHED 3132762/./server

但是过一会，只剩下侦听的socket了。
root@ubuntu2204:/home/obc# netstat -anlp|grep 9191
tcp        0      0 192.168.4.208:9191      0.0.0.0:*               LISTEN      3132762/./server
也就是ACCEPT 生产的socket 被关闭了。但是不影响业务层收发数据。

// listener 侦听后，iptables 规则如下:
Chain OUTPUT (policy ACCEPT 0 packets, 0 bytes)

	pkts bytes target     prot opt in     out     source               destination
	 245 13688 DROP       tcp  --  *      *       0.0.0.0/0            0.0.0.0/0            TTL match TTL == 1 tcp spt:9191

//服务端只需要一条规则就可以丢弃所有本地发出的TTL=1 的TCP 包。
// 客户端要每次dial 成功时，都要针对指定的ip和端口设置规则，其实可以只需要一条规则, 避免每次都要设置规则，如果忘记删除，规则会越来越多。
------------------
客户端设备上: 客户端连接后，ss -i |grep 9191 -A 1 可以看到连接状态， 过会就没了。
root@gw:/home/ubuntu# ss -i |grep 9191 -A 1
tcp   ESTAB    0      0                     192.168.4.202:52220       192.168.4.208:9191

	bbr wscale:8,8 rto:204 rtt:0.6/0.3 mss:1448 pmtu:1500 rcvmss:536 advmss:1448 cwnd:10 bytes_acked:1 segs_out:11 segs_in:137 data_segs_in:136 bbr:(bw:0bps,mrtt:0.6,pacing_gain:1,cwnd_gain:1) send 193Mbps lastsnd:136356 lastrcv:136356 lastack:136356 pacing_rate 552Mbps delivered:1 app_limited rcv_space:14480 rcv_ssthresh:42242 minrtt:0.6

root@gw:/home/ubuntu#
root@gw:/home/ubuntu# netstat -anlp|grep 9191
root@gw:/home/ubuntu# ss -i |grep 9191 -A 1
root@gw:/home/ubuntu#

root@gw:/home/ubuntu# iptables -nvL
Chain OUTPUT (policy ACCEPT 0 packets, 0 bytes)

	pkts bytes target     prot opt in     out     source               destination
	  10   520 DROP       tcp  --  *      *       192.168.4.202        192.168.4.208        TTL match TTL == 1 tcp spt:52220 dpt:9191

这个连接被iptables 丢弃了10个报文后，就close了?。
*/
func (l *Listener) Listen(address string) error {
	// resolve address
	laddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return err
	}
	l.laddr = laddr

	//对于server 而已，如果侦听了0.0.0.0:port， listen 时，创建多个handle，
	// 每个handle指定了一个本地ip作为listen源地址，handle 抓到的报文的目的ip肯定是handle 绑定的侦听ip,
	// conn.ReadFrom 时，得到的流已经是绑定指定的handle,且一定是能关联的真实的tcpconn.
	// conn.WriteTo 时，只是根据目的地址addr 来找到对应的flow, 而flow 是有多个handle 生成的，可能冲突。只是概率低而已。bug还是要修复。
	conn, err := tcpraw.Listen("tcp", address)
	if err != nil {
		log.Panicln(err)
	}
	err = conn.SetReadBuffer(1024 * 1024 * 5)
	if err != nil {
		log.Println(err)
	}
	err = conn.SetWriteBuffer(1024 * 1024 * 5)
	if err != nil {
		log.Println(err)
	}

	l.fakeConn = conn
	l.wg.Add(1)
	//go l.acceptDataLoop()
	go l.acceptDataLoopv2()
	l.wg.Add(1)
	go l.cleaner()
	return nil
}

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case <-l.ctx.Done():
		return nil, errors.New("listener context done")
	case conn := <-l.connChan:
		return conn, nil
	}
}

func (l *Listener) deleteFakeConnDelay(fc *FakeConn, delay time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.delayMap[fc] = delay
}

func (l *Listener) Close() error {
	if l.cancel != nil {
		l.cancel()
	}
	if l.fakeConn != nil {
		l.fakeConn.Close()
	}
	l.closeAllFakeConns()

	log.Println("listener closing, wait all tasks done")
	l.wg.Wait()
	log.Println("all tasks done, listener closed")
	return nil
}

func (l *Listener) closeAllFakeConns() {
	for {
		fc, err := l.FatchOneFakeConn()
		if err != nil {
			break
		}
		fc.close(true) //这个函数可能调用listener的锁，为了避免死锁, 这里不能持有listener的锁。
	}
}

func (l *Listener) FatchOneFakeConn() (*FakeConn, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, fc := range l.connMap {
		return fc, nil
	}
	return nil, errors.New("no fake conn found")
}

func (l *Listener) newFakeConn(addr net.Addr) *FakeConn {
	ctx, cancel := context.WithCancel(l.ctx)
	fc := &FakeConn{
		ctx:      ctx,
		cancel:   cancel,
		listener: l,
		laddr:    l.laddr,
		addr:     addr,
		fakeConn: l.fakeConn,
		recvch:   make(chan []byte, 2048), //put操作是非阻塞, 队列满马上丢弃, 所以这里设置大一点，避免队列满而drop数据。
		sendch:   make(chan []byte, 1024),
	}
	if l.fecEnable {
		fc.fec = NewFec(l.fecDataShards, l.fecParityShards, 0)
	}
	return fc
}

func (l *Listener) acceptDataLoop() {
	defer l.Close() //mo: last defer, 最后执行关闭listener。
	defer l.wg.Done()
	buf := make([]byte, tcpraw.MaxPayloadSize)
	for {
		n, addr, err := l.fakeConn.ReadFrom(buf)
		if err != nil {
			panic(fmt.Sprintf("listener readFrom error: %v", err))
			return
		}
		//log.Printf("listener received bytes: %d, addr: %s", n, addr.String())
		// conn.ReadFrom 时，得到的流已经是绑定指定的handle,且一定是能关联的真实的tcpconn.
		// 这里根据addr 创建FakeConn 在发送数据时，tcpraw 可以使用handle.LocalAddr() 作为本地地址, 也可以使用tcpraw flow 绑定的真实的tcpconn的源地址。
		//先判断connMap是否存在
		l.mu.RLock()
		if fc, ok := l.connMap[common.AddrToKey(addr)]; ok {
			l.mu.RUnlock()
			data := make([]byte, n)
			copy(data, buf[:n])
			//log.Println("listener push bytes:", n, "data:", string(data))
			fc.Push(data) //非阻塞。避免影响后面其他流的数据的读取。
			continue
		}
		l.mu.RUnlock()

		fc := l.newFakeConn(addr)
		log.Printf("new connection from:%s local addr:%s, fec:%d-%d", addr.String(), l.laddr.String(), l.fecDataShards, l.fecParityShards)

		//非阻塞。
		select {
		case l.connChan <- fc:
			l.mu.Lock()
			l.connMap[common.AddrToKey(addr)] = fc
			l.mu.Unlock()
			data := make([]byte, n)
			copy(data, buf[:n])
			//log.Println("listener push bytes:", n, "data:", string(data))
			fc.Push(data)
		default:
			log.Println("listener connChan is full, drop connection from:", addr.String())
		}
	}
}

// acceptDataLoopv2 是 acceptDataLoop 的升级版本, 少了两次复制数据的操作。
// 1. 直接从 fakeConn ReadFromv2 直接获取数据，而不是从 buf 复制数据。
// 2. 直接调用 fc.Push(data) 而不是复制数据到 data 后调用 fc.Push(data)。
func (l *Listener) acceptDataLoopv2() {
	defer l.Close() //mo: last defer, 最后执行关闭listener。
	defer l.wg.Done()

	for {
		data, addr, err := l.fakeConn.ReadFromv2()
		if err != nil {
			panic(fmt.Sprintf("listener readFrom error: %v", err))
			return
		}
		//log.Printf("listener received bytes: %d, addr: %s", n, addr.String())
		// conn.ReadFrom 时，得到的流已经是绑定指定的handle,且一定是能关联的真实的tcpconn.
		// 这里根据addr 创建FakeConn 在发送数据时，tcpraw 可以使用handle.LocalAddr() 作为本地地址, 也可以使用tcpraw flow 绑定的真实的tcpconn的源地址。
		//先判断connMap是否存在
		l.mu.RLock()
		if fc, ok := l.connMap[common.AddrToKey(addr)]; ok {
			l.mu.RUnlock()
			fc.Push(data) //非阻塞。避免影响后面其他流的数据的读取。
			continue
		}
		l.mu.RUnlock()

		fc := l.newFakeConn(addr)
		log.Printf("new connection from:%s local addr:%s, fec:%d-%d", addr.String(), l.laddr.String(), l.fecDataShards, l.fecParityShards)

		//非阻塞。
		select {
		case l.connChan <- fc:
			l.mu.Lock()
			l.connMap[common.AddrToKey(addr)] = fc
			l.mu.Unlock()
			fc.Push(data)
		default:
			log.Println("listener connChan is full, drop connection from:", addr.String())
		}
	}
}

func (l *Listener) deleteFakeConn(fc *FakeConn) {
	l.mu.Lock()
	delete(l.connMap, common.AddrToKey(fc.addr))
	l.mu.Unlock()

	//TODO: 删除fakeconn 对应的flow表项, 现在可以让对应的flow过期自动删除
}

func (l *Listener) cleaner() {
	defer l.wg.Done()
	DelayDeleteTicker := time.NewTicker(time.Second * 10)
	defer DelayDeleteTicker.Stop()

	expireTicker := time.NewTicker(time.Second * 33)
	defer expireTicker.Stop()
	expire := time.Minute
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-DelayDeleteTicker.C: //延迟删除fakeconn
			l.mu.Lock()
			for fc, t := range l.delayMap {
				if time.Now().After(t) {
					delete(l.connMap, common.AddrToKey(fc.addr))
					delete(l.delayMap, fc)
				}
			}
			l.mu.Unlock()
		case <-expireTicker.C: //超时删除fakeconn
			deleteFcs := []*FakeConn{}
			l.mu.RLock()
			for _, fc := range l.connMap {
				if time.Since(fc.ts) > expire {
					deleteFcs = append(deleteFcs, fc)
				}
			}
			l.mu.RUnlock()

			for _, fc := range deleteFcs {
				fc.close(true)
			}
		}
	}
}
