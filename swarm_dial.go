package swarm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	logging "github.com/ipfs/go-log"
	addrutil "github.com/libp2p/go-addr-util"
	lgbl "github.com/libp2p/go-libp2p-loggables"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	transport "github.com/libp2p/go-libp2p-transport"
	ma "github.com/multiformats/go-multiaddr"
)

// Diagram of dial sync:
//
//   many callers of Dial()   synched w.  dials many addrs       results to callers
//  ----------------------\    dialsync    use earliest            /--------------
//  -----------------------\              |----------\           /----------------
//  ------------------------>------------<-------     >---------<-----------------
//  -----------------------|              \----x                 \----------------
//  ----------------------|                \-----x                \---------------
//                                         any may fail          if no addr at end
//                                                             retry dialAttempt x

var (
	// ErrDialBackoff is returned by the backoff code when a given peer has
	// been dialed too frequently
	ErrDialBackoff = errors.New("dial backoff")

	// ErrDialFailed is returned when connecting to a peer has ultimately failed
	ErrDialFailed = errors.New("dial attempt failed")

	// ErrDialToSelf is returned if we attempt to dial our own peer
	ErrDialToSelf = errors.New("dial to self attempted")

	// ErrNoTransport is returned when we don't know a transport for the
	// given multiaddr.
	ErrNoTransport = errors.New("no transport for protocol")
)

// DialAttempts governs how many times a goroutine will try to dial a given peer.
// Note: this is down to one, as we have _too many dials_ atm. To add back in,
// add loop back in Dial(.)
const DialAttempts = 1

// ConcurrentFdDials is the number of concurrent outbound dials over transports
// that consume file descriptors
const ConcurrentFdDials = 160

// DefaultPerPeerRateLimit is the number of concurrent outbound dials to make
// per peer
const DefaultPerPeerRateLimit = 8

// dialbackoff is a struct used to avoid over-dialing the same, dead peers.
// Whenever we totally time out on a peer (all three attempts), we add them
// to dialbackoff. Then, whenevers goroutines would _wait_ (dialsync), they
// check dialbackoff. If it's there, they don't wait and exit promptly with
// an error. (the single goroutine that is actually dialing continues to
// dial). If a dial is successful, the peer is removed from backoff.
// Example:
//
//  for {
//  	if ok, wait := dialsync.Lock(p); !ok {
//  		if backoff.Backoff(p) {
//  			return errDialFailed
//  		}
//  		<-wait
//  		continue
//  	}
//  	defer dialsync.Unlock(p)
//  	c, err := actuallyDial(p)
//  	if err != nil {
//  		dialbackoff.AddBackoff(p)
//  		continue
//  	}
//  	dialbackoff.Clear(p)
//  }
//

// DialBackoff is a type for tracking peer dial backoffs.
//
// * It's safe to use its zero value.
// * It's thread-safe.
// * It's *not* safe to move this type after using.
type DialBackoff struct {
	entries map[peer.ID]*backoffPeer
	lock    sync.RWMutex
}

type backoffPeer struct {
	tries int
	until time.Time
}

func (db *DialBackoff) init() {
	if db.entries == nil {
		db.entries = make(map[peer.ID]*backoffPeer)
	}
}

// Backoff returns whether the client should backoff from dialing
// peer p
func (db *DialBackoff) Backoff(p peer.ID) (backoff bool) {
	db.lock.Lock()
	defer db.lock.Unlock()
	db.init()
	bp, found := db.entries[p]
	if found && time.Now().Before(bp.until) {
		return true
	}

	return false
}

// BackoffBase is the base amount of time to backoff (default: 5s).
var BackoffBase = time.Second * 5

// BackoffCoef is the backoff coefficient (default: 1s).
var BackoffCoef = time.Second

// BackoffMax is the maximum backoff time (default: 5m).
var BackoffMax = time.Minute * 5

// AddBackoff lets other nodes know that we've entered backoff with
// peer p, so dialers should not wait unnecessarily. We still will
// attempt to dial with one goroutine, in case we get through.
//
// Backoff is not exponential, it's quadratic and computed according to the
// following formula:
//
//     BackoffBase + BakoffCoef * PriorBackoffs^2
//
// Where PriorBackoffs is the number of previous backoffs.
func (db *DialBackoff) AddBackoff(p peer.ID) {
	db.lock.Lock()
	defer db.lock.Unlock()
	db.init()
	bp, ok := db.entries[p]
	if !ok {
		db.entries[p] = &backoffPeer{
			tries: 1,
			until: time.Now().Add(BackoffBase),
		}
		return
	}

	backoffTime := BackoffBase + BackoffCoef*time.Duration(bp.tries*bp.tries)
	if backoffTime > BackoffMax {
		backoffTime = BackoffMax
	}
	bp.until = time.Now().Add(backoffTime)
	bp.tries++
}

// Clear removes a backoff record. Clients should call this after a
// successful Dial.
func (db *DialBackoff) Clear(p peer.ID) {
	db.lock.Lock()
	defer db.lock.Unlock()
	db.init()
	delete(db.entries, p)
}

// DialPeer connects to a peer.
//
// The idea is that the client of Swarm does not need to know what network
// the connection will happen over. Swarm can use whichever it choses.
// This allows us to use various transport protocols, do NAT traversal/relay,
// etc. to achieve connection.
func (s *Swarm) DialPeer(ctx context.Context, p peer.ID) (inet.Conn, error) {
	return s.dialPeer(ctx, p)
}

// internal dial method that returns an unwrapped conn
//
// It is gated by the swarm's dial synchronization systems: dialsync and
// dialbackoff.
func (s *Swarm) dialPeer(ctx context.Context, p peer.ID) (*Conn, error) {
	log.Debugf("[%s] swarm dialing peer [%s]", s.local, p)
	var logdial = lgbl.Dial("swarm", s.LocalPeer(), p, nil, nil)
	err := p.Validate()
	if err != nil {
		return nil, err
	}

	if p == s.local {
		log.Event(ctx, "swarmDialSelf", logdial)
		return nil, ErrDialToSelf
	}

	defer log.EventBegin(ctx, "swarmDialAttemptSync", p).Done()

	// check if we already have an open connection first
	conn := s.bestConnToPeerWrapper(p)
	if conn != nil {
		return conn, nil
	}

	// if this peer has been backed off, lets get out of here
	if s.backf.Backoff(p) {
		log.Event(ctx, "swarmDialBackoff", p)
		return nil, ErrDialBackoff
	}

	// apply the DialPeer timeout
	ctx, cancel := context.WithTimeout(ctx, inet.GetDialPeerTimeout(ctx))
	defer cancel()

	conn, err = s.dsync.DialLock(ctx, p)
	if err != nil {
		return nil, err
	}

	log.Debugf("network for %s finished dialing %s", s.local, p)
	return conn, err
}

// doDial is an ugly shim method to retain all the logging and backoff logic
// of the old dialsync code
func (s *Swarm) doDial(ctx context.Context, p peer.ID) (*Conn, error) {
	// Short circuit.
	// By the time we take the dial lock, we may already *have* a connection
	// to the peer.
	c := s.bestConnToPeerWrapper(p)
	if c != nil {
		return c, nil
	}

	logdial := lgbl.Dial("swarm", s.LocalPeer(), p, nil, nil)

	// ok, we have been charged to dial! let's do it.
	// if it succeeds, dial will add the conn to the swarm itself.
	defer log.EventBegin(ctx, "swarmDialAttemptStart", logdial).Done()

	conn, err := s.dial(ctx, p)
	if err != nil {
		conn = s.bestConnToPeerFallbackWrapper(p)
		if conn != nil {
			// Hm? What error?
			// Could have canceled the dial because we received a
			// connection or some other random reason.
			// Just ignore the error and return the connection.
			log.Debugf("ignoring dial error because we have a connection: %s", err)
			return conn, nil
		}
		if err != context.Canceled {
			log.Event(ctx, "swarmDialBackoffAdd", logdial)
			s.backf.AddBackoff(p) // let others know to backoff
		}

		// ok, we failed.
		return nil, fmt.Errorf("dial attempt failed: %s", err)
	}
	return conn, nil
}

func (s *Swarm) canDial(addr ma.Multiaddr) bool {
	t := s.TransportForDialing(addr)
	return t != nil && t.CanDial(addr)
}

// dial is the actual swarm's dial logic, gated by Dial.
func (s *Swarm) dial(ctx context.Context, p peer.ID) (*Conn, error) {
	var logdial = lgbl.Dial("swarm", s.LocalPeer(), p, nil, nil)
	if p == s.local {
		log.Event(ctx, "swarmDialDoDialSelf", logdial)
		return nil, ErrDialToSelf
	}
	defer log.EventBegin(ctx, "swarmDialDo", logdial).Done()
	logdial["dial"] = "failure" // start off with failure. set to "success" at the end.

	sk := s.peers.PrivKey(s.local)
	logdial["encrypted"] = sk != nil // log whether this will be an encrypted dial or not.
	if sk == nil {
		// fine for sk to be nil, just log.
		log.Debug("Dial not given PrivateKey, so WILL NOT SECURE conn.")
	}

	//////
	/*
		This slice-to-chan code is temporary, the peerstore can currently provide
		a channel as an interface for receiving addresses, but more thought
		needs to be put into the execution. For now, this allows us to use
		the improved rate limiter, while maintaining the outward behaviour
		that we previously had (halting a dial when we run out of addrs)
	*/
	peerAddrs := s.peers.Addrs(p)
	if len(peerAddrs) == 0 {
		return nil, errors.New("no addresses")
	}
	goodAddrs := s.filterKnownUndialables(peerAddrs)

	if len(goodAddrs) == 0 {
		return nil, errors.New("no good addresses")
	}

	if s.bestDest != nil {
		// Select the best address to peer.
		bestAddrs := s.bestDestSelectWrapper(p, goodAddrs)
		if len(bestAddrs) != 0 {
			goodAddrs = bestAddrs
		}
	}
	goodAddrsChan := make(chan ma.Multiaddr, len(goodAddrs))
	for _, a := range goodAddrs {
		goodAddrsChan <- a
	}
	close(goodAddrsChan)
	/////////

	// try to get a connection to any addr
	connC, err := s.dialAddrs(ctx, p, goodAddrsChan)
	if err != nil {
		logdial["error"] = err.Error()
		return nil, err
	}
	logdial["conn"] = logging.Metadata{
		"localAddr":  connC.LocalMultiaddr(),
		"remoteAddr": connC.RemoteMultiaddr(),
	}
	swarmC, err := s.addConn(connC, inet.DirOutbound)
	if err != nil {
		logdial["error"] = err.Error()
		connC.Close() // close the connection. didn't work out :(
		return nil, err
	}

	logdial["dial"] = "success"
	return swarmC, nil
}

// filterKnownUndialables takes a list of multiaddrs, and removes those
// that we definitely don't want to dial: addresses configured to be blocked,
// IPv6 link-local addresses, addresses without a dial-capable transport,
// and addresses that we know to be our own.
// This is an optimization to avoid wasting time on dials that we know are going to fail.
func (s *Swarm) filterKnownUndialables(addrs []ma.Multiaddr) []ma.Multiaddr {
	lisAddrs, _ := s.InterfaceListenAddresses()
	var ourAddrs []ma.Multiaddr
	for _, addr := range lisAddrs {
		protos := addr.Protocols()
		// we're only sure about filtering out /ip4 and /ip6 addresses, so far
		if len(protos) == 2 && (protos[0].Code == ma.P_IP4 || protos[0].Code == ma.P_IP6) {
			ourAddrs = append(ourAddrs, addr)
		}
	}

	return addrutil.FilterAddrs(addrs,
		addrutil.SubtractFilter(ourAddrs...),
		s.canDial,
		// TODO: Consider allowing link-local addresses
		addrutil.AddrOverNonLocalIP,
		addrutil.FilterNeg(s.Filters.AddrBlocked),
	)
}

func (s *Swarm) dialAddrs(ctx context.Context, p peer.ID, remoteAddrs <-chan ma.Multiaddr) (transport.Conn, error) {
	log.Debugf("%s swarm dialing %s", s.local, p)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // cancel work when we exit func

	// use a single response type instead of errs and conns, reduces complexity *a ton*
	respch := make(chan dialResult)

	defaultDialFail := inet.ErrNoRemoteAddrs
	exitErr := defaultDialFail

	defer s.limiter.clearAllPeerDials(p)

	var active int
	for remoteAddrs != nil || active > 0 {
		// Check for context cancellations and/or responses first.
		select {
		case <-ctx.Done():
			if exitErr == defaultDialFail {
				exitErr = ctx.Err()
			}
			return nil, exitErr
		case resp := <-respch:
			active--
			if resp.Err != nil {
				log.Infof("got error on dial to %s: %s", resp.Addr, resp.Err)
				// Errors are normal, lots of dials will fail
				exitErr = resp.Err
			} else if resp.Conn != nil {
				return resp.Conn, nil
			}

			// We got a result, try again from the top.
			continue
		default:
		}

		// Now, attempt to dial.
		select {
		case addr, ok := <-remoteAddrs:
			if !ok {
				remoteAddrs = nil
				continue
			}

			s.limitedDial(ctx, p, addr, respch)
			active++
		case <-ctx.Done():
			if exitErr == defaultDialFail {
				exitErr = ctx.Err()
			}
			return nil, exitErr
		case resp := <-respch:
			active--
			if resp.Err != nil {
				log.Infof("got error on dial to %s: %s", resp.Addr, resp.Err)
				// Errors are normal, lots of dials will fail
				exitErr = resp.Err
			} else if resp.Conn != nil {
				return resp.Conn, nil
			}
		}
	}
	return nil, exitErr
}

// limitedDial will start a dial to the given peer when
// it is able, respecting the various different types of rate
// limiting that occur without using extra goroutines per addr
func (s *Swarm) limitedDial(ctx context.Context, p peer.ID, a ma.Multiaddr, resp chan dialResult) {
	s.limiter.AddDialJob(&dialJob{
		addr: a,
		peer: p,
		resp: resp,
		ctx:  ctx,
	})
}

func (s *Swarm) dialAddr(ctx context.Context, p peer.ID, addr ma.Multiaddr) (transport.Conn, error) {
	// Just to double check. Costs nothing.
	if s.local == p {
		return nil, ErrDialToSelf
	}
	log.Debugf("%s swarm dialing %s %s", s.local, p, addr)

	tpt := s.TransportForDialing(addr)
	if tpt == nil {
		return nil, ErrNoTransport
	}

	connC, err := tpt.Dial(ctx, addr, p)
	if err != nil {
		return nil, fmt.Errorf("%s --> %s dial attempt failed: %s", s.local, p, err)
	}

	// Trust the transport? Yeah... right.
	if connC.RemotePeer() != p {
		connC.Close()
		err = fmt.Errorf("BUG in transport %T: tried to dial %s, dialed %s", p, connC.RemotePeer(), tpt)
		log.Error(err)
		return nil, err
	}

	// success! we got one!
	return connC, nil
}
