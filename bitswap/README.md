go-bitswap
==================

> An implementation of the bitswap protocol in go!

## Table of Contents

- [Background](#background)
- [Usage](#usage)
- [Implementation](#implementation)
- [Contribute](#contribute)
- [License](#license)

## Background

Bitswap is the data trading module for ipfs. It manages requesting and sending
blocks to and from other peers in the network. Bitswap has two main jobs:
- to acquire blocks requested by the client from the network
- to judiciously send blocks in its possession to other peers who want them

Bitswap is a message based protocol, as opposed to request-response. All messages
contain wantlists or blocks.

A node sends a wantlist to tell peers which blocks it wants. When a node receives
a wantlist it should check which blocks it has from the wantlist, and consider
sending the matching blocks to the requester.

When a node receives blocks that it asked for, the node should send out a
notification called a 'Cancel' to tell its peers that the node no longer
wants those blocks.

`go-bitswap` provides an implementation of the Bitswap protocol in go.

[Learn more about how Bitswap works](./client/docs/how-bitswap-works.md)

## Usage

### Initializing a Bitswap Exchange

```golang
import (
  "context"
  bitswap "github.com/ipfs/boxo/bitswap"
  bsnet "github.com/ipfs/boxo/bitswap/network"
  blockstore "github.com/ipfs/go-ipfs-blockstore"
  "github.com/libp2p/go-libp2p-core/routing"
  "github.com/libp2p/go-libp2p-core/host"
)

var ctx context.Context
var host host.Host
var router routing.ContentRouting
var bstore blockstore.Blockstore

network := bsnet.NewFromIpfsHost(host, router)
exchange := bitswap.New(ctx, network, bstore)
```

Parameter Notes:

1. `ctx` is just the parent context for all of Bitswap
2. `network` is a network abstraction provided to Bitswap on top of libp2p & content routing.
3. `bstore` is an IPFS blockstore

### Get A Block Synchronously

```golang
var c cid.Cid
var ctx context.Context
var exchange bitswap.Bitswap

block, err := exchange.GetBlock(ctx, c)
```

Parameter Notes:

1. `ctx` is the context for this request, which can be canceled to cancel the request
2. `c` is the content ID of the block you're requesting

### Get Several Blocks Asynchronously

```golang
var cids []cid.Cid
var ctx context.Context
var exchange bitswap.Bitswap

blockChannel, err := exchange.GetBlocks(ctx, cids)
```

Parameter Notes:

1. `ctx` is the context for this request, which can be canceled to cancel the request
2. `cids` is a slice of content IDs for the blocks you're requesting

### Get Related Blocks Faster With Sessions

In IPFS, content blocks are often connected to each other through a MerkleDAG. If you know ahead of time that block requests are related, Bitswap can make several optimizations internally in how it requests those blocks in order to get them faster. Bitswap provides a mechanism called a Bitswap Session to manage a series of block requests as part of a single higher level operation. You should initialize a Bitswap Session any time you intend to make a series of block requests that are related -- and whose responses are likely to come from the same peers.

```golang
var ctx context.Context
var cids []cids.cid
var exchange bitswap.Bitswap

session := exchange.NewSession(ctx)
blocksChannel, err := session.GetBlocks(ctx, cids)
// later
var relatedCids []cids.cid
relatedBlocksChannel, err := session.GetBlocks(ctx, relatedCids)
```

Note that `NewSession` returns an interface with `GetBlock` and `GetBlocks` methods that have the same signature as the overall Bitswap exchange.

### Tell bitswap a new block was added to the local datastore

```golang
var blk blocks.Block
var exchange bitswap.Bitswap

err := exchange.HasBlock(blk)
```
