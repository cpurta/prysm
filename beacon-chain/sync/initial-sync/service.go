// Package initialsync is run by the beacon node when the local chain is
// behind the network's longest chain. Initial sync works as follows:
// The node requests for the slot number of the most recent finalized block.
// The node then builds from the most recent finalized block by requesting for subsequent
// blocks by slot number. Once the service detects that the local chain is caught up with
// the network, the service hands over control to the regular sync service.
// Note: The behavior of initialsync will likely change as the specification changes.
// The most significant and highly probable change will be determining where to sync from.
// The beacon chain may sync from a block in the pasts X months in order to combat long-range attacks
// (see here: https://github.com/ethereum/wiki/wiki/Proof-of-Stake-FAQs#what-is-weak-subjectivity)
package initialsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/prysmaticlabs/prysm/beacon-chain/types"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/event"
	"github.com/prysmaticlabs/prysm/shared/p2p"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("prefix", "initial-sync")

// Config defines the configurable properties of InitialSync.
//
type Config struct {
	SyncPollingInterval         time.Duration
	BlockBufferSize             int
	BlockAnnounceBufferSize     int
	CrystallizedStateBufferSize int
	BeaconDB                    beaconDB
	P2P                         p2pAPI
	SyncService                 syncService
	QueryService                queryService
}

// DefaultConfig provides the default configuration for a sync service.
// SyncPollingInterval determines how frequently the service checks that initial sync is complete.
// BlockBufferSize determines that buffer size of the `blockBuf` channel.
// CrystallizedStateBufferSize determines the buffer size of thhe `crystallizedStateBuf` channel.
func DefaultConfig() Config {
	return Config{
		SyncPollingInterval:         time.Duration(params.BeaconConfig().SyncPollingInterval) * time.Second,
		BlockBufferSize:             100,
		BlockAnnounceBufferSize:     100,
		CrystallizedStateBufferSize: 100,
	}
}

type p2pAPI interface {
	Subscribe(msg proto.Message, channel chan p2p.Message) event.Subscription
	Send(msg proto.Message, peer p2p.Peer)
	Broadcast(msg proto.Message)
}

type beaconDB interface {
	SaveBlock(*types.Block) error
	SaveCrystallizedState(*types.CrystallizedState) error
}

// SyncService is the interface for the Sync service.
// InitialSync calls `Start` when initial sync completes.
type syncService interface {
	Start()
	ResumeSync()
	IsSyncedWithNetwork() bool
}

type queryService interface {
	IsSynced() (bool, error)
}

// InitialSync defines the main class in this package.
// See the package comments for a general description of the service's functions.
type InitialSync struct {
	ctx                          context.Context
	cancel                       context.CancelFunc
	p2p                          p2pAPI
	syncService                  syncService
	queryService                 queryService
	db                           beaconDB
	blockAnnounceBuf             chan p2p.Message
	blockBuf                     chan p2p.Message
	crystallizedStateBuf         chan p2p.Message
	currentSlot                  uint64
	highestObservedSlot          uint64
	syncPollingInterval          time.Duration
	initialCrystallizedStateRoot [32]byte
	inMemoryBlocks               map[uint64]*pb.BeaconBlockResponse
}

// NewInitialSyncService constructs a new InitialSyncService.
// This method is normally called by the main node.
func NewInitialSyncService(ctx context.Context,
	cfg Config,
) *InitialSync {
	ctx, cancel := context.WithCancel(ctx)

	blockBuf := make(chan p2p.Message, cfg.BlockBufferSize)
	crystallizedStateBuf := make(chan p2p.Message, cfg.CrystallizedStateBufferSize)
	blockAnnounceBuf := make(chan p2p.Message, cfg.BlockAnnounceBufferSize)

	return &InitialSync{
		ctx:                  ctx,
		cancel:               cancel,
		p2p:                  cfg.P2P,
		syncService:          cfg.SyncService,
		db:                   cfg.BeaconDB,
		currentSlot:          0,
		highestObservedSlot:  0,
		blockBuf:             blockBuf,
		crystallizedStateBuf: crystallizedStateBuf,
		blockAnnounceBuf:     blockAnnounceBuf,
		syncPollingInterval:  cfg.SyncPollingInterval,
		inMemoryBlocks:       map[uint64]*pb.BeaconBlockResponse{},
		queryService:         cfg.QueryService,
	}
}

// Start begins the goroutine.
func (s *InitialSync) Start() {
	synced, err := s.queryService.IsSynced()
	if err != nil {
		log.Error(err)
	}

	if synced {
		// TODO(#661): Bail out of the sync service if the chain is only partially synced.
		log.Info("Chain fully synced, exiting initial sync")
		return
	}

	go func() {
		ticker := time.NewTicker(s.syncPollingInterval)
		s.run(ticker.C)
		ticker.Stop()
	}()
}

// Stop kills the initial sync goroutine.
func (s *InitialSync) Stop() error {
	log.Info("Stopping service")
	s.cancel()
	return nil
}

// run is the main goroutine for the initial sync service.
// delayChan is explicitly passed into this function to facilitate tests that don't require a timeout.
// It is assumed that the goroutine `run` is only called once per instance.
func (s *InitialSync) run(delaychan <-chan time.Time) {

	blockSub := s.p2p.Subscribe(&pb.BeaconBlockResponse{}, s.blockBuf)
	blockAnnounceSub := s.p2p.Subscribe(&pb.BeaconBlockAnnounce{}, s.blockAnnounceBuf)
	crystallizedStateSub := s.p2p.Subscribe(&pb.CrystallizedStateResponse{}, s.crystallizedStateBuf)
	defer func() {
		blockSub.Unsubscribe()
		blockAnnounceSub.Unsubscribe()
		crystallizedStateSub.Unsubscribe()
		close(s.blockBuf)
		close(s.crystallizedStateBuf)
	}()

	for {
		select {
		case <-s.ctx.Done():
			log.Debug("Exiting goroutine")
			return
		case <-delaychan:
			if s.currentSlot == 0 {
				continue
			}
			if s.highestObservedSlot == s.currentSlot {
				log.Info("Exiting initial sync and starting normal sync")
				s.syncService.ResumeSync()
				// TODO(#661): Resume sync after completion of initial sync.
				return
			}

			// requests multiple blocks so as to save and sync quickly.
			s.requestBatchedBlocks(s.highestObservedSlot)
		case msg := <-s.blockAnnounceBuf:
			data := msg.Data.(*pb.BeaconBlockAnnounce)

			if data.GetSlotNumber() > s.highestObservedSlot {
				s.highestObservedSlot = data.GetSlotNumber()
			}

			s.requestBatchedBlocks(s.highestObservedSlot)
			log.Debugf("Successfully requested the next block with slot: %d", data.GetSlotNumber())
		case msg := <-s.blockBuf:
			data := msg.Data.(*pb.BeaconBlockResponse)

			if data.Block.GetSlot() > s.highestObservedSlot {
				s.highestObservedSlot = data.Block.GetSlot()
			}

			if s.currentSlot == 0 {
				if s.initialCrystallizedStateRoot != [32]byte{} {
					continue
				}
				if data.GetBlock().GetSlot() != 1 {

					// saves block in memory if it isn't the initial block.
					if _, ok := s.inMemoryBlocks[data.Block.GetSlot()]; !ok {
						s.inMemoryBlocks[data.Block.GetSlot()] = data
					}
					s.requestNextBlockBySlot(1)
					continue
				}
				if err := s.setBlockForInitialSync(data); err != nil {
					log.Errorf("Could not set block for initial sync: %v", err)
				}
				if err := s.requestCrystallizedStateFromPeer(data, msg.Peer); err != nil {
					log.Errorf("Could not request crystallized state from peer: %v", err)
				}

				continue
			}
			// if it isn't the block in the next slot it saves it in memory.
			if data.Block.GetSlot() != (s.currentSlot + 1) {
				if _, ok := s.inMemoryBlocks[data.Block.GetSlot()]; !ok {
					s.inMemoryBlocks[data.Block.GetSlot()] = data
				}
				continue
			}

			if err := s.validateAndSaveNextBlock(data); err != nil {
				log.Errorf("Unable to save block: %v", err)
			}
			s.requestNextBlockBySlot(s.currentSlot + 1)
		case msg := <-s.crystallizedStateBuf:
			data := msg.Data.(*pb.CrystallizedStateResponse)

			if s.initialCrystallizedStateRoot == [32]byte{} {
				continue
			}

			cState := types.NewCrystallizedState(data.CrystallizedState)
			hash, err := cState.Hash()
			if err != nil {
				log.Errorf("Unable to hash crytsallized state: %v", err)
			}

			if hash != s.initialCrystallizedStateRoot {
				continue
			}

			if err := s.db.SaveCrystallizedState(cState); err != nil {
				log.Errorf("Unable to set crystallized state for initial sync %v", err)
			}

			log.Debug("Successfully saved crystallized state to the db")

			if s.currentSlot >= cState.LastFinalizedSlot() {
				continue
			}

			// sets the current slot to the last finalized slot of the
			// crystallized state to begin our sync from.
			s.currentSlot = cState.LastFinalizedSlot()
			log.Debugf("Successfully saved crystallized state with the last finalized slot: %d", cState.LastFinalizedSlot())

			s.requestNextBlockBySlot(s.currentSlot + 1)
			crystallizedStateSub.Unsubscribe()
		}
	}
}

// requestCrystallizedStateFromPeer sends a request to a peer for the corresponding crystallized state
// for a beacon block.
func (s *InitialSync) requestCrystallizedStateFromPeer(data *pb.BeaconBlockResponse, peer p2p.Peer) error {
	block := types.NewBlock(data.Block)
	h := block.CrystallizedStateRoot()
	log.Debugf("Successfully processed incoming block with crystallized state hash: %#x", h)
	s.p2p.Send(&pb.CrystallizedStateRequest{Hash: h[:]}, peer)
	return nil
}

// setBlockForInitialSync sets the first received block as the base finalized
// block for initial sync.
func (s *InitialSync) setBlockForInitialSync(data *pb.BeaconBlockResponse) error {
	block := types.NewBlock(data.Block)

	h, err := block.Hash()
	if err != nil {
		return err
	}
	log.WithField("blockhash", fmt.Sprintf("%#x", h)).Debug("Crystallized state hash exists locally")

	if err := s.writeBlockToDB(block); err != nil {
		return err
	}

	s.initialCrystallizedStateRoot = block.CrystallizedStateRoot()

	log.Infof("Saved block with hash %#x for initial sync", h)
	s.currentSlot = block.SlotNumber()
	s.requestNextBlockBySlot(s.currentSlot + 1)
	return nil
}

// requestNextBlock broadcasts a request for a block with the entered slotnumber.
func (s *InitialSync) requestNextBlockBySlot(slotnumber uint64) {
	log.Debugf("Requesting block %d ", slotnumber)
	if _, ok := s.inMemoryBlocks[slotnumber]; ok {
		s.blockBuf <- p2p.Message{
			Data: s.inMemoryBlocks[slotnumber],
		}
		return
	}
	s.p2p.Broadcast(&pb.BeaconBlockRequestBySlotNumber{SlotNumber: slotnumber})
}

// requestBatchedBlocks sends out multiple requests for blocks till a
// specified bound slot number.
func (s *InitialSync) requestBatchedBlocks(endSlot uint64) {
	log.Debug("Requesting batched blocks")
	for i := s.currentSlot + 1; i <= endSlot; i++ {
		s.requestNextBlockBySlot(i)
	}
}

// validateAndSaveNextBlock will validate whether blocks received from the blockfetcher
// routine can be added to the chain.
func (s *InitialSync) validateAndSaveNextBlock(data *pb.BeaconBlockResponse) error {
	block := types.NewBlock(data.Block)
	h, err := block.Hash()
	if err != nil {
		return err
	}

	if s.currentSlot == uint64(0) {
		return errors.New("invalid slot number for syncing")
	}

	if (s.currentSlot + 1) == block.SlotNumber() {

		if err := s.writeBlockToDB(block); err != nil {
			return err
		}

		log.Infof("Saved block with hash %#x and slot %d for initial sync", h, block.SlotNumber())
		s.currentSlot = block.SlotNumber()

		// delete block from memory
		if _, ok := s.inMemoryBlocks[block.SlotNumber()]; ok {
			delete(s.inMemoryBlocks, block.SlotNumber())
		}
	}
	return nil
}

// writeBlockToDB saves the corresponding block to the local DB.
func (s *InitialSync) writeBlockToDB(block *types.Block) error {
	return s.db.SaveBlock(block)
}
