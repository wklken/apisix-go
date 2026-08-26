package server

import (
	"net"
	"sync"
)

type generationConn struct {
	net.Conn
	closeOnce  sync.Once
	closeErr   error
	closePanic any
	release    func()
	unregister func()
}

func newGenerationConn(connection net.Conn, release, unregister func()) *generationConn {
	if release == nil {
		release = func() {}
	}
	if unregister == nil {
		unregister = func() {}
	}
	return &generationConn{Conn: connection, release: release, unregister: unregister}
}

func (connection *generationConn) Close() error {
	connection.closeOnce.Do(func() {
		connection.closePanic = captureGenerationConnPanic(func() {
			connection.closeErr = connection.Conn.Close()
		})
		if recovered := captureGenerationConnPanic(connection.unregister); connection.closePanic == nil {
			connection.closePanic = recovered
		}
		if recovered := captureGenerationConnPanic(connection.release); connection.closePanic == nil {
			connection.closePanic = recovered
		}
	})
	if connection.closePanic != nil {
		panic(connection.closePanic)
	}
	return connection.closeErr
}

func captureGenerationConnPanic(call func()) (panicValue any) {
	defer func() { panicValue = recover() }()
	call()
	return nil
}
