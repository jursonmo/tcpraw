### tcpraw 实现逻辑：
#### client:
   + Dial :  net.DialIP("ip:tcp", nil, &net.IPAddr{IP: raddr.IP}), 指定Dial的目的ip 会生成handle, DialTCP 生成真是 realtcpsocket, handle 和 realtcpsocket 都会自动绑定一个本地ip地址，他们应该是一样的，只是真实tcp还自动绑定本地端口。通过真实tcp socket可以得到本端绑定端口, 在handle抓包时，可以根据这个端口过滤掉非该端口的tcp报文。
        真实tcp拨号成功后，会创建一个flow, 并且记录flow.conn = realtcpsocket, 以此说明该flow不是孤儿。只有不是孤儿的flow的数据才push推送到上层业务。也就是真实建立三次握手成功的flow 的数据才会被业务层读到。
    + handle := net.DialIP("ip:tcp"..), 从handle.ReadFromIp()的角度看收到的是tcp的数据。其实底层套接字读到的数据是包含ip头部的，只是golang c.fd.readFrom(b)->stripIPv4Header()把ip头部去掉了而已，也就是在给套接字设置bpf时，是要从ip头部开始设置bpf字节码。
    + handle 是一个原始套接字，有队列的，也就是真实tcp 拨号成功后再调用handle.captureFlow(),也是能看到握手过程中对方发送的syn 和 ack, 因为这些数据已经在handle对应的内核队列里了。可以根据这些报文更新seq ack.
    + 真实tcp 拨号成功后，设置socketopt ttl = 1, 然后配置iptables 把ttl为1的数据drop,实际就是drop掉所有真实tcp的报文。
    + handle 抓到数据后，根据对方数据的seq 是否等于flow记录的ack,才更新ack. 判断不是孤儿的flow的数据，就可以往上推送给业务层。
    + client 在发送数据时，也是需要根据目的ip找到对应的flow，然后根据flow的seq ack 构造数据报文。mo: 如果找不到flow, 可以认为底层连接断, 我的做法是返回错误。构造数据报文的源ip地址可以通过 handle.LocalAddr()获取，源端口可以从真实tcp socket获取。

#### server listener:
    + 根据Listen 侦听的ip 创建handle, 如果侦听0.0.0.0:9191, 那么根据本地的所有ip 都要创建原始socket handle, net.ListenIP("ip:tcp", &net.IPAddr{IP: ipaddr.IP}), net.ListenIP 只能侦听ip, 并且侦听tcp报文, 但是端口9191需要在抓包时过滤掉, 
    + 真实的TCPListen 会accept 真实tcp 连接，这样可以创建flow, 并且记录flow.conn = realtcpsocket.  以此说明该flow不是孤儿。只有不是孤儿的flow的数据才push推送到上层业务。
    + 其他跟client 差不多。

### 需要快速验证放在公网和放在能不能正常长时间的跑数据，注意为了验证运营商能否卡住这种连接，同时验证数据流经过Nat路由器，路由器是否会卡住这种连接。
   1. 在网络上，很容易有乱序，也就是tcpraw放到对方，对方接受时就可以能有乱序，就像udp， 如果隧道内的用户数据是tcp,它会自己排序，所以不要紧。
       或者以后tcpraw加上自定义头部后，可以先排序再推送到业务层。

### TODO list:
1. 锁：
   + 1.1 tcpraw flowtable 改成读写锁，且最好把锁sharding成多个锁，避免锁竞争。(DONE)
   + 1.2 减低锁的颗粒度，在captureFlow()-->lockflow()更新flow seq ack和发送数据时，都是在flowTable锁的保护下，多个不同的流会竞争flowTable锁， 为了减少不同流之间的锁竞争，把flow seq ack 的更新放在flow自己的锁里。(DONE) 
2. 由于为了业务层更方便接入，把流封装成net.Conn. (DONE)
3. bug: 
    + 3.1 对于服务端而已, 只以对端的信息作为flow 的key, 不需要本地ip和端口吗? 那么如果服务端侦听多个本地地址， 对方用同一个地址来连接，冲突怎么处理? conn.flowtable 是包含了所有handle 生成的flow表项的， 是有可能冲突的。(不好处理，因为发送数据时，只知道目的地址，不知道源地址，除非每个流生成一个对应的自定义myconn 对象，源地址记录在myconn里，然后由myconn来收发数据)
    + 3.2 cleaner这个协程没有用for来保证重复执行, 只执行一次就退出循环了。(DONE)
4. client 每次dial拨号都生成一个明显的iptables规则，这样容易累计过多的iptables规则，可以像listener那样，添加一个禁止所有ttl=1 的tcp 报文iptables规则即可。(DONE)
5. 如果对方是真实的tcp, 本地端是tcpraw能，即对端正常根据seq ack 来处理重传的，这会不会有异常。
6. 为了能像tcp 那样可以快速让对方关闭，发送的数据增加控制信息头部, 而不是单纯是用户数据。
7. 增加额外的头部信息后，还有可以增加序号，这样可以让双方都知道丢包率。为以后调整fec 参数。
8. 性能优化: 
   + 8.1 从抓包读到的数据报文到送到业务层，发生的拷贝次数有点多，可以适当减少。可以借鉴udpx.
   + 8.2 现在是在抓包后再过滤掉不想要的tcp端口的数据，最好是在内核层面过滤，使用socket BPF, 这样提高抓包的有效性。否则handle.cpatureflow会抓到对应ip非常多的tcp报文，但是都不是指定的tcp端口的报文，效率非常低。
   + 8.3 为了提高从内核读数据的效率，使用PacketConn.ReadBatch()调用底层recvmmsg：一次系统调用读到多个报文.（DONE)(先实现批量读，暂时不要考虑批量写，根据系统性能单个写数据， 但是批量读也有必要，不然来不及从内核队列里读取数据，就会被丢弃)
   + 8.4 实现批量写。
   + 8.5 给listener 可以增加REUSEADDR特性, 这样可以使多个listener侦听一个本地地址，其到负载作用吗？答:经过验证，原始套接字，使用 SO_REUSEPORT 并不能实现真正的负载均衡，多个原始套接字会收到相同的数据包副本。 为什么不能负载均衡 1. 数据包复制：内核将每个匹配的数据包复制到所有绑定的原始套接字 2.无分发逻辑：原始套接字没有像 TCP/UDP 那样的连接或流的概念

9. log: 增加日志，方便调试.
10. FakeConn 关联 flow, 这样flow 超时删除时，可以通知FakeConn 执行关闭操作。或者FakeConn关闭时，可以让flow 进入到timewait的状态，这个状态下即使收到数据，也不会往上层push送数据（flow 的作用类似于内核的socket维护）。
11. server 侦听0.0.0.0，会给每个本地每个ip 都创建一个handle来抓包，这样实在是太多了，目前tcp.raw.Listen()只能侦听单个ip. (DONE: Listeners对象是对Listener 的封装，实现侦听多个地址, 兼容单个地址的侦听. 接口FakeTcpListeners(ctx, addrs...).)。