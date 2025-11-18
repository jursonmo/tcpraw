./server_fec -l 192.168.4.208:9191 -d 10 -p 2
./client_fec -addr 192.168.4.208:9191 -c 100 -d 10 -p 2

-------------------------------------------------------------------------------------
### 服务端接受报文时，模拟丢包场景：dataShards 10, parityShards 2, 每12个包， 模拟丢包一次. 验证fec能否正确恢复丢失的包
```go
func (f *FakeConn) Read(b []byte) (int, error) {
    .....
    .....
				if fecTest {
					test_num++
					log.Printf("recv data len: %d, test_num: %d", len(data), test_num)
					//在不丢包的环境里模拟丢包, 测试配置项 dataShards 10, parityShards 2, 所以每12个包， 模拟丢包一次
					if test_num%12 == 5 {
						log.Printf("simulate drop packet for fec test recover, test_num: %d", test_num)
						continue //模拟丢包
					}
				}
    .....
    .....
}
```

#### 查看服务端打印日志
```
root@ubuntu2204:~/faketcp# ./server_fec -l 192.168.4.208:9191 -d 10 -p 2
2025/11/18 03:20:25 fec enabled, data shards: 10, parity shards: 2
2025/11/18 03:20:25 reuseport enabled, reuseport num: 1
2025/11/18 03:20:25 use read batch, batch size: 4
2025/11/18 03:20:25 captureFlow: not target port:9191 packet, local: 192.168.4.208, received tcp data, src port: 62343, dst port: 22, remote: 192.168.4.133, len: 32
2025/11/18 03:20:25 server listening on: 192.168.4.208:9191


2025/11/18 03:20:31 listener: 192.168.4.208:9191 new flow: {3232236766 55916} shardIndex: 5
2025/11/18 03:20:31 captureFlow, shardIndex:5, SYN packet local: 192.168.4.208:9191, remote: 192.168.4.222:55916, seq:3009719037 ack:0
2025/11/18 03:20:32 new connection from:192.168.4.222:55916 local addr:192.168.4.208:9191, fec:10-2
2025/11/18 03:20:32 ok, server accepted client: 192.168.4.222:55916 local addr: 192.168.4.208:9191
2025/11/18 03:20:32 recv data len: 25, test_num: 1
2025/11/18 03:20:32 server received from fakeconn: 192.168.4.222:55916, bytes: 17, data:xx hello server 0
2025/11/18 03:20:32 ok server send bytes: 17 data: xx hello server 0
2025/11/18 03:20:32 recv data len: 15, test_num: 2
2025/11/18 03:20:32 server received from fakeconn: 192.168.4.222:55916, bytes: 7, data:hello 1
2025/11/18 03:20:32 ok server send bytes: 7 data: hello 1
2025/11/18 03:20:33 recv data len: 25, test_num: 3
2025/11/18 03:20:33 server received from fakeconn: 192.168.4.222:55916, bytes: 17, data:xx hello server 2
2025/11/18 03:20:33 ok server send bytes: 17 data: xx hello server 2
2025/11/18 03:20:33 recv data len: 15, test_num: 4
2025/11/18 03:20:33 server received from fakeconn: 192.168.4.222:55916, bytes: 7, data:hello 3
2025/11/18 03:20:33 ok server send bytes: 7 data: hello 3
2025/11/18 03:20:34 recv data len: 25, test_num: 5
2025/11/18 03:20:34 simulate drop packet for fec test recover, test_num: 5
2025/11/18 03:20:34 recv data len: 15, test_num: 6
2025/11/18 03:20:34 server received from fakeconn: 192.168.4.222:55916, bytes: 7, data:hello 5
2025/11/18 03:20:34 ok server send bytes: 7 data: hello 5
2025/11/18 03:20:35 recv data len: 25, test_num: 7
2025/11/18 03:20:35 server received from fakeconn: 192.168.4.222:55916, bytes: 17, data:xx hello server 6
2025/11/18 03:20:35 ok server send bytes: 17 data: xx hello server 6
2025/11/18 03:20:35 recv data len: 15, test_num: 8
2025/11/18 03:20:35 server received from fakeconn: 192.168.4.222:55916, bytes: 7, data:hello 7
2025/11/18 03:20:35 ok server send bytes: 7 data: hello 7
2025/11/18 03:20:36 recv data len: 25, test_num: 9
2025/11/18 03:20:36 server received from fakeconn: 192.168.4.222:55916, bytes: 17, data:xx hello server 8
2025/11/18 03:20:36 ok server send bytes: 17 data: xx hello server 8
2025/11/18 03:20:36 recv data len: 15, test_num: 10
2025/11/18 03:20:36 server received from fakeconn: 192.168.4.222:55916, bytes: 7, data:hello 9
2025/11/18 03:20:36 ok server send bytes: 7 data: hello 9
2025/11/18 03:20:36 recv data len: 25, test_num: 11
2025/11/18 03:20:36 server received from fakeconn: 192.168.4.222:55916, bytes: 17, data:xx hello server 4
2025/11/18 03:20:36 ok server send bytes: 17 data: xx hello server 4
2025/11/18 03:20:36 recv data len: 25, test_num: 12
2025/11/18 03:20:37 recv data len: 26, test_num: 13
2025/11/18 03:20:37 server received from fakeconn: 192.168.4.222:55916, bytes: 18, data:xx hello server 10
2025/11/18 03:20:37 ok server send bytes: 18 data: xx hello server 10
2025/11/18 03:20:37 recv data len: 16, test_num: 14
2025/11/18 03:20:37 server received from fakeconn: 192.168.4.222:55916, bytes: 8, data:hello 11
2025/11/18 03:20:37 ok server send bytes: 8 data: hello 11
2025/11/18 03:20:38 recv data len: 26, test_num: 15
2025/11/18 03:20:38 server received from fakeconn: 192.168.4.222:55916, bytes: 18, data:xx hello server 12
2025/11/18 03:20:38 ok server send bytes: 18 data: xx hello server 12
2025/11/18 03:20:38 recv data len: 16, test_num: 16
2025/11/18 03:20:38 server received from fakeconn: 192.168.4.222:55916, bytes: 8, data:hello 13
2025/11/18 03:20:38 ok server send bytes: 8 data: hello 13
2025/11/18 03:20:39 recv data len: 26, test_num: 17
2025/11/18 03:20:39 simulate drop packet for fec test recover, test_num: 17
2025/11/18 03:20:39 recv data len: 16, test_num: 18
2025/11/18 03:20:39 server received from fakeconn: 192.168.4.222:55916, bytes: 8, data:hello 15
2025/11/18 03:20:39 ok server send bytes: 8 data: hello 15
2025/11/18 03:20:40 recv data len: 26, test_num: 19
2025/11/18 03:20:40 server received from fakeconn: 192.168.4.222:55916, bytes: 18, data:xx hello server 16
2025/11/18 03:20:40 ok server send bytes: 18 data: xx hello server 16
2025/11/18 03:20:40 recv data len: 16, test_num: 20
2025/11/18 03:20:40 server received from fakeconn: 192.168.4.222:55916, bytes: 8, data:hello 17
2025/11/18 03:20:40 ok server send bytes: 8 data: hello 17
2025/11/18 03:20:41 recv data len: 26, test_num: 21
2025/11/18 03:20:41 server received from fakeconn: 192.168.4.222:55916, bytes: 18, data:xx hello server 18
2025/11/18 03:20:41 ok server send bytes: 18 data: xx hello server 18
2025/11/18 03:20:41 recv data len: 16, test_num: 22
2025/11/18 03:20:41 server received from fakeconn: 192.168.4.222:55916, bytes: 8, data:hello 19
2025/11/18 03:20:41 ok server send bytes: 8 data: hello 19
2025/11/18 03:20:41 recv data len: 26, test_num: 23
2025/11/18 03:20:41 server received from fakeconn: 192.168.4.222:55916, bytes: 18, data:xx hello server 14
2025/11/18 03:20:41 ok server send bytes: 18 data: xx hello server 14
2025/11/18 03:20:41 recv data len: 26, test_num: 24
2025/11/18 03:20:42 recv data len: 26, test_num: 25
2025/11/18 03:20:42 server received from fakeconn: 192.168.4.222:55916, bytes: 18, data:xx hello server 20
2025/11/18 03:20:42 ok server send bytes: 18 data: xx hello server 20
2025/11/18 03:20:42 recv data len: 16, test_num: 26
2025/11/18 03:20:42 server received from fakeconn: 192.168.4.222:55916, bytes: 8, data:hello 21
2025/11/18 03:20:42 ok server send bytes: 8 data: hello 21

^C
root@ubuntu2204:~/faketcp#
```
#### 结果是能正确恢复丢失的包. 恢复报文“xx hello server 4”  和 “xx hello server 14”

----------------------------------------------------
### 模拟丢包2次
```go
				if fecTest {
					test_num++
					log.Printf("recv data len: %d, test_num: %d", len(data), test_num)
					//在不丢包的环境里模拟丢包, 测试配置项 dataShards 10, parityShards 2, 所以每12个包，模拟丢包2次
					if test_num%12 == 5 || test_num%12 == 6 {
						log.Printf("simulate drop packet for fec test recover, test_num: %d", test_num)
						continue //模拟丢包
					}
				}
```
#### 查看服务端打印日志
```
root@ubuntu2204:~/faketcp# ./server_fec -l 192.168.4.208:9191 -d 10 -p 2
2025/11/18 03:38:37 fec enabled, data shards: 10, parity shards: 2
2025/11/18 03:38:37 reuseport enabled, reuseport num: 1
2025/11/18 03:38:37 use read batch, batch size: 4
2025/11/18 03:38:37 server listening on: 192.168.4.208:9191

2025/11/18 03:38:41 listener: 192.168.4.208:9191 new flow: {3232236766 34102} shardIndex: 7
2025/11/18 03:38:41 captureFlow, shardIndex:7, SYN packet local: 192.168.4.208:9191, remote: 192.168.4.222:34102, seq:2511430264 ack:0
2025/11/18 03:38:42 new connection from:192.168.4.222:34102 local addr:192.168.4.208:9191, fec:10-2
2025/11/18 03:38:42 ok, server accepted client: 192.168.4.222:34102 local addr: 192.168.4.208:9191
2025/11/18 03:38:42 recv data len: 25, test_num: 1
2025/11/18 03:38:42 server received from fakeconn: 192.168.4.222:34102, bytes: 17, data:xx hello server 0
2025/11/18 03:38:42 ok server send bytes: 17 data: xx hello server 0
2025/11/18 03:38:42 recv data len: 15, test_num: 2
2025/11/18 03:38:42 server received from fakeconn: 192.168.4.222:34102, bytes: 7, data:hello 1
2025/11/18 03:38:42 ok server send bytes: 7 data: hello 1
2025/11/18 03:38:43 recv data len: 25, test_num: 3
2025/11/18 03:38:43 server received from fakeconn: 192.168.4.222:34102, bytes: 17, data:xx hello server 2
2025/11/18 03:38:43 ok server send bytes: 17 data: xx hello server 2
2025/11/18 03:38:43 recv data len: 15, test_num: 4
2025/11/18 03:38:43 server received from fakeconn: 192.168.4.222:34102, bytes: 7, data:hello 3
2025/11/18 03:38:43 ok server send bytes: 7 data: hello 3
2025/11/18 03:38:44 recv data len: 25, test_num: 5
2025/11/18 03:38:44 simulate drop packet for fec test recover, test_num: 5
2025/11/18 03:38:44 recv data len: 15, test_num: 6
2025/11/18 03:38:44 simulate drop packet for fec test recover, test_num: 6
2025/11/18 03:38:45 recv data len: 25, test_num: 7
2025/11/18 03:38:45 server received from fakeconn: 192.168.4.222:34102, bytes: 17, data:xx hello server 6
2025/11/18 03:38:45 ok server send bytes: 17 data: xx hello server 6
2025/11/18 03:38:45 recv data len: 15, test_num: 8
2025/11/18 03:38:45 server received from fakeconn: 192.168.4.222:34102, bytes: 7, data:hello 7
2025/11/18 03:38:45 ok server send bytes: 7 data: hello 7
2025/11/18 03:38:46 recv data len: 25, test_num: 9
2025/11/18 03:38:46 server received from fakeconn: 192.168.4.222:34102, bytes: 17, data:xx hello server 8
2025/11/18 03:38:46 ok server send bytes: 17 data: xx hello server 8
2025/11/18 03:38:46 recv data len: 15, test_num: 10
2025/11/18 03:38:46 server received from fakeconn: 192.168.4.222:34102, bytes: 7, data:hello 9
2025/11/18 03:38:46 ok server send bytes: 7 data: hello 9
2025/11/18 03:38:46 recv data len: 25, test_num: 11
2025/11/18 03:38:46 recv data len: 25, test_num: 12
2025/11/18 03:38:46 server received from fakeconn: 192.168.4.222:34102, bytes: 17, data:xx hello server 4
2025/11/18 03:38:46 ok server send bytes: 17 data: xx hello server 4
2025/11/18 03:38:46 server received from fakeconn: 192.168.4.222:34102, bytes: 7, data:hello 5
2025/11/18 03:38:46 ok server send bytes: 7 data: hello 5
2025/11/18 03:38:47 recv data len: 26, test_num: 13
2025/11/18 03:38:47 server received from fakeconn: 192.168.4.222:34102, bytes: 18, data:xx hello server 10
2025/11/18 03:38:47 ok server send bytes: 18 data: xx hello server 10
2025/11/18 03:38:47 recv data len: 16, test_num: 14
2025/11/18 03:38:47 server received from fakeconn: 192.168.4.222:34102, bytes: 8, data:hello 11
2025/11/18 03:38:47 ok server send bytes: 8 data: hello 11
2025/11/18 03:38:48 recv data len: 26, test_num: 15
2025/11/18 03:38:48 server received from fakeconn: 192.168.4.222:34102, bytes: 18, data:xx hello server 12
2025/11/18 03:38:48 ok server send bytes: 18 data: xx hello server 12
````

#### 结果是能正确恢复丢失的包. 恢复报文“xx hello server 4” 和 报文“hello 5”