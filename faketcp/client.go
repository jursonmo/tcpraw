package faketcp

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jursonmo/tcpraw"
)

// TODO: 增加context 支持
func Dial(ctx context.Context, address string, opts ...ClientOption) (net.Conn, error) {
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

	c := NewClient(conn, remoteAddr, opts...)
	return c, nil
}

type Client struct {
	fakeConn   *tcpraw.TCPConn
	remoteAddr *net.TCPAddr

	txBytes     uint64
	rxBytes     uint64
	dropRxBytes uint64

	fec             *Fec
	fecDataShards   int
	fecParityShards int
}

type ClientOption func(*Client)

func WithClientFec(dataShards, parityShards int) ClientOption {
	return func(c *Client) {
		if dataShards <= 0 || parityShards <= 0 {
			panic("fec data shards or parity shards must be greater than 0")
		}
		c.fec = NewFec(dataShards, parityShards, 0)
		c.fecDataShards = dataShards
		c.fecParityShards = parityShards
	}
}

func NewClient(fakeConn *tcpraw.TCPConn, remoteAddr *net.TCPAddr, opts ...ClientOption) *Client {
	c := &Client{fakeConn: fakeConn, remoteAddr: remoteAddr}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) Read(b []byte) (int, error) {
	if c.fec != nil {
		data, err := c.fec.DecodeData(func() ([]byte, error) {
			fecBuf := make([]byte, len(b)+FecHeaderSizePlus2)
			n, _, err := c.fakeConn.ReadFrom(fecBuf)
			if err != nil {
				return nil, err
			}
			return fecBuf[:n], nil
		})
		if err != nil {
			return 0, err
		}

		copy(b, data)
		c.rxBytes += uint64(len(data))
		return len(data), nil
	}

	n, _, err := c.fakeConn.ReadFrom(b) //这里能收到数据，已经证明不是孤立的flow了。已经关联了真实tcp conn。
	if err != nil {
		return 0, err
	}
	c.rxBytes += uint64(n)
	return n, nil
}

func (c *Client) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if len(b) > 1530 { //1514 + mvnet_header +
		return 0, fmt.Errorf("packet size %d > mtuLimit %d", len(b), mtuLimit)
	}

	if c.fec != nil {
		err := c.fec.EncodeData(b, func(p []byte) error {
			n, err := c.fakeConn.WriteTo(p, c.remoteAddr) //最后都是通过 flow 绑定的handle 发送出去的。
			if err != nil {
				return err
			}
			c.txBytes += uint64(n)
			return nil
		})
		if err != nil {
			return 0, err
		}
		return len(b), nil //成功, 默认返回业务层期望的值len(b), 而不是真实WriteTo的返回值n, 因为n 大于len(b), 如果返回n, 业务层会confused。
	}

	n, err := c.fakeConn.WriteTo(b, c.remoteAddr) //最后都是通过 flow 绑定的handle 发送出去的。
	if err != nil {
		return 0, err
	}
	c.txBytes += uint64(n)
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
