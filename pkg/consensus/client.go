package consensus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/protolambda/eth2api"
	"github.com/protolambda/eth2api/client/beaconapi"
	"github.com/protolambda/eth2api/client/validatorapi"
	"github.com/protolambda/zrnt/eth2/beacon/bellatrix"
	"github.com/protolambda/zrnt/eth2/beacon/common"
	"github.com/r3labs/sse/v2"
	"github.com/ralexstokes/relay-monitor/pkg/types"
	"go.uber.org/zap"
)

type ValidatorInfo struct {
	publicKey types.PublicKey
	index     types.ValidatorIndex
}

type Client struct {
	logger *zap.Logger
	client *eth2api.Eth2HttpClient
	clock  *Clock

	proposerCache      map[types.Slot]ValidatorInfo
	proposerCacheMutex sync.Mutex

	executionCache      map[types.Slot]types.Hash
	executionCacheMutex sync.Mutex
}

func NewClient(endpoint string, clock *Clock, logger *zap.Logger) *Client {
	client := &eth2api.Eth2HttpClient{
		Addr: endpoint,
		Cli: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 128,
			},
			Timeout: 5 * time.Second,
		},
		Codec: eth2api.JSONCodec{},
	}
	return &Client{
		logger:         logger,
		client:         client,
		clock:          clock,
		proposerCache:  make(map[types.Slot]ValidatorInfo),
		executionCache: make(map[types.Slot]types.Hash),
	}
}

func (c *Client) GetParentHash(slot types.Slot) (types.Hash, error) {
	c.executionCacheMutex.Lock()
	parentHash, ok := c.executionCache[slot-1]
	c.executionCacheMutex.Unlock()
	if !ok {
		// TODO fetch hash when missing
		return types.Hash{}, fmt.Errorf("missing execution hash for slot %d", slot)
	}
	return parentHash, nil
}

func (c *Client) GetProposerPublicKey(slot types.Slot) (*types.PublicKey, error) {
	c.proposerCacheMutex.Lock()
	defer c.proposerCacheMutex.Unlock()

	validator, ok := c.proposerCache[slot]
	if !ok {
		return nil, fmt.Errorf("missing proposal for slot %d", slot)
	}

	return &validator.publicKey, nil
}

func (c *Client) fetchProposers(epoch types.Epoch) error {
	ctx := context.Background()

	var proposerDuties eth2api.DependentProposerDuty
	syncing, err := validatorapi.ProposerDuties(ctx, c.client, common.Epoch(epoch), &proposerDuties)
	if syncing {
		return fmt.Errorf("could not fetch proposal duties in epoch %d because node is syncing", epoch)
	} else if err != nil {
		return err
	}

	// TODO handle reorgs, etc.
	c.proposerCacheMutex.Lock()
	for _, duty := range proposerDuties.Data {
		c.proposerCache[uint64(duty.Slot)] = ValidatorInfo{
			publicKey: types.PublicKey(duty.Pubkey),
			index:     uint64(duty.ValidatorIndex),
		}
	}
	c.proposerCacheMutex.Unlock()

	return nil
}

func (c *Client) loadData(epoch types.Epoch) error {
	err := c.fetchProposers(epoch)
	if err != nil {
		return err
	}

	return nil
}

type headEvent struct {
	Slot  string     `json:"slot"`
	Block types.Root `json:"block"`
}

func (c *Client) streamHeads() <-chan types.Coordinate {
	logger := c.logger.Sugar()

	sseClient := sse.NewClient(c.client.Addr + "/eth/v1/events?topics=head")
	ch := make(chan types.Coordinate, 1)
	go func() {
		sseClient.SubscribeRaw(func(msg *sse.Event) {
			var event headEvent
			err := json.Unmarshal(msg.Data, &event)
			if err != nil {
				logger.Warnf("could not unmarshal `head` node event: %s", err)
				return
			}
			slot, err := strconv.Atoi(event.Slot)
			if err != nil {
				logger.Warnf("could not unmarshal slot from `head` node event: %s", err)
				return
			}
			coordinate := types.Coordinate{
				Slot: types.Slot(slot),
				Root: event.Block,
			}
			ch <- coordinate
		})
	}()
	return ch
}

func (c *Client) fetchLatestExecutionHash(head *types.Coordinate) error {
	ctx := context.Background()

	blockID, err := eth2api.ParseBlockId(head.Root.String())
	if err != nil {
		return fmt.Errorf("could not parse block for head %v", head)
	}

	var signedBeaconBlock eth2api.VersionedSignedBeaconBlock
	exists, err := beaconapi.BlockV2(ctx, c.client, blockID, &signedBeaconBlock)
	if !exists {
		return fmt.Errorf("block with root %s is missing", head.Root)
	} else if err != nil {
		return err
	}

	slot := head.Slot
	bellatrixBlock, ok := signedBeaconBlock.Data.(*bellatrix.SignedBeaconBlock)
	if !ok {
		return fmt.Errorf("could not parse block %s", signedBeaconBlock)
	}
	executionHash := bellatrixBlock.Message.Body.ExecutionPayload.BlockHash

	// TODO handle reorgs, etc.
	c.executionCacheMutex.Lock()
	c.executionCache[slot] = types.Hash(executionHash)
	c.executionCacheMutex.Unlock()

	return nil
}

func (c *Client) runSlotTasks(wg *sync.WaitGroup) {
	logger := c.logger.Sugar()
	wg.Done()

	for head := range c.streamHeads() {
		err := c.fetchLatestExecutionHash(&head)
		if err != nil {
			logger.Warnf("could not load consensus state for slot %d: %s", head.Slot, err)
		}
	}
}

func (c *Client) runEpochTasks(wg *sync.WaitGroup) {
	logger := c.logger.Sugar()

	epochs := c.clock.TickEpochs()

	// load data for the current epoch
	epoch := <-epochs
	err := c.loadData(epoch)
	if err != nil {
		logger.Warnf("could not load consensus state for epoch %d: %s", epoch, err)
	}

	// load data for the next epoch, as we will typically do
	err = c.loadData(epoch + 1)
	if err != nil {
		logger.Warnf("could not load consensus state for epoch %d: %s", epoch, err)
	}
	// signal to caller that we have done the initialization...
	wg.Done()

	for epoch := range epochs {
		err := c.loadData(epoch + 1)
		if err != nil {
			logger.Warnf("could not load consensus state for epoch %d: %s", epoch, err)
		}
	}
}

func (c *Client) Run(wg *sync.WaitGroup) {
	wg.Add(1)
	go c.runSlotTasks(wg)

	wg.Add(1)
	go c.runEpochTasks(wg)

	wg.Done()
}
