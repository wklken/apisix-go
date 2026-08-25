package server

import (
	"net"
	"sync"
)

type generationConn struct {
	net.Conn
	closeOnce  sync.Once
	closeErr   error
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
		connection.closeErr = connection.Conn.Close()
		connection.unregister()
		connection.release()
	})
	return connection.closeErr
}
