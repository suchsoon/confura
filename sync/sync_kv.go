package sync

import (
	"context"
	"sync"
	"time"

	sdk "github.com/Conflux-Chain/go-conflux-sdk"
	"github.com/Conflux-Chain/go-conflux-sdk/types"
	"github.com/conflux-chain/conflux-infura/metrics"
	"github.com/conflux-chain/conflux-infura/store"
	gometrics "github.com/ethereum/go-ethereum/metrics"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

const (
	// The threshold gap between the latest epoch and some epoch after which the epochs are regarded as decayed.
	decayedEpochGapThreshold = 100000
)

// KVCacheSyncer is used to sync blockchain data into kv cache against the latest state epoch.
type KVCacheSyncer struct {
	cfx   sdk.ClientOperator
	cache store.CacheStore

	syncIntervalNormal  time.Duration // interval to sync epoch data in normal status
	syncIntervalCatchUp time.Duration // interval to sync epoch data in catching up mode

	maxSyncEpochs uint64       // maximum number of epochs to sync once
	syncWindow    *epochWindow // epoch sync window on which the sync polling depends

	subEpochCh   chan uint64 // receive the epoch from pub/sub to detect pivot chain switch or to update epoch sync window
	checkPointCh chan bool   // checkpoint channel received to check epoch data
}

// NewKVCacheSyncer creates an instance of KVCacheSyncer to sync latest state epoch data.
func NewKVCacheSyncer(cfx sdk.ClientOperator, cache store.CacheStore) *KVCacheSyncer {
	syncer := &KVCacheSyncer{
		cfx:   cfx,
		cache: cache,

		syncIntervalNormal:  time.Millisecond * 500,
		syncIntervalCatchUp: time.Millisecond * 100,

		maxSyncEpochs: viper.GetUint64("sync.maxEpochs"),
		syncWindow:    newEpochWindow(decayedEpochGapThreshold),

		subEpochCh:   make(chan uint64, viper.GetInt64("sync.sub.buffer")),
		checkPointCh: make(chan bool, 2),
	}

	// Ensure epoch data not reverted
	if err := ensureStoreEpochDataOk(cfx, cache); err != nil {
		logrus.WithError(err).Fatal("Cache syncer failed to ensure latest state epoch data not reverted")
	}

	// Load last sync epoch information
	syncer.mustLoadLastSyncEpoch()

	return syncer
}

// Sync starts to sync epoch data from blockchain with specified cfx instance.
func (syncer *KVCacheSyncer) Sync(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()

	logger := logrus.WithField("syncWindow", syncer.syncWindow)
	logger.Info("Cache syncer starting to sync epoch data...")

	ticker := time.NewTicker(syncer.syncIntervalCatchUp)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Cache syncer shutdown ok")
			return
		case <-syncer.checkPointCh:
			if err := syncer.doCheckPoint(); err != nil {
				logger.WithError(err).Error("Cache syncer failed to do sync checkpoint")
			}
		case newEpoch := <-syncer.subEpochCh:
			syncer.handleNewEpoch(newEpoch)
		case <-ticker.C:
			if err := syncer.doTicker(ticker); err != nil {
				logger.WithError(err).Error("Cache syncer failed to sync epoch data")
			}
		}
	}
}

// Load last sync epoch from cache store to continue synchronization
func (syncer *KVCacheSyncer) mustLoadLastSyncEpoch() {
	_, maxEpoch, err := syncer.cache.GetGlobalEpochRange()
	if err == nil {
		syncer.syncWindow.reset(maxEpoch+1, maxEpoch)
	} else if !syncer.cache.IsRecordNotFound(err) {
		logrus.WithError(err).Fatal("Cache syncer failed to get global epoch range from cache store")
	}
}

// Do epoch data checking for synchronized epoch data in cache
func (syncer *KVCacheSyncer) doCheckPoint() error {
	logrus.Debug("Cache syncer doing checkpoint...")

	return ensureStoreEpochDataOk(syncer.cfx, syncer.cache)
}

// Revert the epoch data in cache store until to some epoch
func (syncer *KVCacheSyncer) pivotSwitchRevert(revertTo uint64) error {
	logrus.WithFields(logrus.Fields{
		"revertTo": revertTo, "syncWindow": syncer.syncWindow,
	}).Debug("Reverting epoch data in cache due to pivot switch...")

	return syncer.cache.Popn(revertTo)
}

// Handle new epoch received to detect pivot switch or update epoch sync window
func (syncer *KVCacheSyncer) handleNewEpoch(newEpoch uint64) {
	logger := logrus.WithFields(logrus.Fields{"newEpoch": newEpoch, "syncWindow": syncer.syncWindow})

	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		logger.Debug("Cache syncer handling new epoch received...")
		defer logger.Debug("Cache syncer new received epoch handled")
	}

	// Peek if pivot switch will happen with the new epoch received
	if syncer.syncWindow.peekWillPivotSwitch(newEpoch) {
		logger.Debug("Cache syncer pivot switch detected")

		if err := syncer.pivotSwitchRevert(newEpoch); err == nil {
			syncer.syncWindow.expandFrom(newEpoch)
		} else {
			logger.WithError(err).Error("Failed to remove epoch data in cache due to pivot switch")
		}

		return
	}

	// Peek if overflow will happen with the new epoch received
	if syncer.syncWindow.peekWillOverflow(newEpoch) {
		logger.Debug("Cache syncer sync window overflow detected")

		if err := syncer.cache.Flush(); err == nil { // flush all decayed old data in cache store
			syncer.syncWindow.reset(newEpoch, newEpoch)
		} else {
			logger.WithError(err).Error("Failed to flush decayed data in store due to sync window overflow")
		}

		return
	}

	// Expand the sync window to the new epoch received
	syncer.syncWindow.expandTo(newEpoch)
}

// Ticker to catch up or sync epoch data
func (syncer *KVCacheSyncer) doTicker(ticker *time.Ticker) error {
	logrus.Debug(">>> Cache syncer ticking...")

	if complete, err := syncer.syncOnce(); err != nil {
		ticker.Reset(syncer.syncIntervalNormal)
		return err
	} else if complete {
		ticker.Reset(syncer.syncIntervalNormal)
	} else {
		ticker.Reset(syncer.syncIntervalCatchUp)
	}

	return nil
}

// Sync data once and return true if sync window is consumed to be empty, otherwise false.
func (syncer *KVCacheSyncer) syncOnce() (bool, error) {
	logger := logrus.WithField("syncWindow", syncer.syncWindow)

	updater := metrics.NewTimerUpdaterByName("infura/cache/sync/once")
	defer updater.Update()

	if syncer.syncWindow.isEmpty() {
		logger.Debug("Cache syncer syncOnce skipped with epoch sync window empty")
		return true, nil
	}

	syncFrom, syncSize := syncer.syncWindow.peekShrinkFrom(uint32(syncer.maxSyncEpochs))

	logger = logger.WithFields(logrus.Fields{"syncFrom": syncFrom, "syncSize": syncSize})
	logger.Debug("Cache syncer starting to sync epoch(s)...")

	syncSizeGauge := gometrics.GetOrRegisterGauge("infura/cache/sync/epoch/size/stated", nil)
	syncSizeGauge.Update(int64(syncSize))

	epochDataSlice := make([]*store.EpochData, 0, syncSize)
	for i := uint32(0); i < syncSize; i++ {
		epochNo := syncFrom + uint64(i)

		data, err := store.QueryEpochData(syncer.cfx, epochNo)
		if err != nil {
			logger.WithError(err).WithField("epoch", epochNo).Error("Cache syncer failed to query epoch data")
			return false, errors.WithMessagef(err, "failed to query epoch data for epoch %v", epochNo)
		}

		logrus.WithField("epoch", epochNo).Debug("Cache syncer succeeded to query epoch data")
		epochDataSlice = append(epochDataSlice, &data)
	}

	if err := syncer.cache.Pushn(epochDataSlice); err != nil {
		logger.WithError(err).Error("Cache syncer failed to write epoch data to cache store")
		return false, errors.WithMessage(err, "failed to write epoch data to cache store")
	}

	syncFrom, syncSize = syncer.syncWindow.shrinkFrom(uint32(syncer.maxSyncEpochs))
	logrus.WithFields(logrus.Fields{"syncFrom": syncFrom, "syncSize": syncSize}).Trace("Cache syncer succeeded to sync epoch data range")

	return syncer.syncWindow.isEmpty(), nil
}

// implement the EpochSubscriber interface.
func (syncer *KVCacheSyncer) onEpochReceived(epoch types.WebsocketEpochResponse) {
	epochNo := epoch.EpochNumber.ToInt().Uint64()

	logrus.WithField("epoch", epochNo).Debug("Cache syncer onEpochReceived new epoch received")
	syncer.subEpochCh <- epochNo
}

func (syncer *KVCacheSyncer) onEpochSubStart() {
	logrus.Debug("Cache syncer onEpochSubStart event received")

	syncer.checkPointCh <- true
}