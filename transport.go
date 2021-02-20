package libp2pquic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/connmgr"
	n "github.com/libp2p/go-libp2p-core/network"

	"github.com/minio/sha256-simd"
	"golang.org/x/crypto/hkdf"

	logging "github.com/ipfs/go-log"
	ic "github.com/libp2p/go-libp2p-core/crypto"
	in "github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/pnet"
	tpt "github.com/libp2p/go-libp2p-core/transport"
	p2ptls "github.com/libp2p/go-libp2p-tls"
	quic "github.com/lucas-clemente/quic-go"
	ma "github.com/multiformats/go-multiaddr"
	mafmt "github.com/multiformats/go-multiaddr-fmt"
	manet "github.com/multiformats/go-multiaddr/net"
)

var log = logging.Logger("quic-transport")

var ErrHolePunching = errors.New("hole punching attempted; no active dial")

var quicDialContext = quic.DialContext // so we can mock it in tests

var quicConfig = &quic.Config{
	MaxIncomingStreams:                    1000,
	MaxIncomingUniStreams:                 -1,             // disable unidirectional streams
	MaxReceiveStreamFlowControlWindow:     10 * (1 << 20), // 10 MB
	MaxReceiveConnectionFlowControlWindow: 15 * (1 << 20), // 15 MB
	AcceptToken: func(clientAddr net.Addr, _ *quic.Token) bool {
		// TODO(#6): require source address validation when under load
		return true
	},
	KeepAlive: true,
	Versions:  []quic.VersionNumber{quic.VersionDraft29, quic.VersionDraft32},
}

const statelessResetKeyInfo = "libp2p quic stateless reset key"
const errorCodeConnectionGating = 0x47415445 // GATE in ASCII

type connManager struct {
	reuseUDP4 *reuse
	reuseUDP6 *reuse
}

func newConnManager(gater connmgr.ConnectionGater) (*connManager, error) {
	reuseUDP4 := newReuse(gater)
	reuseUDP6 := newReuse(gater)

	return &connManager{
		reuseUDP4: reuseUDP4,
		reuseUDP6: reuseUDP6,
	}, nil
}

func (c *connManager) getReuse(network string) (*reuse, error) {
	switch network {
	case "udp4":
		return c.reuseUDP4, nil
	case "udp6":
		return c.reuseUDP6, nil
	default:
		return nil, errors.New("invalid network: must be either udp4 or udp6")
	}
}

func (c *connManager) Listen(network string, laddr *net.UDPAddr) (*reuseConn, error) {
	reuse, err := c.getReuse(network)
	if err != nil {
		return nil, err
	}
	return reuse.Listen(network, laddr)
}

func (c *connManager) Dial(network string, raddr *net.UDPAddr) (*reuseConn, error) {
	reuse, err := c.getReuse(network)
	if err != nil {
		return nil, err
	}
	return reuse.Dial(network, raddr)
}

// The Transport implements the tpt.Transport interface for QUIC connections.
type transport struct {
	privKey      ic.PrivKey
	localPeer    peer.ID
	identity     *p2ptls.Identity
	connManager  *connManager
	serverConfig *quic.Config
	clientConfig *quic.Config
	gater        connmgr.ConnectionGater

	holePunchingMx sync.Mutex
	holePunching   map[peer.ID]map[*net.UDPAddr]context.CancelFunc
}

var _ tpt.Transport = &transport{}

// NewTransport creates a new QUIC transport
func NewTransport(key ic.PrivKey, psk pnet.PSK, gater connmgr.ConnectionGater) (tpt.Transport, error) {
	if len(psk) > 0 {
		log.Error("QUIC doesn't support private networks yet.")
		return nil, errors.New("QUIC doesn't support private networks yet")
	}
	localPeer, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return nil, err
	}
	identity, err := p2ptls.NewIdentity(key)
	if err != nil {
		return nil, err
	}
	connManager, err := newConnManager(gater)
	if err != nil {
		return nil, err
	}
	config := quicConfig.Clone()
	keyBytes, err := key.Raw()
	if err != nil {
		return nil, err
	}
	keyReader := hkdf.New(sha256.New, keyBytes, nil, []byte(statelessResetKeyInfo))
	config.StatelessResetKey = make([]byte, 32)
	if _, err := io.ReadFull(keyReader, config.StatelessResetKey); err != nil {
		return nil, err
	}
	config.Tracer = tracer

	return &transport{
		privKey:      key,
		localPeer:    localPeer,
		identity:     identity,
		connManager:  connManager,
		serverConfig: config,
		clientConfig: config.Clone(),
		gater:        gater,
		holePunching: make(map[peer.ID]map[*net.UDPAddr]context.CancelFunc),
	}, nil
}

// Dial dials a new QUIC connection
func (t *transport) Dial(ctx context.Context, raddr ma.Multiaddr, p peer.ID) (tpt.CapableConn, error) {
	network, host, err := manet.DialArgs(raddr)
	if err != nil {
		return nil, err
	}
	addr, err := net.ResolveUDPAddr(network, host)
	if err != nil {
		return nil, err
	}
	remoteMultiaddr, err := toQuicMultiaddr(addr)
	if err != nil {
		return nil, err
	}
	tlsConf, keyCh := t.identity.ConfigForPeer(p)

	if simConnect, _ := in.GetSimultaneousConnect(ctx); simConnect {
		if bytes.Compare([]byte(t.localPeer), []byte(p)) < 0 {
			err = t.holePunch(ctx, p, network, addr)
			if err == nil {
				err = ErrHolePunching
			}
			return nil, err
		}
	}

	pconn, err := t.connManager.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	sess, err := quicDialContext(ctx, pconn, addr, host, tlsConf, t.clientConfig)
	if err != nil {
		pconn.DecreaseCount()
		return nil, err
	}
	// Should be ready by this point, don't block.
	var remotePubKey ic.PubKey
	select {
	case remotePubKey = <-keyCh:
	default:
	}
	if remotePubKey == nil {
		pconn.DecreaseCount()
		return nil, errors.New("go-libp2p-quic-transport BUG: expected remote pub key to be set")
	}
	go func() {
		<-sess.Context().Done()
		pconn.DecreaseCount()
	}()

	localMultiaddr, err := toQuicMultiaddr(pconn.LocalAddr())
	if err != nil {
		sess.CloseWithError(0, "")
		return nil, err
	}
	conn := &conn{
		sess:            sess,
		transport:       t,
		privKey:         t.privKey,
		localPeer:       t.localPeer,
		localMultiaddr:  localMultiaddr,
		remotePubKey:    remotePubKey,
		remotePeerID:    p,
		remoteMultiaddr: remoteMultiaddr,
	}
	if t.gater != nil && !t.gater.InterceptSecured(n.DirOutbound, p, conn) {
		sess.CloseWithError(errorCodeConnectionGating, "connection gated")
		return nil, fmt.Errorf("secured connection gated")
	}
	return conn, nil
}

func (t *transport) holePunch(ctx context.Context, p peer.ID, network string, addr *net.UDPAddr) error {
	pconn, err := t.connManager.Dial(network, addr)
	if err != nil {
		return err
	}
	defer pconn.DecreaseCount()

	ctx, cancel := context.WithTimeout(ctx, time.Second)

	t.holePunchingMx.Lock()
	holePunches, ok := t.holePunching[p]
	if !ok {
		holePunches = make(map[*net.UDPAddr]context.CancelFunc)
		t.holePunching[p] = holePunches
	}
	holePunches[addr] = cancel
	t.holePunchingMx.Unlock()

	defer func() {
		t.holePunchingMx.Lock()
		defer t.holePunchingMx.Unlock()

		holePunches, ok := t.holePunching[p]
		if ok {
			cancel()
			delete(holePunches, addr)
		}
	}()

	conn := pconn.UDPConn
	payload := make([]byte, 64)

	for i := 0; true; i++ {
		_, err = rand.Read(payload)
		if err != nil {
			return err
		}

		_, err = conn.WriteToUDP(payload, addr)
		if err != nil {
			return err
		}

		sleep := 10*time.Millisecond + time.Duration(rand.Intn((i+1)*int(10*time.Millisecond)))
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return nil
		}
	}

	return nil
}

// Don't use mafmt.QUIC as we don't want to dial DNS addresses. Just /ip{4,6}/udp/quic
var dialMatcher = mafmt.And(mafmt.IP, mafmt.Base(ma.P_UDP), mafmt.Base(ma.P_QUIC))

// CanDial determines if we can dial to an address
func (t *transport) CanDial(addr ma.Multiaddr) bool {
	return dialMatcher.Matches(addr)
}

// Listen listens for new QUIC connections on the passed multiaddr.
func (t *transport) Listen(addr ma.Multiaddr) (tpt.Listener, error) {
	lnet, host, err := manet.DialArgs(addr)
	if err != nil {
		return nil, err
	}
	laddr, err := net.ResolveUDPAddr(lnet, host)
	if err != nil {
		return nil, err
	}
	conn, err := t.connManager.Listen(lnet, laddr)
	if err != nil {
		return nil, err
	}
	ln, err := newListener(conn, t, t.localPeer, t.privKey, t.identity)
	if err != nil {
		conn.DecreaseCount()
		return nil, err
	}
	return ln, nil
}

// Proxy returns true if this transport proxies.
func (t *transport) Proxy() bool {
	return false
}

// Protocols returns the set of protocols handled by this transport.
func (t *transport) Protocols() []int {
	return []int{ma.P_QUIC}
}

func (t *transport) String() string {
	return "QUIC"
}
