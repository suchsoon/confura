package mysql

import (
	"github.com/conflux-chain/conflux-infura/store"
	"github.com/conflux-chain/conflux-infura/util"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

const (
	// entity name for block number partitioned logs
	bnPartitionedLogEntity = "logs"
	// volume size per log partition
	bnPartitionedLogVolumeSize = 10_000_000
)

type logV2 struct {
	ID          uint64
	ContractID  uint64 `gorm:"column:cid;size:64;not null"`
	BlockNumber uint64 `gorm:"column:bn;not null;index:idx_bn"`
	Epoch       uint64 `gorm:"not null"`
	Topic0      string `gorm:"size:66;not null"`
	Topic1      string `gorm:"size:66"`
	Topic2      string `gorm:"size:66"`
	Topic3      string `gorm:"size:66"`
	LogIndex    uint64 `gorm:"not null"`
	Extra       []byte `gorm:"type:mediumText"` // extra data in JSON format
}

func (logV2) TableName() string {
	return "logs_v2"
}

type logStoreV2 struct {
	*bnPartitionedStore
	cs   *ContractStore
	ebms *epochBlockMapStore
}

func newLogStoreV2(db *gorm.DB, cs *ContractStore, ebms *epochBlockMapStore) *logStoreV2 {
	return &logStoreV2{
		bnPartitionedStore: newBnPartitionedStore(db), cs: cs, ebms: ebms,
	}
}

// preparePartition create new log partitions if necessary.
func (ls *logStoreV2) preparePartition(dataSlice []*store.EpochData) (bnPartition, error) {
	partition, ok, err := ls.latestPartition(bnPartitionedLogEntity)
	if err != nil {
		return bnPartition{}, errors.WithMessage(err, "failed to get latest log partition")
	}

	// if no partition exists or partition oversizes capacity, create a new partition
	if !ok || partition.Count >= bnPartitionedLogVolumeSize {
		newPartition, err := ls.growPartition(bnPartitionedLogEntity, &logV2{})
		if err != nil {
			return bnPartition{}, errors.WithMessage(err, "failed to grow log partition")
		}

		logrus.WithFields(logrus.Fields{
			"entity":       bnPartitionedLogEntity,
			"partition":    partition,
			"newPartition": newPartition,
		}).Debug("created new log partition")

		return *newPartition, nil
	}

	return *partition, nil
}

func (ls *logStoreV2) Pushn(dbTx *gorm.DB, dataSlice []*store.EpochData, logPartition bnPartition) error {
	// containers to collect event logs for batch inserting
	var logs []*logV2

	for _, data := range dataSlice {
		for _, block := range data.Blocks {
			bn := block.BlockNumber.ToInt().Uint64()

			for _, tx := range block.Transactions {
				receipt := data.Receipts[tx.Hash]

				// Skip transactions that unexecuted in block.
				// !!! Still need to check BlockHash and Status in case more than one transactions
				// of the same hash appeared in the same epoch.
				if receipt == nil || !util.IsTxExecutedInBlock(&tx) {
					continue
				}

				var rcptExt *store.ReceiptExtra
				if len(data.ReceiptExts) > 0 {
					rcptExt = data.ReceiptExts[tx.Hash]
				}

				for k, log := range receipt.Logs {
					contract, _, err := ls.cs.AddContractIfAbsent(log.Address.MustGetBase32Address())
					if err != nil {
						return errors.WithMessage(err, "failed to add contract")
					}

					var logExt *store.LogExtra
					if rcptExt != nil && k < len(rcptExt.LogExts) {
						logExt = rcptExt.LogExts[k]
					}

					log := store.ParseCfxLog(&log, contract.ID, bn, logExt)
					logs = append(logs, (*logV2)(log))
				}
			}
		}
	}

	if len(logs) == 0 {
		return nil
	}

	tblName := ls.getPartitionedTableName(&logV2{}, logPartition.Index)
	err := dbTx.Table(tblName).CreateInBatches(logs, defaultBatchSizeLogInsert).Error
	if err != nil {
		return err
	}

	// update block range for log partition router
	bnMin := dataSlice[0].Blocks[0].BlockNumber.ToInt().Uint64()
	bnMax := dataSlice[len(dataSlice)-1].GetPivotBlock().BlockNumber.ToInt().Uint64()

	err = ls.expandBnRange(dbTx, bnPartitionedLogEntity, int(logPartition.Index), bnMin, bnMax)
	if err != nil {
		return errors.WithMessage(err, "failed to expand partition bn range")
	}

	// update partition data size
	err = ls.deltaUpdateCount(dbTx, bnPartitionedLogEntity, int(logPartition.Index), len(logs))
	if err != nil {
		return errors.WithMessage(err, "failed to delta update partition size")
	}

	return nil
}

// Popn pops event logs until the specific epoch from db store.
func (ls *logStoreV2) Popn(dbTx *gorm.DB, epochUntil uint64) error {
	bn, ok, err := ls.ebms.BlockRange(epochUntil)
	if err != nil {
		return errors.WithMessagef(err, "failed to get block mapping for epoch %v", epochUntil)
	}

	if !ok { // no block mapping found for epoch
		return errors.Errorf("no block mapping found for epoch %v", epochUntil)
	}

	// update block range for log partition router
	partitions, err := ls.shrinkBnRange(dbTx, bnPartitionedLogEntity, bn.From)
	if err != nil {
		return errors.WithMessage(err, "failed to shrink partition bn range")
	}

	for i := len(partitions) - 1; i >= 0; i-- {
		partition := partitions[i]
		tblName := ls.getPartitionedTableName(&logV2{}, partition.Index)

		res := dbTx.Table(tblName).Where("bn >= ?", bn.From).Delete(logV2{})
		if res.Error != nil {
			return res.Error
		}

		// update partition data size
		err = ls.deltaUpdateCount(dbTx, bnPartitionedLogEntity, int(partition.Index), -int(res.RowsAffected))
		if err != nil {
			return errors.WithMessage(err, "failed to delta update partition size")
		}
	}

	return nil
}