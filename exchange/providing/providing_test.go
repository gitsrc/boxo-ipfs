package providing

import (
	"context"
	"testing"
	"time"

	testinstance "github.com/ipfs/boxo/bitswap/testinstance"
	tn "github.com/ipfs/boxo/bitswap/testnet"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/provider"
	mockrouting "github.com/ipfs/boxo/routing/mock"
	delay "github.com/ipfs/go-ipfs-delay"
	"github.com/ipfs/go-test/random"
)

func TestExchange(t *testing.T) {
	ctx := context.Background()
	net := tn.VirtualNetwork(delay.Fixed(0))
	routing := mockrouting.NewServer()
	sg := testinstance.NewTestInstanceGenerator(net, routing, nil, nil)
	i := sg.Next()
	provFinder := routing.Client(i.Identity)
	prov, err := provider.New(i.Datastore,
		provider.Online(provFinder),
	)
	if err != nil {
		t.Fatal(err)
	}
	provExchange := New(i.Exchange, prov)
	// write-through so that we notify when re-adding block
	bs := blockservice.New(i.Blockstore, provExchange,
		blockservice.WriteThrough(true))
	block := random.BlocksOfSize(1, 10)[0]
	// put it on the blockstore of the first instance
	err = i.Blockstore.Put(ctx, block)
	if err != nil {
		t.Fatal()
	}

	// Trigger reproviding, otherwise it's not really provided.
	err = prov.Reprovide(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	providersChan := provFinder.FindProvidersAsync(ctx, block.Cid(), 1)
	_, ok := <-providersChan
	if ok {
		t.Fatal("there should be no providers yet for block")
	}

	// Now add it via BlockService. It should trigger NotifyNewBlocks
	// on the exchange and thus they should get announced.
	err = bs.AddBlock(ctx, block)
	if err != nil {
		t.Fatal()
	}
	// Trigger reproviding, otherwise it's not really provided.
	err = prov.Reprovide(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	providersChan = provFinder.FindProvidersAsync(ctx, block.Cid(), 1)
	_, ok = <-providersChan
	if !ok {
		t.Fatal("there should be one provider for the block")
	}
}

func TestExchangeWithProvideDisabled(t *testing.T) {
	ctx := context.Background()
	net := tn.VirtualNetwork(delay.Fixed(0))
	routing := mockrouting.NewServer()
	sg := testinstance.NewTestInstanceGenerator(net, routing, nil, nil)
	i := sg.Next()
	provFinder := routing.Client(i.Identity)
	prov, err := provider.New(i.Datastore,
		provider.Online(provFinder),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Create exchange with providing disabled
	provExchange := New(i.Exchange, prov, WithProvideEnabled(false))
	// write-through so that we notify when re-adding block
	bs := blockservice.New(i.Blockstore, provExchange,
		blockservice.WriteThrough(true))
	block := random.BlocksOfSize(1, 10)[0]

	// Add block via BlockService
	err = bs.AddBlock(ctx, block)
	if err != nil {
		t.Fatal(err)
	}

	// Trigger reproviding
	err = prov.Reprovide(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Since providing is disabled, there should be no providers
	providersChan := provFinder.FindProvidersAsync(ctx, block.Cid(), 1)
	_, ok := <-providersChan
	if ok {
		t.Fatal("there should be no providers when providing is disabled")
	}
}

func TestExchangeWithContextSuppressProvide(t *testing.T) {
	ctx := context.Background()
	net := tn.VirtualNetwork(delay.Fixed(0))
	routing := mockrouting.NewServer()
	sg := testinstance.NewTestInstanceGenerator(net, routing, nil, nil)
	i := sg.Next()
	provFinder := routing.Client(i.Identity)
	prov, err := provider.New(i.Datastore,
		provider.Online(provFinder),
	)
	if err != nil {
		t.Fatal(err)
	}

	// Create exchange with providing enabled (default)
	provExchange := New(i.Exchange, prov)

	// Create a context that suppresses providing
	suppressCtx := ContextWithSuppressProvide(ctx)

	// Directly call NotifyNewBlocks with the suppress context
	block := random.BlocksOfSize(1, 10)[0]
	err = provExchange.NotifyNewBlocks(suppressCtx, block)
	if err != nil {
		t.Fatal(err)
	}

	// Trigger reproviding
	err = prov.Reprovide(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// Since providing was suppressed via context, there should be no providers
	providersChan := provFinder.FindProvidersAsync(ctx, block.Cid(), 1)
	_, ok := <-providersChan
	if ok {
		t.Fatal("there should be no providers when providing is suppressed via context")
	}

	// Now try with normal context - should provide
	block2 := random.BlocksOfSize(1, 10)[0]
	err = provExchange.NotifyNewBlocks(ctx, block2)
	if err != nil {
		t.Fatal(err)
	}

	// Trigger reproviding
	err = prov.Reprovide(ctx)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	// With normal context, the block should be provided
	providersChan = provFinder.FindProvidersAsync(ctx, block2.Cid(), 1)
	_, ok = <-providersChan
	if !ok {
		t.Fatal("there should be a provider with normal context")
	}
}
