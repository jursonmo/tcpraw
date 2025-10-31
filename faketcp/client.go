package faketcp

import (
	"fmt"
	"net"
	"time"

	"github.com/xtaci/tcpraw"
)

func Dial(network, address string) (net.Conn, error) {
	conn, err := tcpraw.Dial("tcp", address) //真实tcp 连接成功才返回conn, 否则返回nil, err
	if err != nil {
		return nil, err
	}

	err = conn.SetReadBuffer(1024 * 1024 * 3)
	if err != nil {
		return nil, err
	}
	err = conn.SetWriteBuffer(1024 * 1024 * 3)
	if err != nil {
		return nil, err
	}

	remoteAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}
	if conn.RemoteAddr().String() != remoteAddr.String() {
		return nil, fmt.Errorf("remote addr not match: fakeConn remote addr %s != %s", conn.RemoteAddr().String(), remoteAddr.String())
	}

	return &Client{fakeConn: conn, remoteAddr: remoteAddr}, nil
}

type Client struct {
	fakeConn   *tcpraw.TCPConn
	remoteAddr *net.TCPAddr
}

func (c *Client) Read(b []byte) (int, error) {
	n, _, err := c.fakeConn.ReadFrom(b) //这里能收到数据，已经证明不是孤立的flow了。已经关联了真实tcp conn。
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (c *Client) Write(b []byte) (int, error) {
	n, err := c.fakeConn.WriteTo(b, c.remoteAddr) //最后都是通过 flow 绑定的handle 发送出去的。
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (c *Client) Close() error {
	return c.fakeConn.Close()
}

func (c *Client) LocalAddr() net.Addr {
	return c.fakeConn.LocalAddr()
}

func (c *Client) RemoteAddr() net.Addr {
	return c.remoteAddr
}

func (c *Client) SetDeadline(t time.Time) error {
	return c.fakeConn.SetDeadline(t)
}

func (c *Client) SetReadDeadline(t time.Time) error {
	return c.fakeConn.SetReadDeadline(t)
}

func (c *Client) SetWriteDeadline(t time.Time) error {
	return c.fakeConn.SetWriteDeadline(t)
}
