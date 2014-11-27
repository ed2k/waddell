package waddell

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"

	"github.com/getlantern/framed"
	"github.com/getlantern/tlsdefaults"
	"github.com/oxtoacart/bpool"
)

const (
	DefaultNumBuffers = 10000
)

// Server is a waddell server
type Server struct {
	// NumBuffers: number of buffers to cache for reading and writing (balances
	// overall memory consumption against CPU usage).  Defaults to 10,000.
	NumBuffers int

	// BufferBytes: size of each buffer (this places a cap on the maxmimum
	// message size that can be transmitted).  Defaults to 65,535.
	BufferBytes int

	peers      map[PeerId]*peer // connected peers by id
	peersMutex sync.RWMutex     // protects access to peers map
	buffers    *bpool.BytePool  // pool of buffers for reading/writing
}

// Listen creates a listener at the given address. pkfile and certfile are
// optional. If both are specified, connections will be secured with TLS.
func Listen(addr string, pkfile string, certfile string) (net.Listener, error) {
	if (pkfile != "" && certfile == "") || (pkfile == "" && certfile != "") {
		return nil, fmt.Errorf("Please specify both pkfile and certfile")
	}
	if pkfile != "" {
		return listenTLS(addr, pkfile, certfile)
	} else {
		return net.Listen("tcp", addr)
	}
}

// Serve starts the waddell server using the given listener
func (server *Server) Serve(listener net.Listener) error {
	// Set default values
	if server.NumBuffers == 0 {
		server.NumBuffers = DefaultNumBuffers
	}
	if server.BufferBytes == 0 {
		server.BufferBytes = framed.MaxFrameSize
	}

	server.buffers = bpool.NewBytePool(server.NumBuffers, server.BufferBytes)
	server.peers = make(map[PeerId]*peer)

	for {
		conn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("Error accepting connection: %s", err)
		}
		p := server.addPeer(&peer{
			server: server,
			id:     randomPeerId(),
			conn:   conn,
			reader: framed.NewReader(conn),
			writer: framed.NewWriter(conn),
		})
		go p.run()
	}
}

func listenTLS(addr string, pkfile string, certfile string) (net.Listener, error) {
	cert, err := tls.LoadX509KeyPair(certfile, pkfile)
	if err != nil {
		return nil, fmt.Errorf("Unable to load cert and pk: %s", err)
	}

	cfg := tlsdefaults.Server()
	cfg.MinVersion = tls.VersionTLS12 // force newest available version of TLS
	cfg.Certificates = []tls.Certificate{cert}
	return tls.Listen("tcp", addr, cfg)
}

type peer struct {
	server *Server
	id     PeerId
	conn   net.Conn
	reader *framed.Reader
	writer *framed.Writer
}

func (server *Server) addPeer(p *peer) *peer {
	server.peersMutex.Lock()
	defer server.peersMutex.Unlock()
	server.peers[p.id] = p
	return p
}

func (server *Server) getPeer(id PeerId) *peer {
	server.peersMutex.RLock()
	defer server.peersMutex.RUnlock()
	return server.peers[id]
}

func (server *Server) removePeer(id PeerId) {
	server.peersMutex.Lock()
	defer server.peersMutex.Unlock()
	delete(server.peers, id)
}

func (p *peer) run() {
	defer p.conn.Close()
	defer p.server.removePeer(p.id)

	// Tell the peer its id (and set topic to UnknownTopic)
	_, err := p.writer.WritePieces(p.id.toBytes(), UnknownTopic.toBytes())
	if err != nil {
		log.Debugf("Unable to send peerid on connect: %s", err)
		return
	}

	// Read messages until there are no more to read
	for {
		if !p.readNext() {
			return
		}
	}
}

func (p *peer) readNext() (ok bool) {
	b := p.server.buffers.Get()
	defer p.server.buffers.Put(b)
	n, err := p.reader.Read(b)
	if err != nil {
		return false
	}
	msg := b[:n]
	if len(msg) == 1 && msg[0] == keepAlive[0] {
		// Got a keepalive message, ignore it
		return true
	}
	to, err := readPeerId(msg)
	if err != nil {
		// Problem determining recipient
		log.Errorf("Unable to determine recipient: ", err.Error())
		return true
	}
	cto := p.server.getPeer(to)
	if cto == nil {
		// Recipient not found
		return true
	}
	// Set sender's id as the id in the message
	err = p.id.write(msg)
	if err != nil {
		return true
	}
	_, err = cto.writer.Write(msg)
	if err != nil {
		cto.conn.Close()
		return true
	}
	return true
}
