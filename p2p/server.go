// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package p2p implements the Ethereum p2p network protocols.
package p2p

import (
	"crypto/ecdsa"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/teamnsrg/go-ethereum/common"
	"github.com/teamnsrg/go-ethereum/common/mclock"
	"github.com/teamnsrg/go-ethereum/crypto"
	"github.com/teamnsrg/go-ethereum/event"
	"github.com/teamnsrg/go-ethereum/log"
	"github.com/teamnsrg/go-ethereum/p2p/discover"
	"github.com/teamnsrg/go-ethereum/p2p/discv5"
	"github.com/teamnsrg/go-ethereum/p2p/nat"
	"github.com/teamnsrg/go-ethereum/p2p/netutil"
)

const (
	defaultDialTimeout      = 15 * time.Second
	refreshPeersInterval    = 30 * time.Second
	staticPeerCheckInterval = 15 * time.Second

	// Maximum time allowed for reading a complete message.
	// This is effectively the amount of time a connection can be idle.
	frameReadTimeout = 30 * time.Second

	// Maximum amount of time allowed for writing a complete message.
	frameWriteTimeout = 20 * time.Second
)

var errServerStopped = errors.New("server stopped")

// Config holds Server options.
type Config struct {
	// MySQLName is the MySQL node database connection information
	MySQLName string

	// MaxDial is the maximum number of concurrently dialing outbound connections.
	MaxDial int

	// MaxDial is the maximum number of concurrently handshaking inbound connections.
	MaxAcceptConns int

	// NoMaxPeers ignores/overwrites MaxPeers, allowing unlimited number of peer connections.
	NoMaxPeers bool

	// Blacklist is the list of IP networks that we should not connect to
	Blacklist *netutil.Netlist `toml:",omitempty"`

	// This field must be set to a valid secp256k1 private key.
	PrivateKey *ecdsa.PrivateKey `toml:"-"`

	// MaxPeers is the maximum number of peers that can be
	// connected. It must be greater than zero.
	MaxPeers int

	// MaxPendingPeers is the maximum number of peers that can be pending in the
	// handshake phase, counted separately for inbound and outbound connections.
	// Zero defaults to preset values.
	MaxPendingPeers int `toml:",omitempty"`

	// NoDiscovery can be used to disable the peer discovery mechanism.
	// Disabling is useful for protocol debugging (manual topology).
	NoDiscovery bool

	// DiscoveryV5 specifies whether the the new topic-discovery based V5 discovery
	// protocol should be started or not.
	DiscoveryV5 bool `toml:",omitempty"`

	// Listener address for the V5 discovery protocol UDP traffic.
	DiscoveryV5Addr string `toml:",omitempty"`

	// Name sets the node name of this server.
	// Use common.MakeName to create a name that follows existing conventions.
	Name string `toml:"-"`

	// BootstrapNodes are used to establish connectivity
	// with the rest of the network.
	BootstrapNodes []*discover.Node

	// BootstrapNodesV5 are used to establish connectivity
	// with the rest of the network using the V5 discovery
	// protocol.
	BootstrapNodesV5 []*discv5.Node `toml:",omitempty"`

	// Static nodes are used as pre-configured connections which are always
	// maintained and re-connected on disconnects.
	StaticNodes []*discover.Node

	// Trusted nodes are used as pre-configured connections which are always
	// allowed to connect, even above the peer limit.
	TrustedNodes []*discover.Node

	// Connectivity can be restricted to certain IP networks.
	// If this option is set to a non-nil value, only hosts which match one of the
	// IP networks contained in the list are considered.
	NetRestrict *netutil.Netlist `toml:",omitempty"`

	// NodeDatabase is the path to the database containing the previously seen
	// live nodes in the network.
	NodeDatabase string `toml:",omitempty"`

	// Protocols should contain the protocols supported
	// by the server. Matching protocols are launched for
	// each peer.
	Protocols []Protocol `toml:"-"`

	// If ListenAddr is set to a non-nil address, the server
	// will listen for incoming connections.
	//
	// If the port is zero, the operating system will pick a port. The
	// ListenAddr field will be updated with the actual address when
	// the server is started.
	ListenAddr string

	// If set to a non-nil value, the given NAT port mapper
	// is used to make the listening port available to the
	// Internet.
	NAT nat.Interface `toml:",omitempty"`

	// If Dialer is set to a non-nil value, the given Dialer
	// is used to dial outbound peer connections.
	Dialer NodeDialer `toml:"-"`

	// If NoDial is true, the server will not dial any peers.
	NoDial bool `toml:",omitempty"`

	// If EnableMsgEvents is set then the server will emit PeerEvents
	// whenever a message is sent to or received from a peer
	EnableMsgEvents bool
}

// Server manages all peer connections.
type Server struct {
	addNodeInfoStmt     *sql.Stmt
	updateNodeInfoStmt  *sql.Stmt
	addNodeMetaInfoStmt *sql.Stmt
	KnownNodeInfos      map[discover.NodeID]*KnownNodeInfo // information on known nodes
	DB                  *sql.DB                            // MySQL database handle

	// Config fields may not be modified while the server is running.
	Config

	// Hooks for testing. These are useful because we can inhibit
	// the whole protocol stack.
	newTransport func(net.Conn) transport
	newPeerHook  func(*Peer)

	lock    sync.Mutex // protects running
	running bool

	ntab         discoverTable
	listener     net.Listener
	ourHandshake *protoHandshake
	lastLookup   time.Time
	DiscV5       *discv5.Network

	// These are for Peers, PeerCount (and nothing else).
	peerOp     chan peerOpFunc
	peerOpDone chan struct{}

	quit          chan struct{}
	addstatic     chan *discover.Node
	removestatic  chan *discover.Node
	posthandshake chan *conn
	addpeer       chan *conn
	delpeer       chan peerDrop
	loopWG        sync.WaitGroup // loop, listenLoop
	peerFeed      event.Feed
}

type peerOpFunc func(map[discover.NodeID]*Peer)

type peerDrop struct {
	*Peer
	err       error
	requested bool // true if signaled by the peer
}

type connFlag int

const (
	dynDialedConn connFlag = 1 << iota
	staticDialedConn
	inboundConn
	trustedConn
)

// conn wraps a network connection with information gathered
// during the two handshakes.
type conn struct {
	fd net.Conn
	transport
	flags connFlag
	cont  chan error      // The run loop uses cont to signal errors to SetupConn.
	id    discover.NodeID // valid after the encryption handshake
	caps  []Cap           // valid after the protocol handshake
	name  string          // valid after the protocol handshake
}

type transport interface {
	// The two handshakes.
	doEncHandshake(prv *ecdsa.PrivateKey, dialDest *discover.Node) (discover.NodeID, error)
	doProtoHandshake(our *protoHandshake, peer discover.NodeID) (*protoHandshake, *time.Time, error)
	// The MsgReadWriter can only be used after the encryption
	// handshake has completed. The code uses conn.id to track this
	// by setting it to a non-nil value after the encryption handshake.
	MsgReadWriter
	// transports must provide Close because we use MsgPipe in some of
	// the tests. Closing the actual network connection doesn't do
	// anything in those tests because NsgPipe doesn't use it.
	close(err error, peer discover.NodeID)
}

func (c *conn) String() string {
	s := c.flags.String()
	if (c.id != discover.NodeID{}) {
		s += " " + c.id.String()
	}
	s += " " + c.fd.RemoteAddr().String()
	return s
}

func (f connFlag) String() string {
	s := ""
	if f&trustedConn != 0 {
		s += "-trusted"
	}
	if f&dynDialedConn != 0 {
		s += "-dyndial"
	}
	if f&staticDialedConn != 0 {
		s += "-staticdial"
	}
	if f&inboundConn != 0 {
		s += "-inbound"
	}
	if s != "" {
		s = s[1:]
	}
	return s
}

func (c *conn) is(f connFlag) bool {
	return c.flags&f != 0
}

// Peers returns all connected peers.
func (srv *Server) Peers() []*Peer {
	var ps []*Peer
	select {
	// Note: We'd love to put this function into a variable but
	// that seems to cause a weird compiler error in some
	// environments.
	case srv.peerOp <- func(peers map[discover.NodeID]*Peer) {
		for _, p := range peers {
			ps = append(ps, p)
		}
	}:
		<-srv.peerOpDone
	case <-srv.quit:
	}
	return ps
}

// PeerCount returns the number of connected peers.
func (srv *Server) PeerCount() int {
	var count int
	select {
	case srv.peerOp <- func(ps map[discover.NodeID]*Peer) { count = len(ps) }:
		<-srv.peerOpDone
	case <-srv.quit:
	}
	return count
}

// AddPeer connects to the given node and maintains the connection until the
// server is shut down. If the connection fails for any reason, the server will
// attempt to reconnect the peer.
func (srv *Server) AddPeer(node *discover.Node) {
	select {
	case srv.addstatic <- node:
	case <-srv.quit:
	}
}

// RemovePeer disconnects from the given node
func (srv *Server) RemovePeer(node *discover.Node) {
	select {
	case srv.removestatic <- node:
	case <-srv.quit:
	}
}

// SubscribePeers subscribes the given channel to peer events
func (srv *Server) SubscribeEvents(ch chan *PeerEvent) event.Subscription {
	return srv.peerFeed.Subscribe(ch)
}

// Self returns the local node's endpoint information.
func (srv *Server) Self() *discover.Node {
	srv.lock.Lock()
	defer srv.lock.Unlock()

	if !srv.running {
		return &discover.Node{IP: net.ParseIP("0.0.0.0")}
	}
	return srv.makeSelf(srv.listener, srv.ntab)
}

func (srv *Server) makeSelf(listener net.Listener, ntab discoverTable) *discover.Node {
	// If the server's not running, return an empty node.
	// If the node is running but discovery is off, manually assemble the node infos.
	if ntab == nil {
		// Inbound connections disabled, use zero address.
		if listener == nil {
			return &discover.Node{IP: net.ParseIP("0.0.0.0"), ID: discover.PubkeyID(&srv.PrivateKey.PublicKey)}
		}
		// Otherwise inject the listener address too
		addr := listener.Addr().(*net.TCPAddr)
		return &discover.Node{
			ID:  discover.PubkeyID(&srv.PrivateKey.PublicKey),
			IP:  addr.IP,
			TCP: uint16(addr.Port),
		}
	}
	// Otherwise return the discovery node.
	return ntab.Self()
}

// Stop terminates the server and all active peer connections.
// It blocks until all active connections have been closed.
func (srv *Server) Stop() {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if !srv.running {
		return
	}
	srv.running = false
	if srv.listener != nil {
		// this unblocks listener Accept
		srv.listener.Close()
	}
	close(srv.quit)
	srv.loopWG.Wait()

	// close mysql db handle
	if srv.DB != nil {
		if srv.addNodeInfoStmt != nil {
			if err := srv.addNodeInfoStmt.Close(); err != nil {
				log.Proto("MYSQL", "action", "close AddNodeInfo statement", "result", "fail", "err", err)
			} else {
				log.Proto("MYSQL", "action", "close AddNodeInfo statement", "result", "success")
			}
		}
		if srv.updateNodeInfoStmt != nil {
			if err := srv.updateNodeInfoStmt.Close(); err != nil {
				log.Proto("MYSQL", "action", "close UpdateNodeInfo statement", "result", "fail", "err", err)
			} else {
				log.Proto("MYSQL", "action", "close UpdateNodeInfo statement", "result", "success")
			}
		}
		if srv.addNodeMetaInfoStmt != nil {
			if err := srv.addNodeMetaInfoStmt.Close(); err != nil {
				log.Proto("MYSQL", "action", "close AddNodeMetaInfo statement", "result", "fail", "err", err)
			} else {
				log.Proto("MYSQL", "action", "close AddNodeMetaInfo statement", "result", "success")
			}
		}
		driver := "mysql"
		if err := srv.DB.Close(); err != nil {
			log.Proto("MYSQL", "action", "close handle", "result", "fail", "database", srv.MySQLName, "driver", driver, "err", err)
		} else {
			log.Proto("MYSQL", "action", "close handle", "result", "success", "database", srv.MySQLName, "driver", driver)
		}
	}
}

// Start starts running the server.
// Servers can not be re-used after stopping.
func (srv *Server) Start() (err error) {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if srv.running {
		return errors.New("server already running")
	}

	// open mysql db handle
	if srv.MySQLName != "" {
		driver := "mysql"
		db, err := sql.Open(driver, srv.MySQLName)
		if err != nil {
			log.Proto("MYSQL", "action", "open handle", "result", "fail", "database", srv.MySQLName, "driver", driver, "err", err)
			return err
		}
		log.Proto("MYSQL", "action", "open handle", "result", "success", "database", srv.MySQLName, "driver", driver)
		err = db.Ping()
		if err != nil {
			log.Proto("MYSQL", "action", "ping test", "result", "fail", "database", srv.MySQLName, "driver", driver, "err", err)
			return err
		}
		log.Proto("MYSQL", "action", "ping test", "result", "success")
		srv.DB = db
	}

	srv.KnownNodeInfos = make(map[discover.NodeID]*KnownNodeInfo)

	if srv.DB != nil {
		// fill KnownNodesInfos with info from the mysql database
		srv.loadKnownNodeInfos()

		// prepare sql statements
		srv.prepareAddNodeInfoStmt()
		srv.prepareUpdateNodeInfoStmt()
		srv.prepareAddNodeMetaInfoStmt()
	}

	// TODO: load info from mysql db

	srv.running = true
	log.Info("Starting P2P networking")

	// static fields
	if srv.PrivateKey == nil {
		return fmt.Errorf("Server.PrivateKey must be set to a non-nil key")
	}
	if srv.newTransport == nil {
		srv.newTransport = newRLPX
	}
	if srv.Dialer == nil {
		srv.Dialer = TCPDialer{&net.Dialer{Timeout: defaultDialTimeout}}
	}
	srv.quit = make(chan struct{})
	srv.addpeer = make(chan *conn)
	srv.delpeer = make(chan peerDrop)
	srv.posthandshake = make(chan *conn)
	srv.addstatic = make(chan *discover.Node)
	srv.removestatic = make(chan *discover.Node)
	srv.peerOp = make(chan peerOpFunc)
	srv.peerOpDone = make(chan struct{})

	// node table
	if !srv.NoDiscovery {
		ntab, err := discover.ListenUDP(srv.PrivateKey, srv.ListenAddr, srv.NAT, srv.NodeDatabase, srv.NetRestrict, srv.Blacklist, srv.DB)
		if err != nil {
			return err
		}
		if err := ntab.SetFallbackNodes(srv.BootstrapNodes); err != nil {
			return err
		}
		srv.ntab = ntab
	}

	if srv.DiscoveryV5 {
		ntab, err := discv5.ListenUDP(srv.PrivateKey, srv.DiscoveryV5Addr, srv.NAT, "", srv.NetRestrict) //srv.NodeDatabase)
		if err != nil {
			return err
		}
		if err := ntab.SetFallbackNodes(srv.BootstrapNodesV5); err != nil {
			return err
		}
		srv.DiscV5 = ntab
	}

	// TODO: determine whether srv.MaxPeers/2 is necessary
	// use srv.MaxDial for now
	// dynPeers := (srv.MaxPeers + 1) / 2

	dynPeers := srv.MaxDial

	if srv.NoDiscovery {
		dynPeers = 0
	}
	dialer := newDialState(srv.StaticNodes, srv.BootstrapNodes, srv.ntab, dynPeers, srv.NetRestrict, srv.Blacklist)

	// handshake
	srv.ourHandshake = &protoHandshake{Version: baseProtocolVersion, Name: srv.Name, ID: discover.PubkeyID(&srv.PrivateKey.PublicKey)}
	for _, p := range srv.Protocols {
		srv.ourHandshake.Caps = append(srv.ourHandshake.Caps, p.cap())
	}
	// listen/dial
	if srv.ListenAddr != "" {
		if err := srv.startListening(); err != nil {
			return err
		}
	}
	if srv.NoDial && srv.ListenAddr == "" {
		log.Warn("P2P server will be useless, neither dialing nor listening")
	}

	srv.loopWG.Add(1)
	go srv.run(dialer)
	srv.running = true
	return nil
}

func (srv *Server) startListening() error {
	// Launch the TCP listener.
	listener, err := net.Listen("tcp", srv.ListenAddr)
	if err != nil {
		return err
	}
	laddr := listener.Addr().(*net.TCPAddr)
	srv.ListenAddr = laddr.String()
	srv.listener = listener
	srv.loopWG.Add(1)
	go srv.listenLoop()
	// Map the TCP listening port if NAT is configured.
	if !laddr.IP.IsLoopback() && srv.NAT != nil {
		srv.loopWG.Add(1)
		go func() {
			nat.Map(srv.NAT, srv.quit, "tcp", laddr.Port, laddr.Port, "ethereum p2p")
			srv.loopWG.Done()
		}()
	}
	return nil
}

type dialer interface {
	newTasks(running int, peers map[discover.NodeID]*Peer, now time.Time) []task
	taskDone(task, time.Time)
	addStatic(*discover.Node)
	removeStatic(*discover.Node)
}

func (srv *Server) run(dialstate dialer) {
	defer srv.loopWG.Done()
	var (
		peers        = make(map[discover.NodeID]*Peer)
		trusted      = make(map[discover.NodeID]bool, len(srv.TrustedNodes))
		taskdone     = make(chan task, srv.MaxDial)
		runningTasks []task
		queuedTasks  []task // tasks that can't run yet
	)
	// Put trusted nodes into a map to speed up checks.
	// Trusted peers are loaded on startup and cannot be
	// modified while the server is running.
	for _, n := range srv.TrustedNodes {
		trusted[n.ID] = true
	}

	// removes t from runningTasks
	delTask := func(t task) {
		for i := range runningTasks {
			if runningTasks[i] == t {
				runningTasks = append(runningTasks[:i], runningTasks[i+1:]...)
				break
			}
		}
	}
	// starts until max number of active tasks is satisfied
	startTasks := func(ts []task) (rest []task) {
		i := 0
		for ; len(runningTasks) < srv.MaxDial && i < len(ts); i++ {
			t := ts[i]
			log.Trace("New dial task", "task", t)
			go func() { t.Do(srv); taskdone <- t }()
			runningTasks = append(runningTasks, t)
		}
		return ts[i:]
	}
	scheduleTasks := func() {
		// Start from queue first.
		queuedTasks = append(queuedTasks[:0], startTasks(queuedTasks)...)
		// Query dialer for new tasks and start as many as possible now.
		if len(runningTasks) < srv.MaxDial {
			nt := dialstate.newTasks(len(runningTasks)+len(queuedTasks), peers, time.Now())
			queuedTasks = append(queuedTasks, startTasks(nt)...)
		}
	}

running:
	for {
		scheduleTasks()

		select {
		case <-srv.quit:
			// The server was stopped. Run the cleanup logic.
			break running
		case n := <-srv.addstatic:
			// This channel is used by AddPeer to add to the
			// ephemeral static peer list. Add it to the dialer,
			// it will keep the node connected.
			log.Debug("Adding static node", "node", n)
			dialstate.addStatic(n)
		case n := <-srv.removestatic:
			// This channel is used by RemovePeer to send a
			// disconnect request to a peer and begin the
			// stop keeping the node connected
			log.Debug("Removing static node", "node", n)
			dialstate.removeStatic(n)
			if p, ok := peers[n.ID]; ok {
				p.Disconnect(DiscRequested)
			}
		case op := <-srv.peerOp:
			// This channel is used by Peers and PeerCount.
			op(peers)
			srv.peerOpDone <- struct{}{}
		case t := <-taskdone:
			// A task got done. Tell dialstate about it so it
			// can update its state and remove it from the active
			// tasks list.
			log.Trace("Dial task done", "task", t)
			dialstate.taskDone(t, time.Now())
			delTask(t)
		case c := <-srv.posthandshake:
			// A connection has passed the encryption handshake so
			// the remote identity is known (but hasn't been verified yet).
			if trusted[c.id] {
				// Ensure that the trusted flag is set before checking against MaxPeers.
				c.flags |= trustedConn
			}
			// TODO: track in-progress inbound node IDs (pre-Peer) to avoid dialing them.
			select {
			case c.cont <- srv.encHandshakeChecks(peers, c):
			case <-srv.quit:
				break running
			}
		case c := <-srv.addpeer:
			// At this point the connection is past the protocol handshake.
			// Its capabilities are known and the remote identity is verified.
			err := srv.protoHandshakeChecks(peers, c)
			if err == nil {
				// The handshakes are done and it passed all checks.
				p := newPeer(c, srv.Protocols)
				// If message events are enabled, pass the peerFeed
				// to the peer
				if srv.EnableMsgEvents {
					p.events = &srv.peerFeed
				}
				name := truncateName(c.name)
				log.Proto("Adding p2p peer", "id", c.id, "name", name, "addr", c.fd.RemoteAddr(), "peers", len(peers)+1)
				peers[c.id] = p
				go srv.runPeer(p)
			}
			// The dialer logic relies on the assumption that
			// dial tasks complete after the peer has been added or
			// discarded. Unblock the task last.
			select {
			case c.cont <- err:
			case <-srv.quit:
				break running
			}
		case pd := <-srv.delpeer:
			// A peer disconnected.
			d := common.PrettyDuration(mclock.Now() - pd.created)
			pd.log.Proto("Removing p2p peer", "duration", d, "peers", len(peers)-1, "req", pd.requested, "err", pd.err)
			delete(peers, pd.ID())
		}
	}

	log.Trace("P2P networking is spinning down")

	// Terminate discovery. If there is a running lookup it will terminate soon.
	if srv.ntab != nil {
		srv.ntab.Close()
	}
	if srv.DiscV5 != nil {
		srv.DiscV5.Close()
	}
	// Disconnect all peers.
	for _, p := range peers {
		p.Disconnect(DiscQuitting)
	}
	// Wait for peers to shut down. Pending connections and tasks are
	// not handled here and will terminate soon-ish because srv.quit
	// is closed.
	for len(peers) > 0 {
		p := <-srv.delpeer
		p.log.Trace("<-delpeer (spindown)", "remainingTasks", len(runningTasks))
		delete(peers, p.ID())
	}
}

func (srv *Server) protoHandshakeChecks(peers map[discover.NodeID]*Peer, c *conn) error {
	// Drop connections with no matching protocols.
	if len(srv.Protocols) > 0 && countMatchingProtocols(srv.Protocols, c.caps) == 0 {
		return DiscUselessPeer
	}
	// Repeat the encryption handshake checks because the
	// peer set might have changed between the handshakes.
	return srv.encHandshakeChecks(peers, c)
}

func (srv *Server) encHandshakeChecks(peers map[discover.NodeID]*Peer, c *conn) error {
	switch {
	case !c.is(trustedConn|staticDialedConn) && !srv.NoMaxPeers && len(peers) >= srv.MaxPeers:
		return DiscTooManyPeers
	case peers[c.id] != nil:
		return DiscAlreadyConnected
	case c.id == srv.Self().ID:
		return DiscSelf
	default:
		return nil
	}
}

type tempError interface {
	Temporary() bool
}

// listenLoop runs in its own goroutine and accepts
// inbound connections.
func (srv *Server) listenLoop() {
	defer srv.loopWG.Done()
	log.Info("RLPx listener up", "self", srv.makeSelf(srv.listener, srv.ntab))

	// This channel acts as a semaphore limiting
	// active inbound connections that are lingering pre-handshake.
	// If all slots are taken, no further connections are accepted.
	tokens := srv.MaxAcceptConns
	if srv.MaxPendingPeers > 0 {
		tokens = srv.MaxPendingPeers
	}
	slots := make(chan struct{}, tokens)
	for i := 0; i < tokens; i++ {
		slots <- struct{}{}
	}

	for {
		// Wait for a handshake slot before accepting.
		<-slots

		var (
			fd  net.Conn
			err error
		)
		for {
			fd, err = srv.listener.Accept()
			if tempErr, ok := err.(tempError); ok && tempErr.Temporary() {
				log.Debug("Temporary read error", "err", err)
				continue
			} else if err != nil {
				log.Debug("Read error", "err", err)
				return
			}
			break
		}

		// Reject connections that do not match NetRestrict.
		if srv.NetRestrict != nil {
			if tcp, ok := fd.RemoteAddr().(*net.TCPAddr); ok && !srv.NetRestrict.Contains(tcp.IP) {
				log.Debug("Rejected conn (not whitelisted in NetRestrict)", "addr", fd.RemoteAddr())
				fd.Close()
				slots <- struct{}{}
				continue
			}
		}

		// Reject connections that match Blacklist.
		if srv.Blacklist != nil {
			if tcp, ok := fd.RemoteAddr().(*net.TCPAddr); ok && srv.Blacklist.Contains(tcp.IP) {
				log.Proto("BLACKLIST", "addr", fd.RemoteAddr().(*net.TCPAddr).IP.String(), "transport", "tcp")
				fd.Close()
				slots <- struct{}{}
				continue
			}
		}

		fd = newMeteredConn(fd, true)
		log.Trace("Accepted connection", "addr", fd.RemoteAddr())

		// Spawn the handler. It will give the slot back when the connection
		// has been established.
		go func() {
			srv.SetupConn(fd, inboundConn, nil)
			slots <- struct{}{}
		}()
	}
}

// SetupConn runs the handshakes and attempts to add the connection
// as a peer. It returns when the connection has been added as a peer
// or the handshakes have failed.
func (srv *Server) SetupConn(fd net.Conn, flags connFlag, dialDest *discover.Node) {
	// Prevent leftover pending conns from entering the handshake.
	srv.lock.Lock()
	running := srv.running
	srv.lock.Unlock()
	c := &conn{fd: fd, transport: srv.newTransport(fd), flags: flags, cont: make(chan error)}
	if !running {
		c.close(errServerStopped, discover.NodeID{})
		return
	}
	// Run the encryption handshake.
	var err error
	if c.id, err = c.doEncHandshake(srv.PrivateKey, dialDest); err != nil {
		log.Trace("Failed RLPx handshake", "addr", c.fd.RemoteAddr(), "conn", c.flags, "err", err)
		c.close(err, c.id)
		return
	}
	// For dialed connections, check that the remote public key matches.
	clog := log.New("id", c.id, "addr", c.fd.RemoteAddr(), "conn", c.flags)
	if dialDest != nil && c.id != dialDest.ID {
		c.close(DiscUnexpectedIdentity, c.id)
		clog.Trace("Dialed identity mismatch", "want", c, dialDest.ID)
		return
	}
	if err := srv.checkpoint(c, srv.posthandshake); err != nil {
		clog.Trace("Rejected peer before protocol handshake", "err", err)
		c.close(err, c.id)
		return
	}
	// Run the protocol handshake
	phs, receivedAt, err := c.doProtoHandshake(srv.ourHandshake, c.id)
	if err != nil {
		clog.Trace("Failed proto handshake", "err", err)
		if srv.addNodeMetaInfoStmt != nil {
			if r, ok := err.(DiscReason); ok && r == DiscTooManyPeers {
				nodeInfo, dial, accept := srv.getNodeAddress(c, receivedAt)
				nodeid := c.id.String()
				srv.addNodeMetaInfo(nodeid, nodeInfo.Keccak256Hash, dial, accept, true)
			}
		}
		c.close(err, c.id)
		return
	}
	if phs.ID != c.id {
		clog.Trace("Wrong devp2p handshake identity", "err", phs.ID)
		c.close(DiscUnexpectedIdentity, c.id)
		return
	}

	// if sql database handle is available, update node information
	if srv.DB != nil {
		srv.storeNodeInfo(c, receivedAt, phs)
	}

	c.caps, c.name = phs.Caps, phs.Name
	if err := srv.checkpoint(c, srv.addpeer); err != nil {
		clog.Trace("Rejected peer", "err", err)
		c.close(err, c.id)
		return
	}
	// If the checks completed successfully, runPeer has now been
	// launched by run.
}

func truncateName(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}

// checkpoint sends the conn to run, which performs the
// post-handshake checks for the stage (posthandshake, addpeer).
func (srv *Server) checkpoint(c *conn, stage chan<- *conn) error {
	select {
	case stage <- c:
	case <-srv.quit:
		return errServerStopped
	}
	select {
	case err := <-c.cont:
		return err
	case <-srv.quit:
		return errServerStopped
	}
}

// runPeer runs in its own goroutine for each peer.
// it waits until the Peer logic returns and removes
// the peer.
func (srv *Server) runPeer(p *Peer) {
	if srv.newPeerHook != nil {
		srv.newPeerHook(p)
	}

	// broadcast peer add
	srv.peerFeed.Send(&PeerEvent{
		Type: PeerEventTypeAdd,
		Peer: p.ID(),
	})

	// run the protocol
	remoteRequested, err := p.run()

	// broadcast peer drop
	srv.peerFeed.Send(&PeerEvent{
		Type:  PeerEventTypeDrop,
		Peer:  p.ID(),
		Error: err.Error(),
	})

	// Note: run waits for existing peers to be sent on srv.delpeer
	// before returning, so this send should not select on srv.quit.
	srv.delpeer <- peerDrop{p, err, remoteRequested}
}

// NodeInfo represents a short summary of the information known about the host.
type NodeInfo struct {
	ID    string `json:"id"`    // Unique node identifier (also the encryption key)
	Name  string `json:"name"`  // Name of the node, including client type, version, OS, custom data
	Enode string `json:"enode"` // Enode URL for adding this peer from remote peers
	IP    string `json:"ip"`    // IP address of the node
	Ports struct {
		Discovery int `json:"discovery"` // UDP listening port for discovery protocol
		Listener  int `json:"listener"`  // TCP listening port for RLPx
	} `json:"ports"`
	ListenAddr string                 `json:"listenAddr"`
	Protocols  map[string]interface{} `json:"protocols"`
}

// NodeInfo gathers and returns a collection of metadata known about the host.
func (srv *Server) NodeInfo() *NodeInfo {
	node := srv.Self()

	// Gather and assemble the generic node infos
	info := &NodeInfo{
		Name:       srv.Name,
		Enode:      node.String(),
		ID:         node.ID.String(),
		IP:         node.IP.String(),
		ListenAddr: srv.ListenAddr,
		Protocols:  make(map[string]interface{}),
	}
	info.Ports.Discovery = int(node.UDP)
	info.Ports.Listener = int(node.TCP)

	// Gather all the running protocol infos (only once per protocol type)
	for _, proto := range srv.Protocols {
		if _, ok := info.Protocols[proto.Name]; !ok {
			nodeInfo := interface{}("unknown")
			if query := proto.NodeInfo; query != nil {
				nodeInfo = proto.NodeInfo()
			}
			info.Protocols[proto.Name] = nodeInfo
		}
	}
	return info
}

func (srv *Server) loadKnownNodeInfos() {
	fields := "ni.node_id, nmi.hash, ip, tcp_port, remote_port, " +
		"p2p_version, client_id, caps, listen_port, last_hello_at, " +
		"protocol_version, network_id, first_received_td, last_received_td, best_hash, genesis_hash, dao_fork"
	maxIds := "SELECT node_id as nid, MAX(id) as max_id FROM node_info GROUP BY node_id"
	nodeInfos := fmt.Sprintf("SELECT * FROM node_info x INNER JOIN (%s) max_ids ON x.id = max_ids.max_id", maxIds)
	stmt := fmt.Sprintf("SELECT %s FROM (%s) ni INNER JOIN node_meta_info nmi ON ni.node_id=nmi.node_id", fields, nodeInfos)
	rows, _ := srv.DB.Query(stmt)

	type sqlObjects struct {
		p2pVersion      sql.NullInt64
		clientId        sql.NullString
		caps            sql.NullString
		listenPort      sql.NullInt64
		lastHelloAt     sql.NullFloat64
		protocolVersion sql.NullInt64
		networkId       sql.NullInt64
		firstReceivedTd sql.NullString
		lastReceivedTd  sql.NullString
		bestHash        sql.NullString
		genesisHash     sql.NullString
		daoForkSupport  sql.NullInt64
	}

	for rows.Next() {
		var (
			nodeid     string
			hash       string
			ip         string
			tcpPort    uint16
			remotePort uint16
			sqlObj     sqlObjects
		)
		err := rows.Scan(&nodeid, &hash, &ip, &tcpPort, &remotePort,
			&sqlObj.p2pVersion, &sqlObj.clientId, &sqlObj.caps, &sqlObj.listenPort, &sqlObj.lastHelloAt,
			&sqlObj.protocolVersion, &sqlObj.networkId, &sqlObj.firstReceivedTd, &sqlObj.lastReceivedTd, &sqlObj.bestHash, &sqlObj.genesisHash, &sqlObj.daoForkSupport)
		if err != nil {
			log.Proto("MYSQL", "action", "query node info", "result", "fail", "err", err)
			continue
		}
		// convert hex to NodeID
		id, err := discover.HexID(nodeid)
		if err != nil {
			log.Proto("LOAD_FROM_MYSQL", "action", "parse node_id", "result", "fail", "err", err)
			continue
		}
		nodeInfo := &KnownNodeInfo{
			Keccak256Hash: hash,
			IP:            ip,
			TCPPort:       tcpPort,
			RemotePort:    remotePort,
		}
		if sqlObj.p2pVersion.Valid {
			nodeInfo.P2PVersion = uint64(sqlObj.p2pVersion.Int64)
		}
		if sqlObj.clientId.Valid {
			nodeInfo.ClientId = sqlObj.clientId.String
		}
		if sqlObj.caps.Valid {
			nodeInfo.Caps = sqlObj.caps.String
		}
		if sqlObj.listenPort.Valid {
			nodeInfo.ListenPort = uint16(sqlObj.listenPort.Int64)
		}
		if sqlObj.lastHelloAt.Valid {
			i, f := math.Modf(sqlObj.lastHelloAt.Float64)
			t := time.Unix(int64(i), int64(f*1000000000))
			nodeInfo.LastConnectedAt = &t
		}
		if sqlObj.protocolVersion.Valid {
			nodeInfo.ProtocolVersion = uint64(sqlObj.protocolVersion.Int64)
		}
		if sqlObj.networkId.Valid {
			nodeInfo.NetworkId = uint64(sqlObj.networkId.Int64)
		}
		if sqlObj.firstReceivedTd.Valid {
			firstReceivedTd := &big.Int{}
			s := sqlObj.firstReceivedTd.String
			_, ok := firstReceivedTd.SetString(s, 10)
			if !ok {
				log.Proto("LOAD_FROM_MYSQL", "action", "parse *big.Int first_received_td", "result", "fail", "value", s)
			} else {
				nodeInfo.FirstReceivedTd = firstReceivedTd
			}
		}
		if sqlObj.lastReceivedTd.Valid {
			lastReceivedTd := &big.Int{}
			s := sqlObj.lastReceivedTd.String
			_, ok := lastReceivedTd.SetString(s, 10)
			if !ok {
				log.Proto("LOAD_FROM_MYSQL", "action", "parse *big.Int last_received_td", "result", "fail", "value", s)
			} else {
				nodeInfo.LastReceivedTd = lastReceivedTd
			}
		}
		if sqlObj.bestHash.Valid {
			nodeInfo.BestHash = sqlObj.bestHash.String
		}
		if sqlObj.genesisHash.Valid {
			nodeInfo.GenesisHash = sqlObj.genesisHash.String
		}
		if sqlObj.daoForkSupport.Valid {
			var daoForkSupport bool
			if uint16(sqlObj.daoForkSupport.Int64) != 0 {
				daoForkSupport = true
			}
			nodeInfo.DAOForkSupport = daoForkSupport
		}
		srv.KnownNodeInfos[id] = nodeInfo
	}
}

func (srv *Server) getNodeAddress(c *conn, receivedAt *time.Time) (*KnownNodeInfo, bool, bool) {
	var (
		remoteIP   string
		remotePort uint16
		tcpPort    uint16
		dial       bool
		accept     bool
	)
	addrArr := strings.Split(c.fd.RemoteAddr().String(), ":")
	addrLen := len(addrArr)
	remoteIP = strings.Join(addrArr[:addrLen-1], ":")
	if p, err := strconv.ParseUint(addrArr[addrLen-1], 10, 16); err == nil {
		remotePort = uint16(p)
	}
	// if inbound connection, resolve the node's listening port
	// otherwise, remotePort is the listening port
	if c.flags&inboundConn != 0 || c.flags&trustedConn != 0 {
		newNode := srv.ntab.Resolve(c.id)
		// if the node address is resolved, set the tcpPort
		// otherwise, leave it as 0
		if newNode != nil {
			tcpPort = newNode.TCP
		}
		accept = true
	} else {
		tcpPort = remotePort
		dial = true
	}
	oldNodeInfo := srv.KnownNodeInfos[c.id]
	var hash string
	if oldNodeInfo != nil {
		hash = oldNodeInfo.Keccak256Hash
	} else {
		hash = crypto.Keccak256Hash(c.id[:]).String()[2:]
	}
	newNodeInfo := &KnownNodeInfo{
		Keccak256Hash:   hash,
		LastConnectedAt: receivedAt,
		IP:              remoteIP,
		TCPPort:         tcpPort,
		RemotePort:      remotePort,
	}
	return newNodeInfo, dial, accept
}

func (srv *Server) storeNodeInfo(c *conn, receivedAt *time.Time, hs *protoHandshake) {
	// node address currentInfo
	newInfo, dial, accept := srv.getNodeAddress(c, receivedAt)
	id := hs.ID
	nodeid := id.String()
	if srv.addNodeMetaInfoStmt != nil {
		srv.addNodeMetaInfo(nodeid, newInfo.Keccak256Hash, dial, accept, false)
	}

	// DEVp2p Hello
	p2pVersion, clientId, capsArray, listenPort := hs.Version, hs.Name, hs.Caps, uint16(hs.ListenPort)
	caps := ""
	capsLen := len(capsArray)
	for i, c := range capsArray {
		caps += fmt.Sprintf("%s", c.String())
		if i < capsLen-1 {
			caps += ","
		}
	}
	clientId = strings.Replace(clientId, "'", "", -1)
	clientId = strings.Replace(clientId, "\"", "", -1)
	caps = strings.Replace(caps, "'", "", -1)
	caps = strings.Replace(caps, "\"", "", -1)

	newInfo.P2PVersion = p2pVersion
	newInfo.ClientId = clientId
	newInfo.Caps = caps
	newInfo.ListenPort = listenPort

	if currentInfo, ok := srv.KnownNodeInfos[id]; !ok {
		srv.KnownNodeInfos[id] = newInfo
		if srv.addNodeInfoStmt != nil {
			srv.addNodeInfo(nodeid, newInfo)
		}
	} else {
		currentInfo.LastConnectedAt = receivedAt
		currentInfo.RemotePort = newInfo.RemotePort
		if infoChanged(currentInfo, newInfo) {
			currentInfo.IP = newInfo.IP
			currentInfo.TCPPort = newInfo.TCPPort
			currentInfo.P2PVersion = p2pVersion
			currentInfo.ClientId = clientId
			currentInfo.Caps = caps
			currentInfo.ListenPort = listenPort
			if srv.addNodeInfoStmt != nil {
				// TODO: check logic
				// in-memory entry should keep the Ethereum Status info
				// new entry to the mysql db should contain only the new address, DEVp2p info
				// let Ethereum protocol update the Status info, if available.
				srv.addNodeInfo(nodeid, newInfo)
			}
		} else {
			if srv.updateNodeInfoStmt != nil {
				srv.updateNodeInfo(nodeid, newInfo)
			}
		}
	}
}

func infoChanged(oldInfo *KnownNodeInfo, newInfo *KnownNodeInfo) bool {
	return oldInfo.IP != newInfo.IP || oldInfo.TCPPort != newInfo.TCPPort || oldInfo.P2PVersion != newInfo.P2PVersion ||
		oldInfo.ClientId != newInfo.ClientId || oldInfo.Caps != newInfo.Caps || oldInfo.ListenPort != newInfo.ListenPort
}

func (srv *Server) prepareAddNodeInfoStmt() {
	fields := []string{"node_id", "ip", "tcp_port", "remote_port", "p2p_version", "client_id", "caps", "listen_port",
		"first_hello_at", "last_hello_at"}

	stmt := fmt.Sprintf(`INSERT INTO node_info (%s) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.Join(fields, ", "))
	pStmt, err := srv.DB.Prepare(stmt)
	if err != nil {
		log.Proto("MYSQL", "action", "prepare AddNodeInfo statement", "result", "fail", "err", err)
	} else {
		log.Proto("MYSQL", "action", "prepare AddNodeInfo statement", "result", "success")
		srv.addNodeInfoStmt = pStmt
	}
}

func (srv *Server) prepareUpdateNodeInfoStmt() {
	maxIdQuery := "SELECT max_id FROM (SELECT MAX(id) as max_id FROM node_info n WHERE n.node_id=?) tmp"
	stmt := fmt.Sprintf("UPDATE node_info SET remote_port=?, last_hello_at=? WHERE id=(%s)", maxIdQuery)
	pStmt, err := srv.DB.Prepare(stmt)

	if err != nil {
		log.Proto("MYSQL", "action", "prepare UpdateNodeInfo statement", "result", "fail", "err", err)
	} else {
		log.Proto("MYSQL", "action", "prepare UpdateNodeInfo statement", "result", "success")
		srv.updateNodeInfoStmt = pStmt
	}
}

func (srv *Server) prepareAddNodeMetaInfoStmt() {
	var updateFields []string
	fields := []string{"node_id", "hash", "dial_count", "accept_count", "too_many_peers_count"}
	for _, f := range fields[2:] {
		updateFields = append(updateFields, fmt.Sprintf("%s=%s+VALUES(%s)", f, f, f))
	}
	stmt := fmt.Sprintf(`INSERT INTO node_meta_info (%s) VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE %s`,
		strings.Join(fields, ", "), strings.Join(updateFields, ", "))
	pStmt, err := srv.DB.Prepare(stmt)
	if err != nil {
		log.Proto("MYSQL", "action", "prepare AddNodeMetaInfo statement", "result", "fail", "err", err)
	} else {
		log.Proto("MYSQL", "action", "prepare AddNodeMetaInfo statement", "result", "success")
		srv.addNodeMetaInfoStmt = pStmt
	}
}

func (srv *Server) addNodeInfo(nodeid string, newInfo *KnownNodeInfo) {
	unixTime := float64(newInfo.LastConnectedAt.UnixNano()) / 1000000000
	_, err := srv.addNodeInfoStmt.Exec(nodeid, newInfo.IP, newInfo.TCPPort, newInfo.RemotePort,
		newInfo.P2PVersion, newInfo.ClientId, newInfo.Caps, newInfo.ListenPort, unixTime, unixTime)
	if err != nil {
		log.Proto("MYSQL", "action", "execute AddNodeInfo statement", "result", "fail", "err", err)
	} else {
		log.Proto("MYSQL", "action", "execute AddNodeInfo statement", "result", "success")
	}
}

func (srv *Server) updateNodeInfo(nodeid string, newInfo *KnownNodeInfo) {
	unixTime := float64(newInfo.LastConnectedAt.UnixNano()) / 1000000000
	_, err := srv.updateNodeInfoStmt.Exec(newInfo.RemotePort, unixTime, nodeid)
	if err != nil {
		log.Proto("MYSQL", "action", "execute UpdateNodeInfo statement", "result", "fail", "err", err)
	} else {
		log.Proto("MYSQL", "action", "execute UpdateNodeInfo statement", "result", "success")
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (srv *Server) addNodeMetaInfo(nodeid string, hash string, dial bool, accept bool, tooManyPeers bool) {
	_, err := srv.addNodeMetaInfoStmt.Exec(nodeid, hash, boolToInt(dial), boolToInt(accept), boolToInt(tooManyPeers))
	if err != nil {
		log.Proto("MYSQL", "action", "execute AddNodeMetaInfo statement", "result", "fail", "err", err)
	} else {
		log.Proto("MYSQL", "action", "execute AddNodeMetaInfo statement", "result", "success")
	}
}

// PeersInfo returns an array of metadata objects describing connected peers.
func (srv *Server) PeersInfo() []*PeerInfo {
	// Gather all the generic and sub-protocol specific infos
	infos := make([]*PeerInfo, 0, srv.PeerCount())
	for _, peer := range srv.Peers() {
		if peer != nil {
			infos = append(infos, peer.Info())
		}
	}
	// Sort the result array alphabetically by node identifier
	for i := 0; i < len(infos); i++ {
		for j := i + 1; j < len(infos); j++ {
			if infos[i].ID > infos[j].ID {
				infos[i], infos[j] = infos[j], infos[i]
			}
		}
	}
	return infos
}

// KnownNodeInfo represents a short summary of the information known about a known DEVp2p node.
type KnownNodeInfo struct {
	Keccak256Hash   string     `json:"keccak256Hash"`             // Keccak256 hash of node ID
	LastConnectedAt *time.Time `json:"lastConnectedAt,omitempty"` // Last time the node was connected
	IP              string     `json:"ip"`                        // IP address of the node
	TCPPort         uint16     `json:"tcpPort"`                   // TCP listening port for RLPx
	RemotePort      uint16     `json:"tcpPort"`                   // Remote TCP port of the most recent connection

	// DEVp2p Hello info
	P2PVersion uint64 `json:"p2pVersion,omitempty"` // DEVp2p protocol version
	ClientId   string `json:"clientId,omitempty"`   // Name of the node, including client type, version, OS, custom data
	Caps       string `json:"caps,omitempty"`       // Node's capabilities
	ListenPort uint16 `json:"listenPort,omitempty"` // Listening port reported in the node's DEVp2p Hello

	// Ethereum Status info
	ProtocolVersion uint64   `json:"protocolVersion,omitempty"` // Ethereum sub-protocol version
	NetworkId       uint64   `json:"networkId,omitempty"`       // Ethereum network ID
	FirstReceivedTd *big.Int `json:"firstReceivedTd,omitempty"` // First reported total difficulty of the node's blockchain
	LastReceivedTd  *big.Int `json:"lastReceivedTd,omitempty"`  // Last reported total difficulty of the node's blockchain
	BestHash        string   `json:"bestHash,omitempty"`        // Hex string of SHA3 hash of the node's best owned block
	GenesisHash     string   `json:"genesisHash,omitempty"`     // Hex string of SHA3 hash of the node's genesis block
	DAOForkSupport  bool     `json:"daoForkSupport"`            // Whether the node supports or opposes the DAO hard-fork
}

type KnownNodeInfoWrapper struct {
	NodeId string         `json:"nodeid"` // Unique node identifier (also the encryption key)
	Info   *KnownNodeInfo `json:"info"`
}

// NodeInfo gathers and returns a collection of metadata known about the host.
func (srv *Server) KnownNodes() []*KnownNodeInfoWrapper {
	infos := make([]*KnownNodeInfoWrapper, 0, len(srv.KnownNodeInfos))
	for id, info := range srv.KnownNodeInfos {
		nodeInfo := &KnownNodeInfoWrapper{
			id.String(),
			info,
		}
		infos = append(infos, nodeInfo)
	}
	// Sort the result array alphabetically by node identifier
	for i := 0; i < len(infos); i++ {
		for j := i + 1; j < len(infos); j++ {
			if infos[i].NodeId > infos[j].NodeId {
				infos[i], infos[j] = infos[j], infos[i]
			}
		}
	}
	return infos
}
