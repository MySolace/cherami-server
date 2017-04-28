// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package outputhost

import (
	"errors"
	"sync"
	"time"

	"github.com/uber-common/bark"
	"github.com/uber/tchannel-go/thrift"

	"github.com/uber/cherami-server/common"
	"github.com/uber/cherami-server/common/metrics"
	"github.com/uber/cherami-thrift/.generated/go/admin"
	"github.com/uber/cherami-thrift/.generated/go/metadata"
)

const ackLevelInterval = 5 * time.Second
const metaContextTimeout = 10 * time.Second

type storeHostAddress int64

type (
	// internalMsg is the message which is stored locally on the ackMgr
	internalMsg struct {
		addr  storeHostAddress
		seq   common.SequenceNumber
		acked bool
	}

	levels struct {
		asOf          common.UnixNanoTime   // Timestamp when this backLogLevel was calculated
		readLevel     common.SequenceNumber // -1 = nothing received, 0 = 1 message (#0) received, etc.
		ackLevel      common.SequenceNumber // -1 = nothing acked
		ackLevelRate  float64               // Rate of change in ackedTo, seconds⁽⁻¹⁾
		readLevelRate float64               // Rate of change in receivedTo, seconds⁽⁻¹⁾
		ackLevelAddr  storeHostAddress      // Storehost address corresponding to ackLevel, -1 = nothing acked
		readLevelAddr storeHostAddress      // Storehost address corresponding to recievedLevel, -1 = nothing read
		lastAckedSeq  common.SequenceNumber // the latest sequence which is acked
	}

	// ackManager is held per CG extent and it holds the addresses that we get from the store.
	ackManager struct {
		addrs              map[common.SequenceNumber]*internalMsg // ‡
		sealed             bool                                   // ‡
		outputHostUUID     string
		cgUUID             string
		extUUID            string
		connectedStoreUUID *string
		*levels                    // ‡ the current levels
		prev               *levels // ‡ the previous levels
		ackLevelTicker     *time.Ticker
		closeChannel       chan struct{}
		waitConsumed       chan<- bool // waitConsumed is the channel which will signal if the extent is completely consumed given by extentCache
		metaclient         metadata.TChanMetadataService
		doneWG             sync.WaitGroup
		logger             bark.Logger
		sessionID          uint16
		ackMgrID           uint16              // ID of this ackManager; unique on this host
		cgCache            *consumerGroupCache // back pointer to the consumer group cache
		lk                 sync.RWMutex        // ‡ = guarded by this mutex
	}
)

func newAckManager(cgCache *consumerGroupCache, ackMgrID uint32, outputHostUUID string, cgUUID string, extUUID string, connectedStoreUUID *string, waitConsumedCh chan<- bool, cge *metadata.ConsumerGroupExtent, metaclient metadata.TChanMetadataService, logger bark.Logger) *ackManager {
	ackMgr := &ackManager{
		addrs:              make(map[common.SequenceNumber]*internalMsg),
		cgCache:            cgCache,
		outputHostUUID:     outputHostUUID,
		cgUUID:             cgUUID,
		extUUID:            extUUID,
		connectedStoreUUID: connectedStoreUUID,
		sessionID:          cgCache.sessionID, //sessionID,
		ackMgrID:           uint16(ackMgrID),  //ackMgrID,
		metaclient:         metaclient,
		ackLevelTicker:     time.NewTicker(ackLevelInterval),
		waitConsumed:       waitConsumedCh,
		logger:             logger.WithField(common.TagModule, `ackMgr`),
	}

	// Set the previous levels now, so that on our first update, we will calculate rates correctly
	ackMgr.prev = &levels{
		asOf:          common.Now(),
		readLevel:     common.SequenceNumber(cge.GetAckLevelSeqNo()),
		ackLevel:      common.SequenceNumber(cge.GetAckLevelSeqNo()),
		ackLevelAddr:  storeHostAddress(cge.GetAckLevelOffset()),
		readLevelAddr: storeHostAddress(cge.GetReadLevelOffset()),
	}

	ackMgr.levels = &levels{
		readLevel:     common.SequenceNumber(cge.GetAckLevelSeqNo()),
		ackLevel:      common.SequenceNumber(cge.GetAckLevelSeqNo()),
		ackLevelAddr:  storeHostAddress(cge.GetAckLevelOffset()),
		readLevelAddr: storeHostAddress(cge.GetReadLevelOffset()),
	}

	return ackMgr
}

// ackID is a string which is a base64 encoded string
// First we get the ackID and store the address locally in our data structure
// for maintaining the ack level
func (ackMgr *ackManager) getNextAckID(address int64, sequence common.SequenceNumber) (ackID string) {
	ackMgr.lk.Lock()
	ackMgr.readLevel++ // This means that the first ID is '1'
	ackMgr.readLevelAddr = storeHostAddress(address)

	var expectedReadLevel common.SequenceNumber
	if msg, ok := ackMgr.addrs[ackMgr.readLevel]; ok {
		expectedReadLevel = msg.seq
	} else {
		expectedReadLevel = sequence
	}

	// NOTE: sequence should be zero for timer destinations, since they usually have discontinuous sequence numbers
	if sequence != 0 && expectedReadLevel != sequence {
		skippedMessages := sequence - expectedReadLevel

		if skippedMessages < 0 {
			ackMgr.logger.WithFields(bark.Fields{
				`gotSeqNum`:      sequence,
				`gotAddress`:     address,
				`expectedSeqNum`: expectedReadLevel,
				`readLevel`:      ackMgr.readLevel,
			}).Error(`negative discontinuity detected (rollback)`)
			// Don't update gauge, since negative numbers aren't supported for M3 gauges
		} else {
			// update gauge here to say we skipped messages (potentially due to retention?)
			ackMgr.cgCache.consumerM3Client.UpdateGauge(metrics.ConsConnectionScope, metrics.OutputhostCGSkippedMessages, int64(skippedMessages))
		}
		// TODO: Support message auditing by creating discontinuity events, rather than just updating the offset here
		// TODO: Consumption rates will be spuriously high
		// NOTE: Increasing the offset here also affects the AckLevelSeqNo, which will tend to push it into the discontinuity,
		//       which makes it appear that a retained message was received. This is OK, and it will cause queue depth to react
		//       to retention events a bit faster.
		// NOTE: The same skipped messages can be reported twice if the outputhost is restarted before the ack level
		//       passes the discontinuity
		// TODO: review/fix comments above!
	}

	ackID = common.ConstructAckID(ackMgr.sessionID, ackMgr.ackMgrID, uint32(ackMgr.readLevel), address)

	// now store the message in the data structure internally
	ackMgr.addrs[ackMgr.readLevel] = &internalMsg{
		addr: storeHostAddress(address),
		seq:  sequence,
	}

	ackMgr.lk.Unlock()

	return
}

func (ackMgr *ackManager) stop() {
	close(ackMgr.closeChannel)
	ackMgr.doneWG.Wait()
	ackMgr.logger.WithFields(bark.Fields{
		`state`: ackMgr.getAckMgrState().String(),
	}).Info("ackMgr stopped")
}

func (ackMgr *ackManager) start() {
	ackMgr.closeChannel = make(chan struct{})
	ackMgr.doneWG.Add(1)
	go ackMgr.manageAckLevel()
}

func (ackMgr *ackManager) getCurrentReadLevel() (addr storeHostAddress, seqNo common.SequenceNumber) {
	ackMgr.lk.RLock()
	defer ackMgr.lk.RUnlock()

	return ackMgr.readLevelAddr, ackMgr.readLevel
}

// resetMsg is a utility routine which is used to reset the readLevel because
// we couldn't even write this to the client msgsCh
func (ackMgr *ackManager) resetMsg(offset int64) {
	ackMgr.lk.Lock()
	// make sure the address of the current readLevel matches with this offset
	if addrs, ok := ackMgr.addrs[ackMgr.readLevel]; ok {
		expectedOffset := int64(addrs.addr)
		if expectedOffset != offset {
			// this should *never* happen and we should panic here because the only way to
			// get here is only because of memory corruption.
			ackMgr.cgCache.consumerM3Client.UpdateGauge(metrics.ConsConnectionScope, metrics.OutputhostCGAckMgrResetMsgError, 1)
			ackMgr.logger.WithFields(bark.Fields{
				`expectedOffset`: expectedOffset,
				`offset`:         offset,
			}).Panic(`resetMsg: offsets don't match!`)
		} else {
			// report that we removed a read level
			ackMgr.cgCache.consumerM3Client.UpdateGauge(metrics.ConsConnectionScope, metrics.OutputhostCGAckMgrResetMsg, 1)
			delete(ackMgr.addrs, ackMgr.readLevel)
			// move the readLevel one below, since this message is not here anymore
			ackMgr.readLevel--
		}
	}
	ackMgr.lk.Unlock()
}

// notifySealed just makes a note that this extent is sealed
func (ackMgr *ackManager) notifySealed() {
	ackMgr.lk.Lock()
	ackMgr.sealed = true
	ackMgr.lk.Unlock()
	return
}

func (ackMgr *ackManager) updateAckLevel() {
	update := false
	consumed := false
	var oReq *metadata.SetAckOffsetRequest

	ackMgr.lk.Lock()

	count := 0
	stop := ackMgr.ackLevel + common.SequenceNumber(int64(len(ackMgr.addrs)))

	// We go through the map here and see if the messages are acked,
	// moving the acklevel as we go forward.
	for curr := ackMgr.ackLevel + 1; curr <= stop; curr++ {
		if addrs, ok := ackMgr.addrs[curr]; ok {
			if addrs.acked {
				update = true
				ackMgr.ackLevel = curr
				count++
			} else {
				break
			}
		}
	}

	// check if the extent can be marked as consumed
	// We can mark an extent as consumed, if we have both these conditions:
	// 1. The extent is sealed (which means we have it marked after receiving the last message)
	// 2. The ackLevel has reached the end (which means that the ackLevel equals the readLevel)
	if ackMgr.sealed && ackMgr.ackLevel == ackMgr.readLevel {
		ackMgr.logger.Debug("extent sealed and consumed")
		consumed = true
		update = true
	}

	// check if ackLevel is valid here and get the addr here
	if _, ok := ackMgr.addrs[ackMgr.ackLevel]; ok {
		ackMgr.ackLevelAddr = ackMgr.addrs[ackMgr.ackLevel].addr
	}

	if update {
		ackMgr.asOf = common.Now()
		oReq = &metadata.SetAckOffsetRequest{
			OutputHostUUID:     common.StringPtr(ackMgr.outputHostUUID),
			ConsumerGroupUUID:  common.StringPtr(ackMgr.cgUUID),
			ExtentUUID:         common.StringPtr(ackMgr.extUUID),
			ConnectedStoreUUID: common.StringPtr(*ackMgr.connectedStoreUUID),
			AckLevelAddress:    common.Int64Ptr(int64(ackMgr.ackLevelAddr)),
			AckLevelSeqNo:      common.Int64Ptr(int64(ackMgr.addrs[ackMgr.ackLevel].seq)),
			ReadLevelAddress:   common.Int64Ptr(int64(ackMgr.readLevelAddr)),
			ReadLevelSeqNo:     common.Int64Ptr(int64(ackMgr.addrs[ackMgr.readLevel].seq)),
		}

		// check if we can set the status as consumed
		if consumed {
			oReq.Status = common.CheramiConsumerGroupExtentStatusPtr(metadata.ConsumerGroupExtentStatus_CONSUMED)
			// Rates are forced to zero in the consumed case
			oReq.AckLevelSeqNoRate = common.Float64Ptr(0e0)
			oReq.ReadLevelSeqNoRate = common.Float64Ptr(0e0)
		} else {
			oReq.Status = common.CheramiConsumerGroupExtentStatusPtr(metadata.ConsumerGroupExtentStatus_OPEN)
			oReq.AckLevelSeqNoRate = common.Float64Ptr(common.CalculateRate(ackMgr.prev.ackLevel, ackMgr.ackLevel, ackMgr.prev.asOf, ackMgr.asOf))
			oReq.ReadLevelSeqNoRate = common.Float64Ptr(common.CalculateRate(ackMgr.prev.readLevel, ackMgr.readLevel, ackMgr.prev.asOf, ackMgr.asOf))
		}

		// move the current levels to prev and replace the current levels with a deep copy
		ackMgr.prev = ackMgr.levels
		ackMgr.levels = &levels{}
		*ackMgr.levels = *ackMgr.prev
	}

	updatedSize := len(ackMgr.addrs)
	ackMgr.lk.Unlock()

	if update {
		ctx, cancel := thrift.NewContext(metaContextTimeout)
		defer cancel()
		// check if we can set the status as consumed
		// ackMgr.logger.
		//	WithField(common.TagCnsm, common.FmtCnsm(ackMgr.cgUUID)).
		//	WithField(common.TagExt, common.FmtExt(ackMgr.extUUID)).
		//	WithField(`ackLevelAddress`, oReq.GetAckLevelAddress()).
		//	Debug(`outputhost: Setting ackLevel`)

		if err := ackMgr.metaclient.SetAckOffset(ctx, oReq); err != nil {
			ackMgr.logger.WithFields(bark.Fields{
				common.TagErr:     err,
				`ackLevelAddress`: oReq.GetAckLevelAddress(),
			}).Error(`error updating ackLevel`)
		} else {
			// Updating metadata succeeded; report some metrics and mark the extent as consumed if necessary
			// report the count of updates we did this round
			ackMgr.cgCache.consumerM3Client.UpdateGauge(metrics.ConsConnectionScope, metrics.OutputhostCGAckMgrLevelUpdate, int64(count))
			if consumed {
				// report that the extent is consumed
				ackMgr.cgCache.consumerM3Client.UpdateGauge(metrics.ConsConnectionScope, metrics.OutputhostCGAckMgrConsumed, 1)
				// notify extentCache that this extent is consumed
				// We can do this in a non-blocking way because, if we fail to
				// notify this time, we will anyway notify it the next round.
				// We do it in a non-blocking way because we could potentially
				// deadlock during unload
				select {
				case ackMgr.waitConsumed <- true:
					ackMgr.logger.WithField(`ackLevelAddress`, oReq.GetAckLevelAddress()).Info(`extent consumed`)
				default:
				}
			}
		}
	}

	// Report the size of the ackMgr map, if greater than 0
	if updatedSize > 0 {
		ackMgr.cgCache.consumerM3Client.UpdateGauge(metrics.ConsConnectionScope, metrics.OutputhostCGAckMgrSize, int64(updatedSize))
	}
}

func (ackMgr *ackManager) acknowledgeMessage(ackID AckID, seqNum uint32, address int64, isNack bool) error {
	var err error
	notifyCg := true
	ackMgr.lk.Lock() // Read lock would be OK in this case (except for a benign race with two simultaneous acks for the same ackID), see below
	// check if this id is present
	if addrs, ok := ackMgr.addrs[common.SequenceNumber(seqNum)]; ok {
		// validate the address from the ackID
		if addrs.addr != storeHostAddress(address) {
			ackMgr.logger.WithFields(bark.Fields{
				`address`:  address,
				`expected`: addrs.addr,
			}).Error(`ack address does not match!`)
			err = errors.New("address of the ackID doesn't match with ackMgr")
			notifyCg = false
		} else {
			if ackMgr.cgCache.cachedCGDesc.GetOwnerEmail() == SmartRetryDisableString {
				ackMgr.logger.WithFields(bark.Fields{
					`Address`:     address,
					`addr`:        addrs.addr,
					common.TagSeq: seqNum,
					`isNack`:      isNack,
				}).Info(`msg ack`)
			}
			if !isNack {
				addrs.acked = true // This is the only place that this field of addrs is changed. It was initially set under a write lock elsewhere, hence we can have a read lock
				// update the last acked sequence, if this is the most recent ack
				if ackMgr.lastAckedSeq < common.SequenceNumber(seqNum) {
					ackMgr.lastAckedSeq = common.SequenceNumber(seqNum)
				}
			}
		}
	} else {
		// Update metric to reflect that the sequence number is not found
		ackMgr.cgCache.consumerM3Client.IncCounter(metrics.ConsConnectionScope, metrics.OutputhostCGAckMgrSeqNotFound)
	}
	ackMgr.lk.Unlock()

	// Now notify the message cache so that it can update it's state
	// Note: We explicitly do this outside the lock above to prevent us from
	// blocking with a lock held
	// send the ack to the ack channel for the msg cache to cleanup
	if notifyCg {
		if isNack {
			ackMgr.cgCache.nackMsgCh <- timestampedAckID{AckID: ackID, ts: common.Now()}
		} else {
			ackMgr.cgCache.ackMsgCh <- timestampedAckID{AckID: ackID, ts: common.Now()}
		}
	}
	return err
}

func (ackMgr *ackManager) manageAckLevel() {
	defer ackMgr.doneWG.Done()
	// this needs to look at all the acked messages and update the ackLevel
	// accordingly.
	for {
		select {
		case <-ackMgr.ackLevelTicker.C:
			ackMgr.updateAckLevel()
		case <-ackMgr.closeChannel:
			// before returning make sure we try to set the ack offset
			ackMgr.updateAckLevel()
			return
		}
	}
}

// get the number of acked and unacked messages from the last ack level
func (ackMgr *ackManager) getNumAckedAndUnackedMessages() (*int64, *int64) {
	stop := ackMgr.ackLevel + common.SequenceNumber(int64(len(ackMgr.addrs)))

	var acked int64
	var unacked int64
	// We go through the map here and see if the messages are acked,
	for curr := ackMgr.ackLevel + 1; curr <= stop; curr++ {
		if addrs, ok := ackMgr.addrs[curr]; ok {
			if addrs.acked {
				acked++
			} else {
				unacked++
			}
		} else {
			ackMgr.logger.WithFields(bark.Fields{
				common.TagSeq: curr,
			}).Error(`sequence number not found in the ack mgr`)
		}
	}

	return common.Int64Ptr(acked), common.Int64Ptr(unacked)
}

func (ackMgr *ackManager) getAckMgrState() *admin.AckMgrState {
	ackMgrState := admin.NewAckMgrState()
	ackMgr.lk.RLock()
	defer ackMgr.lk.RUnlock()
	ackMgrState.AckMgrID = common.Int16Ptr(int16(ackMgr.ackMgrID))
	ackMgrState.IsSealed = common.BoolPtr(ackMgr.sealed)
	ackMgrState.ReadLevelSeq = common.Int64Ptr(int64(ackMgr.readLevel))
	ackMgrState.AckLevelSeq = common.Int64Ptr(int64(ackMgr.ackLevel))
	ackMgrState.ReadLevelOffset = common.Int64Ptr(int64(ackMgr.readLevelAddr))
	ackMgrState.AckLevelOffset = common.Int64Ptr(int64(ackMgr.ackLevelAddr))
	ackMgrState.LastAckLevelUpdateTime = common.Int64Ptr(int64(ackMgr.asOf))
	ackMgrState.LastAckedSeq = common.Int64Ptr(int64(ackMgr.lastAckedSeq))
	ackMgrState.NumAckedMsgs, ackMgrState.NumUnackedMsgs = ackMgr.getNumAckedAndUnackedMessages()

	return ackMgrState
}
