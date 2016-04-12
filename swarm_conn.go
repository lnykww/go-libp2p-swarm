package swarm

import (
	"fmt"

	inet "github.com/ipfs/go-libp2p/p2p/net"
	conn "github.com/ipfs/go-libp2p/p2p/net/conn"

	peer "gx/ipfs/QmY1xNhBfF9xA1pmD8yejyQAyd77K68qNN6JPM1CN2eiRu/go-libp2p-peer"
	ps "gx/ipfs/QmZK81vcgMhpb2t7GNbozk7qzt6Rj4zFqitpvsWT9mduW8/go-peerstream"
	context "gx/ipfs/QmZy2y8t9zQH2a1b8q2ZSLKp17ATuJoCNxxyMFG5qFExpt/go-net/context"
	ic "gx/ipfs/QmaP38GJApheTr84f8R89vsT7oJLQw1AeCz4HqrQgv2njB/go-libp2p-crypto"
	ma "gx/ipfs/QmcobAGsCjYt5DXoq9et9L8yR8er7o7Cu3DTvpaq12jYSz/go-multiaddr"
)

// a Conn is a simple wrapper around a ps.Conn that also exposes
// some of the methods from the underlying conn.Conn.
// There's **five** "layers" to each connection:
//  * 0. the net.Conn - underlying net.Conn (TCP/UDP/UTP/etc)
//  * 1. the manet.Conn - provides multiaddr friendly Conn
//  * 2. the conn.Conn - provides Peer friendly Conn (inc Secure channel)
//  * 3. the peerstream.Conn - provides peerstream / spdysptream happiness
//  * 4. the Conn - abstracts everyting out, exposing only key parts of underlying layers
// (I know, this is kinda crazy. it's more historical than a good design. though the
// layers do build up pieces of functionality. and they're all just io.RW :) )
type Conn ps.Conn

// ConnHandler is called when new conns are opened from remote peers.
// See peerstream.ConnHandler
type ConnHandler func(*Conn)

func (c *Conn) StreamConn() *ps.Conn {
	return (*ps.Conn)(c)
}

func (c *Conn) RawConn() conn.Conn {
	// righly panic if these things aren't true. it is an expected
	// invariant that these Conns are all of the typewe expect:
	// 		ps.Conn wrapping a conn.Conn
	// if we get something else it is programmer error.
	return (*ps.Conn)(c).NetConn().(conn.Conn)
}

func (c *Conn) String() string {
	return fmt.Sprintf("<SwarmConn %s>", c.RawConn())
}

// LocalMultiaddr is the Multiaddr on this side
func (c *Conn) LocalMultiaddr() ma.Multiaddr {
	return c.RawConn().LocalMultiaddr()
}

// LocalPeer is the Peer on our side of the connection
func (c *Conn) LocalPeer() peer.ID {
	return c.RawConn().LocalPeer()
}

// RemoteMultiaddr is the Multiaddr on the remote side
func (c *Conn) RemoteMultiaddr() ma.Multiaddr {
	return c.RawConn().RemoteMultiaddr()
}

// RemotePeer is the Peer on the remote side
func (c *Conn) RemotePeer() peer.ID {
	return c.RawConn().RemotePeer()
}

// LocalPrivateKey is the public key of the peer on this side
func (c *Conn) LocalPrivateKey() ic.PrivKey {
	return c.RawConn().LocalPrivateKey()
}

// RemotePublicKey is the public key of the peer on the remote side
func (c *Conn) RemotePublicKey() ic.PubKey {
	return c.RawConn().RemotePublicKey()
}

// NewSwarmStream returns a new Stream from this connection
func (c *Conn) NewSwarmStream() (*Stream, error) {
	s, err := c.StreamConn().NewStream()
	return wrapStream(s), err
}

// NewStream returns a new Stream from this connection
func (c *Conn) NewStream() (inet.Stream, error) {
	s, err := c.NewSwarmStream()
	return inet.Stream(s), err
}

func (c *Conn) Close() error {
	return c.StreamConn().Close()
}

func wrapConn(psc *ps.Conn) (*Conn, error) {
	// grab the underlying connection.
	if _, ok := psc.NetConn().(conn.Conn); !ok {
		// this should never happen. if we see it ocurring it means that we added
		// a Listener to the ps.Swarm that is NOT one of our net/conn.Listener.
		return nil, fmt.Errorf("swarm connHandler: invalid conn (not a conn.Conn): %s", psc)
	}
	return (*Conn)(psc), nil
}

// wrapConns returns a *Conn for all these ps.Conns
func wrapConns(conns1 []*ps.Conn) []*Conn {
	conns2 := make([]*Conn, len(conns1))
	for i, c1 := range conns1 {
		if c2, err := wrapConn(c1); err == nil {
			conns2[i] = c2
		}
	}
	return conns2
}

// newConnSetup does the swarm's "setup" for a connection. returns the underlying
// conn.Conn this method is used by both swarm.Dial and ps.Swarm connHandler
func (s *Swarm) newConnSetup(ctx context.Context, psConn *ps.Conn) (*Conn, error) {

	// wrap with a Conn
	sc, err := wrapConn(psConn)
	if err != nil {
		return nil, err
	}

	// if we have a public key, make sure we add it to our peerstore!
	// This is an important detail. Otherwise we must fetch the public
	// key from the DHT or some other system.
	if pk := sc.RemotePublicKey(); pk != nil {
		s.peers.AddPubKey(sc.RemotePeer(), pk)
	}

	// ok great! we can use it. add it to our group.

	// set the RemotePeer as a group on the conn. this lets us group
	// connections in the StreamSwarm by peer, and get a streams from
	// any available connection in the group (better multiconn):
	//   swarm.StreamSwarm().NewStreamWithGroup(remotePeer)
	psConn.AddGroup(sc.RemotePeer())

	return sc, nil
}
