package node

import (
	"fmt"
	"github.com/perlin-network/noise"
	"github.com/perlin-network/noise/protocol"
	"github.com/perlin-network/wavelet"
	"github.com/perlin-network/wavelet/log"
	"github.com/perlin-network/wavelet/store"
	"github.com/perlin-network/wavelet/sys"
	"github.com/pkg/errors"
	"golang.org/x/crypto/blake2b"
)

var _ protocol.Block = (*block)(nil)

const (
	keyLedger      = "wavelet.ledger"
	keyBroadcaster = "wavelet.broadcaster"
	keySyncer      = "wavelet.syncer"
	keyAuthChannel = "wavelet.auth.ch"

	syncChunkSize = 1048576 // change this to a smaller value for debugging.
)

type block struct {
	opcodeGossipRequest  noise.Opcode
	opcodeGossipResponse noise.Opcode

	opcodeQueryRequest  noise.Opcode
	opcodeQueryResponse noise.Opcode

	opcodeSyncViewRequest  noise.Opcode
	opcodeSyncViewResponse noise.Opcode

	opcodeSyncDiffMetadataRequest  noise.Opcode
	opcodeSyncDiffMetadataResponse noise.Opcode
	opcodeSyncDiffChunkRequest     noise.Opcode
	opcodeSyncDiffChunkResponse    noise.Opcode

	opcodeSyncTransactionRequest  noise.Opcode
	opcodeSyncTransactionResponse noise.Opcode
}

func New() *block {
	return &block{}
}

func (b *block) OnRegister(p *protocol.Protocol, node *noise.Node) {
	b.opcodeGossipRequest = noise.RegisterMessage(noise.NextAvailableOpcode(), (*GossipRequest)(nil))
	b.opcodeGossipResponse = noise.RegisterMessage(noise.NextAvailableOpcode(), (*GossipResponse)(nil))
	b.opcodeQueryRequest = noise.RegisterMessage(noise.NextAvailableOpcode(), (*QueryRequest)(nil))
	b.opcodeQueryResponse = noise.RegisterMessage(noise.NextAvailableOpcode(), (*QueryResponse)(nil))
	b.opcodeSyncViewRequest = noise.RegisterMessage(noise.NextAvailableOpcode(), (*SyncViewRequest)(nil))
	b.opcodeSyncViewResponse = noise.RegisterMessage(noise.NextAvailableOpcode(), (*SyncViewResponse)(nil))
	b.opcodeSyncDiffMetadataRequest = noise.RegisterMessage(noise.NextAvailableOpcode(), (*SyncDiffMetadataRequest)(nil))
	b.opcodeSyncDiffMetadataResponse = noise.RegisterMessage(noise.NextAvailableOpcode(), (*SyncDiffMetadataResponse)(nil))
	b.opcodeSyncDiffChunkRequest = noise.RegisterMessage(noise.NextAvailableOpcode(), (*SyncDiffChunkRequest)(nil))
	b.opcodeSyncDiffChunkResponse = noise.RegisterMessage(noise.NextAvailableOpcode(), (*SyncDiffChunkResponse)(nil))
	b.opcodeSyncTransactionRequest = noise.RegisterMessage(noise.NextAvailableOpcode(), (*SyncTransactionRequest)(nil))
	b.opcodeSyncTransactionResponse = noise.RegisterMessage(noise.NextAvailableOpcode(), (*SyncTransactionResponse)(nil))

	genesisPath := "config/genesis.json"

	kv := store.NewInmem()

	ledger := wavelet.NewLedger(kv, genesisPath)
	ledger.RegisterProcessor(sys.TagNop, new(wavelet.NopProcessor))
	ledger.RegisterProcessor(sys.TagTransfer, new(wavelet.TransferProcessor))
	ledger.RegisterProcessor(sys.TagContract, new(wavelet.ContractProcessor))
	ledger.RegisterProcessor(sys.TagStake, new(wavelet.StakeProcessor))

	node.Set(keyLedger, ledger)

	broadcaster := newBroadcaster(node)
	broadcaster.init()

	node.Set(keyBroadcaster, broadcaster)

	syncer := newSyncer(node)
	//syncer.init()

	node.Set(keySyncer, syncer)
}

func (b *block) OnBegin(p *protocol.Protocol, peer *noise.Peer) error {
	go b.receiveLoop(Ledger(peer.Node()), peer)

	close(peer.LoadOrStore(keyAuthChannel, make(chan struct{})).(chan struct{}))

	return nil
}

func (b *block) receiveLoop(ledger *wavelet.Ledger, peer *noise.Peer) {
	chunkCache := newLRU(1024) // 1024 * 4MB

	for {
		select {
		case req := <-peer.Receive(b.opcodeGossipRequest):
			go handleGossipRequest(ledger, peer, req.(GossipRequest))
		case req := <-peer.Receive(b.opcodeQueryRequest):
			go handleQueryRequest(ledger, peer, req.(QueryRequest))
		case req := <-peer.Receive(b.opcodeSyncViewRequest):
			go handleSyncViewRequest(ledger, peer, req.(SyncViewRequest))
		case req := <-peer.Receive(b.opcodeSyncDiffMetadataRequest):
			go handleSyncDiffMetadataRequest(ledger, peer, req.(SyncDiffMetadataRequest), chunkCache)
		case req := <-peer.Receive(b.opcodeSyncDiffChunkRequest):
			go handleSyncDiffChunkRequest(ledger, peer, req.(SyncDiffChunkRequest), chunkCache)
		case req := <-peer.Receive(b.opcodeSyncTransactionRequest):
			go handleSyncTransactionRequest(ledger, peer, req.(SyncTransactionRequest))
		}
	}
}

func handleSyncTransactionRequest(ledger *wavelet.Ledger, peer *noise.Peer, req SyncTransactionRequest) {
	res := new(SyncTransactionResponse)
	defer func() {
		if err := <-peer.SendMessageAsync(res); err != nil {
			_ = peer.DisconnectAsync()
		}
	}()

	for _, id := range req.ids {
		tx, ok := ledger.FindTransaction(id)

		if !ok {
			continue
		}

		res.transactions = append(res.transactions, tx)
	}

	logger := log.Sync("tx_req")
	logger.Debug().
		Int("num_tx", len(req.ids)).
		Msg("Responded to request for transactions data.")
}

func handleSyncDiffMetadataRequest(ledger *wavelet.Ledger, peer *noise.Peer, req SyncDiffMetadataRequest, chunkCache *lru) {
	diff := ledger.Accounts.DumpDiff(req.viewID)

	var chunkHashes [][blake2b.Size256]byte

	for i := 0; i < len(diff); i += syncChunkSize {
		end := i + syncChunkSize

		if end > len(diff) {
			end = len(diff)
		}

		hash := blake2b.Sum256(diff[i:end])

		chunkCache.put(hash, diff[i:end])
		chunkHashes = append(chunkHashes, hash)
	}

	if err := <-peer.SendMessageAsync(&SyncDiffMetadataResponse{
		latestViewID: ledger.ViewID(),
		chunkHashes:  chunkHashes,
	}); err != nil {
		_ = peer.DisconnectAsync()
	}
}

func handleSyncDiffChunkRequest(ledger *wavelet.Ledger, peer *noise.Peer, req SyncDiffChunkRequest, chunkCache *lru) {
	var res SyncDiffChunkResponse

	if chunk, found := chunkCache.load(req.chunkHash); found {
		res.diff = chunk.([]byte)
		res.found = true

		providedHash := blake2b.Sum256(res.diff)

		logger := log.Node()
		logger.Info().
			Hex("requested_hash", req.chunkHash[:]).
			Hex("provided_hash", providedHash[:]).
			Msg("Responded to sync chunk request.")
	}

	if err := <-peer.SendMessageAsync(&res); err != nil {
		_ = peer.DisconnectAsync()
	}
}

func handleSyncViewRequest(ledger *wavelet.Ledger, peer *noise.Peer, req SyncViewRequest) {
	res := new(SyncViewResponse)
	defer func() {
		if err := <-peer.SendMessageAsync(res); err != nil {
			_ = peer.DisconnectAsync()
		}
	}()

	syncer := Syncer(peer.Node())

	if preferred := syncer.resolver.Preferred(); preferred == nil {
		res.root = ledger.Root()
	} else {
		res.root = preferred
	}

	if err := wavelet.AssertValidTransaction(req.root); err != nil {
		return
	}

	if ledger.ViewID() < req.root.ViewID && syncer.resolver.Preferred() == nil {
		res.root = req.root
		syncer.resolver.Prefer(req.root)
	}

	syncer.recordRootFromAccount(protocol.PeerID(peer), req.root.ID)
}

func handleQueryRequest(ledger *wavelet.Ledger, peer *noise.Peer, req QueryRequest) {
	res := new(QueryResponse)
	defer func() {
		//if res.preferred != nil {
		//	logger := log.Consensus("query")
		//	logger.Debug().
		//		Hex("preferred_id", res.preferred.ID[:]).
		//		Uint64("view_id", ledger.ViewID()).
		//		Msg("Responded to finality query with our preferred transaction.")
		//}

		if err := <-peer.SendMessageAsync(res); err != nil {
			_ = peer.DisconnectAsync()
		}
	}()

	if Broadcaster(peer.Node()).Paused.Load() {
		return
	}

	if req.tx.ViewID == ledger.ViewID()-1 {
		res.preferred = ledger.Root()
	} else if preferred := ledger.Resolver().Preferred(); preferred != nil {
		res.preferred = preferred
	}

	if err := ledger.ReceiveTransaction(req.tx); errors.Cause(err) == wavelet.VoteAccepted {
		res.preferred = req.tx
	}
}

func handleGossipRequest(ledger *wavelet.Ledger, peer *noise.Peer, req GossipRequest) {
	fmt.Println("GOT")
	res := new(GossipResponse)
	defer func() {
		if err := <-peer.SendMessageAsync(res); err != nil {
			_ = peer.DisconnectAsync()
		}
	}()

	if Broadcaster(peer.Node()).Paused.Load() {
		return
	}

	//_, seen := ledger.FindTransaction(req.TX.ID)

	vote := ledger.ReceiveTransaction(req.TX)
	res.vote = errors.Cause(vote) == wavelet.VoteAccepted

	if logger := log.Consensus("vote"); !res.vote {
		//	logger.Debug().Hex("tx_id", req.tx.ID[:]).Msg("Gave a positive vote to a transaction.")
		//} else {
		logger.Warn().Hex("tx_id", req.TX.ID[:]).Err(vote).Msg("Gave a negative vote to a transaction.")
	}

	// Gossip transaction out to our peers just once. Ignore any errors if gossiping out fails.
	//if !seen && res.vote {
	//	go func() {
	//		_ = BroadcastTransaction(peer.Node(), req.TX)
	//	}()
	//}
}

func (b *block) OnEnd(p *protocol.Protocol, peer *noise.Peer) error {
	return nil
}

func WaitUntilAuthenticated(peer *noise.Peer) {
	<-peer.LoadOrStore(keyAuthChannel, make(chan struct{})).(chan struct{})
}

func BroadcastTransaction(node *noise.Node, tx *wavelet.Transaction) error {
	return Broadcaster(node).Broadcast(tx)
}

func Ledger(node *noise.Node) *wavelet.Ledger {
	return node.Get(keyLedger).(*wavelet.Ledger)
}

func Broadcaster(node *noise.Node) *broadcaster {
	return node.Get(keyBroadcaster).(*broadcaster)
}

func Syncer(node *noise.Node) *syncer {
	return node.Get(keySyncer).(*syncer)
}
