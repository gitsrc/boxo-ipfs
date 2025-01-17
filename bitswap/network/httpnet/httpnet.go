// Package httpnet implements an Exchange network that sends and receives
// Exchange messages from peers' HTTP endpoints.
package httpnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bsmsg "github.com/ipfs/boxo/bitswap/message"
	"github.com/ipfs/boxo/bitswap/network"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
)

var log = logging.Logger("httpnet")

var ErrNoHTTPAddresses = errors.New("AddrInfo does not contain any valid HTTP addresses")
var ErrNoSuccess = errors.New("none of the peer HTTP endpoints responded successfully to request")

var _ network.BitSwapNetwork = (*httpnet)(nil)

// Defaults for the different options
var (
	DefaultMaxBlockSize       int64         = 2 << 20            // 2MiB.
	DefaultUserAgent                        = defaultUserAgent() // Usually will result in a "boxo@commitID"
	DefaultIdleConnTimeout    time.Duration = 60 * time.Second
	DefaultMaxIdleConns                     = 100
	DefaultSupportsHave                     = false
	DefaultInsecureSkipVerify               = false
)

// Option allows to configure the Network.
type Option func(net *httpnet)

// WithUserAgent sets the user agent when making requests.
func WithUserAgent(agent string) Option {
	return func(net *httpnet) {
		net.userAgent = agent
	}
}

// WithMaxBlockSize sets the maximum size of an HTTP response (block).
func WithMaxBlockSize(size int64) Option {
	return func(net *httpnet) {
		net.maxBlockSize = size
	}
}

// WithIdleConnTimeout sets how long to keep connections before closing them.
func WithIdleConnTimeout(t time.Duration) Option {
	return func(net *httpnet) {
		net.idleConnTimeout = t
	}
}

// WithMaxIdleConns sets how many idle connections we can have.
func WithMaxIdleConns(n int) Option {
	return func(net *httpnet) {
		net.maxIdleConns = n
	}
}

// WithSupportsHave specifies whether want to expose that we can handle Have
// messages (i.e. to the MessageQueue). Have messages trigger HEAD HTTP
// requests. Not all HTTP-endpoints may know how to handle a HEAD request.
func WithSupportsHave(b bool) Option {
	return func(net *httpnet) {
		net.supportsHave = b
	}
}

// WithInsecureSkipVerify allows making HTTPS connections to test servers.
// Use for testing.
func WithInsecureSkipVerify(b bool) Option {
	return func(net *httpnet) {
		net.insecureSkipVerify = b
	}
}

// WithAllowlist sets the hostnames that we are allowed to connect to via
// HTTP.
func WithAllowlist(hosts []string) Option {
	return func(net *httpnet) {
		net.allowlist = make(map[string]struct{})
		for _, h := range hosts {
			net.allowlist[h] = struct{}{}
		}
	}
}

type httpnet struct {
	// NOTE: Stats must be at the top of the heap allocation to ensure 64bit
	// alignment.
	stats network.Stats

	host   host.Host
	client *http.Client
	//dialer *dialer

	// inbound messages from the network are forwarded to the receiver
	receivers      []network.Receiver
	connEvtMgr     *network.ConnectEventManager
	pinger         *pinger
	requestTracker *requestTracker

	cooldownURLsLock sync.RWMutex
	cooldownURLs     map[string]time.Time

	// options
	userAgent          string
	maxBlockSize       int64
	idleConnTimeout    time.Duration
	maxIdleConns       int
	supportsHave       bool
	insecureSkipVerify bool
	allowlist          map[string]struct{}

	metrics *metrics
}

// New returns a BitSwapNetwork supported by underlying IPFS host.
func New(host host.Host, opts ...Option) *httpnet {
	htnet := &httpnet{
		host:               host,
		pinger:             newPinger(host),
		cooldownURLs:       make(map[string]time.Time),
		userAgent:          defaultUserAgent(),
		maxBlockSize:       DefaultMaxBlockSize,
		idleConnTimeout:    DefaultIdleConnTimeout,
		maxIdleConns:       DefaultMaxIdleConns,
		supportsHave:       DefaultSupportsHave,
		insecureSkipVerify: DefaultInsecureSkipVerify,
		metrics:            newMetrics(),
	}

	for _, opt := range opts {
		opt(htnet)
	}

	reqTracker := newRequestTracker(htnet.idleConnTimeout * 2)
	htnet.requestTracker = reqTracker

	netdialer := &net.Dialer{
		// FIXME: interaction between keep-alive  and
		// IdleConnTimeout?
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	tlsCfg := &tls.Config{
		InsecureSkipVerify: htnet.insecureSkipVerify,
		// Needed since we use a custom TLSDialer
		NextProtos: []string{"h2"},
	}

	t := &http.Transport{
		TLSClientConfig: tlsCfg,
		Proxy:           http.ProxyFromEnvironment,
		DialContext:     netdialer.DialContext, // maybe breaks wasm
		//DialTLSContext:        dialer.DialTLSContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          htnet.maxIdleConns,
		IdleConnTimeout:       htnet.idleConnTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	htransport := newTransport(t)

	c := &http.Client{
		Transport: htransport,
	}
	htnet.client = c

	return htnet
}

// Start sets up the given receivers to be notified when message responses are
// received. It also starts the connection event manager. Start must be called
// before using the Network.
func (ht *httpnet) Start(receivers ...network.Receiver) {
	log.Infof("httpnet: HTTP retrieval system started with allowlist: %s", ht.allowlist)
	ht.receivers = receivers
	connectionListeners := make([]network.ConnectionListener, len(receivers))
	for i, v := range receivers {
		connectionListeners[i] = v
	}
	ht.connEvtMgr = network.NewConnectEventManager(connectionListeners...)

	ht.connEvtMgr.Start()
}

// Stop stops the connect event manager associated with this network.
// Other methods should no longer be used after calling Stop().
func (ht *httpnet) Stop() {
	ht.connEvtMgr.Stop()
}

// Ping triggers a ping to the given peer and returns the latency.
func (ht *httpnet) Ping(ctx context.Context, p peer.ID) ping.Result {
	return ht.pinger.ping(ctx, p)

}

// Latency returns the EWMA latency for the given peer.
func (ht *httpnet) Latency(p peer.ID) time.Duration {
	return ht.pinger.latency(p)
}

func (ht *httpnet) setCooldown(u *url.URL, t time.Time) {
	ht.cooldownURLsLock.Lock()
	ht.cooldownURLs[u.String()] = t
	ht.cooldownURLsLock.Unlock()
}

func (ht *httpnet) removeCooldown(u *url.URL) {
	ht.cooldownURLsLock.Lock()
	delete(ht.cooldownURLs, u.String())
	ht.cooldownURLsLock.Unlock()
}

func (ht *httpnet) senderURLs(p peer.ID) []*senderURL {
	pi := ht.host.Peerstore().PeerInfo(p)
	urls := network.ExtractURLsFromPeer(pi)
	if len(urls) == 0 {
		return nil
	}

	senderURLs := make([]*senderURL, len(urls))
	timeNow := time.Now()
	ht.cooldownURLsLock.RLock()
	{

		for i, u := range urls {
			var cooldown time.Time
			urlstr := u.String()
			dl, ok := ht.cooldownURLs[urlstr]
			if ok && timeNow.Before(dl) {
				cooldown = dl
			}
			senderURLs[i] = &senderURL{
				url:      u,
				cooldown: cooldown,
			}
		}
	}
	ht.cooldownURLsLock.RUnlock()
	return senderURLs
}

// SendMessage sends the given message to the given peer. It uses
// NewMessageSender under the hood, with default options.
func (ht *httpnet) SendMessage(ctx context.Context, p peer.ID, msg bsmsg.BitSwapMessage) error {

	if len(msg.Wantlist()) == 0 {
		return nil
	}

	log.Debugf("SendMessage: %s", p)

	// Note: SendMessage seems to only be used to send cancellations.
	// So default options are fine.
	sender, err := ht.NewMessageSender(ctx, p, nil)
	if err != nil {
		return err
	}
	defer sender.Close()
	return sender.SendMsg(ctx, msg)
}

// Self returns the local peer ID.
func (ht *httpnet) Self() peer.ID {
	return ht.host.ID()
}

// Connect attempts setting up an HTTP connection to the given peer.  The
// given AddrInfo must include at least one HTTP endpoint for the peer.  HTTP
// URLs in AddrInfo will be tried by making an HTTP GET request to
// "ipfs/bafyaabakaieac", which is the CID for an empty directory (inlined).
// Any completed request, regardless of the HTTP response, is considered a
// connection success and marks this peer as "connected", setting it up to
// handle messages and make requests. The peer will be pinged regularly to
// collect latency measurements until DisconnectFrom() is called.
func (ht *httpnet) Connect(ctx context.Context, p peer.AddrInfo) error {
	htaddrs, _ := network.SplitHTTPAddrs(p)
	if len(htaddrs.Addrs) == 0 {
		return ErrNoHTTPAddresses
	}

	urls := network.ExtractURLsFromPeer(htaddrs)
	if len(ht.allowlist) > 0 {
		var filteredURLs []*url.URL
		for _, u := range urls {
			host, _, err := net.SplitHostPort(u.Host)
			if err != nil {
				return err
			}
			if _, ok := ht.allowlist[host]; ok {
				filteredURLs = append(filteredURLs, u)
			}
		}
		urls = filteredURLs
	}
	// if filteredURLs == 0 nothing will happen below and we will return
	// an error.

	rand.Shuffle(len(urls), func(i, j int) {
		urls[i], urls[j] = urls[j], urls[i]
	})

	// We will know try to talk to this peer by making an HTTP request.
	// This allows re-using the connection that we are about to open next
	// time with the client. The dialer callbacks will call peer.Connected()
	// on success.
	for _, u := range urls {
		req, err := ht.buildRequest(ctx, p.ID, u, "GET", "bafyaabakaieac")
		if err != nil {
			log.Debug(err)
			return err
		}

		log.Debugf("connect request to %s", req.URL)
		_, err = ht.client.Do(req)
		if err != nil {
			log.Debugf("connect error %s", err)
			if ctxErr := ctx.Err(); ctxErr != nil {
				// abort when context cancelled
				return ctxErr
			}
			continue
		}
		ht.host.Peerstore().AddAddrs(p.ID, htaddrs.Addrs, peerstore.PermanentAddrTTL)
		ht.connEvtMgr.Connected(p.ID)
		ht.pinger.startPinging(p.ID)

		return nil
		// otherwise keep trying other urls. We don't care about the
		// http status code as long as the request succeeded.
	}
	err := fmt.Errorf("%w: %s", ErrNoSuccess, p.ID)
	log.Debug(err)
	return err
}

// DisconnectFrom marks this peer as Disconnected in the connection event
// manager, stops pinging for latency measurements and removes it from the
// peerstore.
func (ht *httpnet) DisconnectFrom(ctx context.Context, p peer.ID) error {
	// this kills all ongoing requests which is more or less equivalent.
	ht.connEvtMgr.Disconnected(p)
	ht.pinger.stopPinging(p)
	ht.host.Peerstore().ClearAddrs(p)
	return nil
}

// ** We have no way of protecting a connection from our side other than using
// it so that it does not idle and gets closed.

func (ht *httpnet) TagPeer(p peer.ID, tag string, w int) {
}
func (ht *httpnet) UntagPeer(p peer.ID, tag string) {
}

func (ht *httpnet) Protect(p peer.ID, tag string) {
}
func (ht *httpnet) Unprotect(p peer.ID, tag string) bool {
	return false
}

// Stats returns message counts for this peer. Each message sent is an HTTP
// requests. Each message received is an HTTP response.
func (ht *httpnet) Stats() network.Stats {
	return network.Stats{
		MessagesRecvd: atomic.LoadUint64(&ht.stats.MessagesRecvd),
		MessagesSent:  atomic.LoadUint64(&ht.stats.MessagesSent),
	}
}

// buildRequests sets up common settings for making a requests.
func (ht *httpnet) buildRequest(ctx context.Context, pid peer.ID, u *url.URL, method string, cid string) (*http.Request, error) {
	// copy url
	sendURL, _ := url.Parse(u.String())
	sendURL.RawQuery = "format=raw"
	sendURL.Path += "/ipfs/" + cid

	req, err := http.NewRequestWithContext(ctx,
		method,
		sendURL.String(),
		nil,
	)
	if err != nil {
		log.Error(err)
		return nil, err
	}

	headers := make(http.Header)
	headers.Add("Accept", "application/vnd.ipld.raw")
	headers.Add("User-Agent", ht.userAgent)
	req.Header = headers
	return req, nil
}

// NewMessageSender returns a MessageSender implementation which sends the
// given message to the given peer over HTTP.
// An error is returned of the peer has no known HTTP endpoints.
func (ht *httpnet) NewMessageSender(ctx context.Context, p peer.ID, opts *network.MessageSenderOpts) (network.MessageSender, error) {
	// cooldowns made by other senders between now and SendMsg will not be
	// taken into account since we access that info here only. From that
	// point, we only react to cooldowns/errors received by this message
	// sender and not others. This is mostly fine given how MessageSender
	// is used as part of MessageQueue:
	//
	// * We expect peers to be associated with single urls so there will
	// not be multiple message sender for the same url normally.
	// * We remember cooldowns between message senders (i.e. when a queue
	// dies and a new one is created).
	// * We track cooldowns in the urls for the lifetime of this sender.
	//
	// This way we minimize lock contention around the cooldown map, with
	// one read access per message sender only.
	urls := ht.senderURLs(p)
	if len(urls) == 0 {
		return nil, ErrNoHTTPAddresses
	}

	log.Debugf("NewMessageSender: %s", p)
	senderOpts := setSenderOpts(opts)

	return &httpMsgSender{
		// ctx ??
		ht:      ht,
		peer:    p,
		urls:    urls,
		closing: make(chan struct{}, 1),
		opts:    senderOpts,
	}, nil
}

// defaultUserAgent returns a useful user agent version string allowing us to
// identify requests coming from official releases of this module vs forks.
func defaultUserAgent() (ua string) {
	p := reflect.ValueOf(httpnet{}).Type().PkgPath()
	// we have monorepo, so stripping the remainder
	importPath := strings.TrimSuffix(p, "/bitswap/network/httpnet")

	ua = importPath
	var module *debug.Module
	if bi, ok := debug.ReadBuildInfo(); ok {
		// If debug.ReadBuildInfo was successful, we can read Version by finding
		// this client in the dependency list of the app that has it in go.mod
		for _, dep := range bi.Deps {
			if dep.Path == importPath {
				module = dep
				break
			}
		}
		if module != nil {
			ua += "@" + module.Version
			return
		}
		ua += "@unknown"
	}
	return
}
