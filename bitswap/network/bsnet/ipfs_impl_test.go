package bsnet_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	bsmsg "github.com/ipfs/boxo/bitswap/message"
	pb "github.com/ipfs/boxo/bitswap/message/pb"
	network "github.com/ipfs/boxo/bitswap/network"
	bsnet "github.com/ipfs/boxo/bitswap/network/bsnet"
	"github.com/ipfs/boxo/bitswap/network/bsnet/internal"
	tn "github.com/ipfs/boxo/bitswap/testnet"
	"github.com/ipfs/go-test/random"
	tnet "github.com/libp2p/go-libp2p-testing/net"
	"github.com/libp2p/go-libp2p/core/host"
	p2pnet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	"github.com/multiformats/go-multistream"
)

// Receiver is an interface for receiving messages from the GraphSyncNetwork.
type receiver struct {
	peers           map[peer.ID]struct{}
	messageReceived chan struct{}
	connectionEvent chan bool
	lastMessage     bsmsg.BitSwapMessage
	lastSender      peer.ID
	listener        p2pnet.Notifiee
}

func newReceiver() *receiver {
	return &receiver{
		peers:           make(map[peer.ID]struct{}),
		messageReceived: make(chan struct{}),
		// Avoid blocking. 100 is good enough for tests.
		connectionEvent: make(chan bool, 100),
	}
}

func (r *receiver) ReceiveMessage(
	ctx context.Context,
	sender peer.ID,
	incoming bsmsg.BitSwapMessage,
) {
	r.lastSender = sender
	r.lastMessage = incoming
	select {
	case <-ctx.Done():
	case r.messageReceived <- struct{}{}:
	}
}

func (r *receiver) ReceiveError(err error) {
}

func (r *receiver) PeerConnected(p peer.ID) {
	r.peers[p] = struct{}{}
	r.connectionEvent <- true
}

func (r *receiver) PeerDisconnected(p peer.ID) {
	delete(r.peers, p)
	r.connectionEvent <- false
}

var errMockNetErr = errors.New("network err")

type ErrStream struct {
	p2pnet.Stream
	lk        sync.Mutex
	err       error
	timingOut bool
	closed    bool
}

type ErrHost struct {
	host.Host
	lk        sync.Mutex
	err       error
	timingOut bool
	streams   []*ErrStream
}

func (es *ErrStream) Write(b []byte) (int, error) {
	es.lk.Lock()
	defer es.lk.Unlock()

	if es.err != nil {
		return 0, es.err
	}
	if es.timingOut {
		return 0, context.DeadlineExceeded
	}
	return es.Stream.Write(b)
}

func (es *ErrStream) Close() error {
	es.lk.Lock()
	es.closed = true
	es.lk.Unlock()

	return es.Stream.Close()
}

func (es *ErrStream) Reset() error {
	es.lk.Lock()
	es.closed = true
	es.lk.Unlock()

	return es.Stream.Reset()
}

func (eh *ErrHost) Connect(ctx context.Context, pi peer.AddrInfo) error {
	eh.lk.Lock()
	defer eh.lk.Unlock()

	if eh.err != nil {
		return eh.err
	}
	if eh.timingOut {
		return context.DeadlineExceeded
	}
	return eh.Host.Connect(ctx, pi)
}

func (eh *ErrHost) NewStream(ctx context.Context, p peer.ID, pids ...protocol.ID) (p2pnet.Stream, error) {
	eh.lk.Lock()
	defer eh.lk.Unlock()

	if eh.err != nil {
		return nil, errMockNetErr
	}
	if eh.timingOut {
		return nil, context.DeadlineExceeded
	}
	stream, err := eh.Host.NewStream(ctx, p, pids...)
	estrm := &ErrStream{Stream: stream, err: eh.err, timingOut: eh.timingOut}

	eh.streams = append(eh.streams, estrm)
	return estrm, err
}

func (eh *ErrHost) setError(err error) {
	eh.lk.Lock()
	defer eh.lk.Unlock()

	eh.err = err
	for _, s := range eh.streams {
		s.lk.Lock()
		s.err = err
		s.lk.Unlock()
	}
}

func (eh *ErrHost) setTimeoutState(timingOut bool) {
	eh.lk.Lock()
	defer eh.lk.Unlock()

	eh.timingOut = timingOut
	for _, s := range eh.streams {
		s.lk.Lock()
		s.timingOut = timingOut
		s.lk.Unlock()
	}
}

func TestMessageSendAndReceive(t *testing.T) {
	// create network
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	mn := mocknet.New()
	defer mn.Close()
	streamNet, err := tn.StreamNet(ctx, mn)
	if err != nil {
		t.Fatal("Unable to setup network")
	}
	p1 := tnet.RandIdentityOrFatal(t)
	p2 := tnet.RandIdentityOrFatal(t)

	bsnet1 := streamNet.Adapter(p1)
	bsnet2 := streamNet.Adapter(p2)
	r1 := newReceiver()
	r2 := newReceiver()
	bsnet1.Start(r1)
	t.Cleanup(bsnet1.Stop)
	bsnet2.Start(r2)
	t.Cleanup(bsnet2.Stop)

	err = mn.LinkAll()
	if err != nil {
		t.Fatal(err)
	}
	err = bsnet1.Connect(ctx, peer.AddrInfo{ID: p2.ID()})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("did not connect peer")
	case <-r1.connectionEvent:
	}
	err = bsnet2.Connect(ctx, peer.AddrInfo{ID: p1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("did not connect peer")
	case <-r2.connectionEvent:
	}
	if _, ok := r1.peers[p2.ID()]; !ok {
		t.Fatal("did to connect to correct peer")
	}
	if _, ok := r2.peers[p1.ID()]; !ok {
		t.Fatal("did to connect to correct peer")
	}
	randBlocks := random.BlocksOfSize(2, 4)
	block1 := randBlocks[0]
	block2 := randBlocks[1]

	sent := bsmsg.New(false)
	sent.AddEntry(block1.Cid(), 1, pb.Message_Wantlist_Block, true)
	sent.AddBlock(block2)

	err = bsnet1.SendMessage(ctx, p2.ID(), sent)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("did not receive message sent")
	case <-r2.messageReceived:
	}

	sender := r2.lastSender
	if sender != p1.ID() {
		t.Fatal("received message from wrong node")
	}

	received := r2.lastMessage

	sentWants := sent.Wantlist()
	if len(sentWants) != 1 {
		t.Fatal("Did not add want to sent message")
	}
	sentWant := sentWants[0]
	receivedWants := received.Wantlist()
	if len(receivedWants) != 1 {
		t.Fatal("Did not add want to received message")
	}
	receivedWant := receivedWants[0]
	if receivedWant.Cid != sentWant.Cid ||
		receivedWant.Priority != sentWant.Priority ||
		receivedWant.Cancel != sentWant.Cancel {
		t.Fatal("Sent message wants did not match received message wants")
	}
	sentBlocks := sent.Blocks()
	if len(sentBlocks) != 1 {
		t.Fatal("Did not add block to sent message")
	}
	sentBlock := sentBlocks[0]
	receivedBlocks := received.Blocks()
	if len(receivedBlocks) != 1 {
		t.Fatal("Did not add response to received message")
	}
	receivedBlock := receivedBlocks[0]
	if receivedBlock.Cid() != sentBlock.Cid() {
		t.Fatal("Sent message blocks did not match received message blocks")
	}
}

func prepareNetwork(t *testing.T, ctx context.Context, p1 tnet.Identity, r1 *receiver, p2 tnet.Identity, r2 *receiver) (*ErrHost, network.BitSwapNetwork, *ErrHost, network.BitSwapNetwork, bsmsg.BitSwapMessage) {
	// create network
	mn := mocknet.New()
	defer mn.Close()

	// Host 1
	h1, err := mn.AddPeer(p1.PrivateKey(), p1.Address())
	if err != nil {
		t.Fatal(err)
	}
	eh1 := &ErrHost{Host: h1}
	bsnet1 := bsnet.NewFromIpfsHost(eh1)
	bsnet1.Start(r1)
	t.Cleanup(bsnet1.Stop)
	if r1.listener != nil {
		eh1.Network().Notify(r1.listener)
	}

	// Host 2
	h2, err := mn.AddPeer(p2.PrivateKey(), p2.Address())
	if err != nil {
		t.Fatal(err)
	}
	eh2 := &ErrHost{Host: h2}
	bsnet2 := bsnet.NewFromIpfsHost(eh2)
	bsnet2.Start(r2)
	t.Cleanup(bsnet2.Stop)
	if r2.listener != nil {
		eh2.Network().Notify(r2.listener)
	}

	// Networking
	err = mn.LinkAll()
	if err != nil {
		t.Fatal(err)
	}
	err = bsnet1.Connect(ctx, peer.AddrInfo{ID: p2.ID()})
	if err != nil {
		t.Fatal(err)
	}
	isConnected := <-r1.connectionEvent
	if !isConnected {
		t.Fatal("Expected connect event")
	}

	err = bsnet2.Connect(ctx, peer.AddrInfo{ID: p1.ID()})
	if err != nil {
		t.Fatal(err)
	}

	block1 := random.BlocksOfSize(1, 4)[0]
	msg := bsmsg.New(false)
	msg.AddEntry(block1.Cid(), 1, pb.Message_Wantlist_Block, true)

	return eh1, bsnet1, eh2, bsnet2, msg
}

func TestMessageResendAfterError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	p1 := tnet.RandIdentityOrFatal(t)
	r1 := newReceiver()
	p2 := tnet.RandIdentityOrFatal(t)
	r2 := newReceiver()

	eh, bsnet1, _, _, msg := prepareNetwork(t, ctx, p1, r1, p2, r2)

	testSendErrorBackoff := 100 * time.Millisecond
	ms, err := bsnet1.NewMessageSender(ctx, p2.ID(), &network.MessageSenderOpts{
		MaxRetries:       3,
		SendTimeout:      100 * time.Millisecond,
		SendErrorBackoff: testSendErrorBackoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Reset()

	// Return an error from the networking layer the next time we try to send
	// a message
	eh.setError(errMockNetErr)

	go func() {
		time.Sleep(testSendErrorBackoff / 2)
		// Stop throwing errors so that the following attempt to send succeeds
		eh.setError(nil)
	}()

	// Send message with retries, first one should fail, then subsequent
	// message should succeed
	err = ms.SendMsg(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-ctx.Done():
		t.Fatal("did not receive message sent")
	case <-r2.messageReceived:
	}
}

func TestMessageSendTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	p1 := tnet.RandIdentityOrFatal(t)
	r1 := newReceiver()
	p2 := tnet.RandIdentityOrFatal(t)
	r2 := newReceiver()

	eh, bsnet1, _, _, msg := prepareNetwork(t, ctx, p1, r1, p2, r2)

	ms, err := bsnet1.NewMessageSender(ctx, p2.ID(), &network.MessageSenderOpts{
		MaxRetries:       3,
		SendTimeout:      100 * time.Millisecond,
		SendErrorBackoff: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Reset()

	// Return a DeadlineExceeded error from the networking layer the next time we try to
	// send a message
	eh.setTimeoutState(true)

	// Send message with retries, all attempts should fail
	err = ms.SendMsg(ctx, msg)
	if err == nil {
		t.Fatal("Expected error from SednMsg")
	}

	select {
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Did not receive disconnect event")
	case isConnected := <-r1.connectionEvent:
		if isConnected {
			t.Fatal("Expected disconnect event (got connect event)")
		}
	}
}

func TestMessageSendNotSupportedResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	p1 := tnet.RandIdentityOrFatal(t)
	r1 := newReceiver()
	p2 := tnet.RandIdentityOrFatal(t)
	r2 := newReceiver()

	eh, bsnet1, _, _, _ := prepareNetwork(t, ctx, p1, r1, p2, r2)

	eh.setError(multistream.ErrNotSupported[protocol.ID]{})
	ms, err := bsnet1.NewMessageSender(ctx, p2.ID(), &network.MessageSenderOpts{
		MaxRetries:       3,
		SendTimeout:      100 * time.Millisecond,
		SendErrorBackoff: 100 * time.Millisecond,
	})
	if err == nil {
		ms.Reset()
		t.Fatal("Expected ErrNotSupported")
	}

	select {
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Did not receive disconnect event")
	case isConnected := <-r1.connectionEvent:
		if isConnected {
			t.Fatal("Expected disconnect event (got connect event)")
		}
	}
}

func TestSupportsHave(t *testing.T) {
	ctx := context.Background()
	mn := mocknet.New()
	defer mn.Close()
	streamNet, err := tn.StreamNet(ctx, mn)
	if err != nil {
		t.Fatalf("Unable to setup network: %s", err)
	}

	type testCase struct {
		proto           protocol.ID
		expSupportsHave bool
	}

	testCases := []testCase{
		{bsnet.ProtocolBitswap, true},
		{bsnet.ProtocolBitswapOneOne, false},
		{bsnet.ProtocolBitswapOneZero, false},
		{bsnet.ProtocolBitswapNoVers, false},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s-%v", tc.proto, tc.expSupportsHave), func(t *testing.T) {
			p1 := tnet.RandIdentityOrFatal(t)
			bsnet1 := streamNet.Adapter(p1)
			bsnet1.Start(newReceiver())
			t.Cleanup(bsnet1.Stop)

			p2 := tnet.RandIdentityOrFatal(t)
			bsnet2 := streamNet.Adapter(p2, bsnet.SupportedProtocols([]protocol.ID{tc.proto}))
			bsnet2.Start(newReceiver())
			t.Cleanup(bsnet2.Stop)

			err = mn.LinkAll()
			if err != nil {
				t.Fatal(err)
			}

			senderCurrent, err := bsnet1.NewMessageSender(ctx, p2.ID(), &network.MessageSenderOpts{})
			if err != nil {
				t.Fatal(err)
			}
			defer senderCurrent.Reset()

			if senderCurrent.SupportsHave() != tc.expSupportsHave {
				t.Fatal("Expected sender HAVE message support", tc.proto, tc.expSupportsHave)
			}
		})
	}
}

func testNetworkCounters(t *testing.T, n1 int, n2 int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p1 := tnet.RandIdentityOrFatal(t)
	r1 := newReceiver()
	p2 := tnet.RandIdentityOrFatal(t)
	r2 := newReceiver()

	h1, bsnet1, h2, bsnet2, msg := prepareNetwork(t, ctx, p1, r1, p2, r2)

	for n := 0; n < n1; n++ {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		err := bsnet1.SendMessage(ctx, p2.ID(), msg)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("p2 did not receive message sent")
		case <-r2.messageReceived:
			for j := 0; j < 2; j++ {
				err := bsnet2.SendMessage(ctx, p1.ID(), msg)
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatal("p1 did not receive message sent")
				case <-r1.messageReceived:
				}
			}
		}
		cancel()
	}

	if n2 > 0 {
		ms, err := bsnet1.NewMessageSender(ctx, p2.ID(), &network.MessageSenderOpts{})
		if err != nil {
			t.Fatal(err)
		}
		defer ms.Reset()
		for n := 0; n < n2; n++ {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			err = ms.SendMsg(ctx, msg)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
				t.Fatal("p2 did not receive message sent")
			case <-r2.messageReceived:
				for j := 0; j < 2; j++ {
					err := bsnet2.SendMessage(ctx, p1.ID(), msg)
					if err != nil {
						t.Fatal(err)
					}
					select {
					case <-ctx.Done():
						t.Fatal("p1 did not receive message sent")
					case <-r1.messageReceived:
					}
				}
			}
			cancel()
		}
		ms.Reset()
	}

	// Wait until all streams are closed and MessagesRecvd counters
	// updated.
	ctxto, cancelto := context.WithTimeout(ctx, 5*time.Second)
	defer cancelto()
	ctxwait, cancelwait := context.WithCancel(ctx)
	go func() {
		// Wait until all streams are closed
		throttler := time.NewTicker(time.Millisecond * 5)
		defer throttler.Stop()
		for {
			h1.lk.Lock()
			var done bool
			for _, s := range h1.streams {
				s.lk.Lock()
				closed := s.closed
				closed = closed || s.err != nil
				s.lk.Unlock()
				if closed {
					continue
				}
				pid := s.Protocol()
				for _, v := range internal.DefaultProtocols {
					if pid == v {
						goto ElseH1
					}
				}
			}
			done = true
		ElseH1:
			h1.lk.Unlock()
			if done {
				break
			}
			select {
			case <-ctxto.Done():
				return
			case <-throttler.C:
			}
		}

		for {
			h2.lk.Lock()
			var done bool
			for _, s := range h2.streams {
				s.lk.Lock()
				closed := s.closed
				closed = closed || s.err != nil
				s.lk.Unlock()
				if closed {
					continue
				}
				pid := s.Protocol()
				for _, v := range internal.DefaultProtocols {
					if pid == v {
						goto ElseH2
					}
				}
			}
			done = true
		ElseH2:
			h2.lk.Unlock()
			if done {
				break
			}
			select {
			case <-ctxto.Done():
				return
			case <-throttler.C:
			}
		}

		cancelwait()
	}()

	select {
	case <-ctxto.Done():
		t.Fatal("network streams closing timed out")
	case <-ctxwait.Done():
	}

	if bsnet1.Stats().MessagesSent != uint64(n1+n2) {
		t.Fatal(fmt.Errorf("expected %d sent messages, got %d", n1+n2, bsnet1.Stats().MessagesSent))
	}

	if bsnet2.Stats().MessagesRecvd != uint64(n1+n2) {
		t.Fatal(fmt.Errorf("expected %d received messages, got %d", n1+n2, bsnet2.Stats().MessagesRecvd))
	}

	if bsnet1.Stats().MessagesRecvd != 2*uint64(n1+n2) {
		t.Fatal(fmt.Errorf("expected %d received reply messages, got %d", 2*(n1+n2), bsnet1.Stats().MessagesRecvd))
	}
}

func TestNetworkCounters(t *testing.T) {
	for n := 0; n < 11; n++ {
		testNetworkCounters(t, 10-n, n)
	}
}
