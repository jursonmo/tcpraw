package faketcp

import (
	"context"
	"errors"
	"net"
	"sync"
)

// Listeners对象是对Listener 的封装，实现侦听多个地址, 兼容单个地址的侦听。
// 同时实现net.Listener 接口, 方便接入原有业务。
var _ net.Listener = (*Listeners)(nil)

type Listeners struct {
	once      sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	listeners []*Listener
	addrs     []string
	connChan  chan net.Conn
}

func FakeTcpListeners(ctx context.Context, addrs ...string) (*Listeners, error) {
	ctx, cancel := context.WithCancel(ctx)
	listeners := &Listeners{
		ctx:       ctx,
		cancel:    cancel,
		addrs:     addrs,
		listeners: make([]*Listener, 0, len(addrs)),
		connChan:  make(chan net.Conn, 1024),
	}

	for _, addr := range addrs {
		listener, err := FakeTcpListen(listeners.ctx, addr)
		if err != nil {
			return nil, err
		}
		listeners.listeners = append(listeners.listeners, listener)
	}
	return listeners, nil
}

// Accept 会从多个listener 中accept 连接，并返回一个连接。
func (l *Listeners) Accept() (net.Conn, error) {
	l.once.Do(func() {
		for _, listener := range l.listeners {
			go func(listener *Listener) {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					l.connChan <- conn
				}
			}(listener)
		}
	})

	for {
		select {
		case <-l.ctx.Done():
			return nil, errors.New("listeners context done")
		case conn := <-l.connChan:
			return conn, nil
		}
	}
}

func (l *Listeners) Close() error {
	for _, listener := range l.listeners {
		listener.Close()
	}
	return nil
}

func (l *Listeners) Addr() net.Addr {
	//return l.listeners[0].Addr()
	return nil
}

// end of Listeners implementation
