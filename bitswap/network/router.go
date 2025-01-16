package network

import (
	"context"
	"time"

	bsmsg "github.com/ipfs/boxo/bitswap/message"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/protocol/ping"
	"go.uber.org/multierr"
)

type router struct {
	Bitswap   BitSwapNetwork
	HTTP      BitSwapNetwork
	Peerstore peerstore.Peerstore
}

// New returns a BitSwapNetwork supported by underlying IPFS host.
func New(pstore peerstore.Peerstore, bitswap BitSwapNetwork, http BitSwapNetwork) BitSwapNetwork {
	return &router{
		Bitswap: bitswap,
		HTTP:    http,
	}
}

func (rt *router) Start(receivers ...Receiver) {
	rt.Bitswap.Start(receivers...)
	rt.HTTP.Start(receivers...)
}

func (rt *router) Stop() {
	rt.Bitswap.Stop()
	rt.HTTP.Stop()
}

// Should be the same for both
func (rt *router) Self() peer.ID {
	return rt.Bitswap.Self()
}

func (rt *router) Ping(ctx context.Context, p peer.ID) ping.Result {
	pi := rt.Peerstore.PeerInfo(p)
	htaddrs, _ := SplitHTTPAddrs(pi)
	if len(htaddrs.Addrs) > 0 {
		return rt.HTTP.Ping(ctx, p)
	}
	return rt.Bitswap.Ping(ctx, p)
}

func (rt *router) Latency(p peer.ID) time.Duration {
	pi := rt.Peerstore.PeerInfo(p)
	htaddrs, _ := SplitHTTPAddrs(pi)
	if len(htaddrs.Addrs) > 0 {
		return rt.HTTP.Latency(p)
	}
	return rt.Bitswap.Latency(p)
}

func (rt *router) SendMessage(ctx context.Context, p peer.ID, msg bsmsg.BitSwapMessage) error {
	// SendMessage is only used by bitswap server so we send a bitswap
	// message.
	return rt.Bitswap.SendMessage(ctx, p, msg)
}

// Connect attempts to connect to a peer. It prioritizes HTTP connections over
// bitswap.
func (rt *router) Connect(ctx context.Context, p peer.AddrInfo) error {
	htaddrs, _ := SplitHTTPAddrs(p)
	if len(htaddrs.Addrs) > 0 {
		return rt.HTTP.Connect(ctx, p)
	} else {
		return rt.Bitswap.Connect(ctx, p)
	}
}

func (rt *router) DisconnectFrom(ctx context.Context, p peer.ID) error {
	return multierr.Combine(
		rt.HTTP.DisconnectFrom(ctx, p),
		rt.Bitswap.DisconnectFrom(ctx, p),
	)
}

func (rt *router) Stats() Stats {
	htstats := rt.HTTP.Stats()
	bsstats := rt.Bitswap.Stats()
	return Stats{
		MessagesRecvd: htstats.MessagesRecvd + bsstats.MessagesRecvd,
		MessagesSent:  htstats.MessagesSent + bsstats.MessagesSent,
	}
}

// NewMessageSender returns a MessageSender using the HTTP network when HTTP
// addresses are knwon, and bitswap otherwise.
func (rt *router) NewMessageSender(ctx context.Context, p peer.ID, opts *MessageSenderOpts) (MessageSender, error) {
	pi := rt.Peerstore.PeerInfo(p)
	htaddrs, _ := SplitHTTPAddrs(pi)
	if len(htaddrs.Addrs) > 0 {
		return rt.HTTP.NewMessageSender(ctx, p, opts)
	}
	return rt.Bitswap.NewMessageSender(ctx, p, opts)
}

func (rt *router) TagPeer(p peer.ID, tag string, w int) {
	rt.HTTP.TagPeer(p, tag, w)
	rt.Bitswap.TagPeer(p, tag, w)
}

func (rt *router) UntagPeer(p peer.ID, tag string) {
	rt.HTTP.UntagPeer(p, tag)
	rt.Bitswap.UntagPeer(p, tag)
}

func (rt *router) Protect(p peer.ID, tag string) {
	rt.HTTP.Protect(p, tag)
	rt.Bitswap.Protect(p, tag)
}
func (rt *router) Unprotect(p peer.ID, tag string) bool {
	return rt.HTTP.Unprotect(p, tag) || rt.Bitswap.Unprotect(p, tag)
}
