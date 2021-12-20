// Copyright 2020 MatrixOrigin.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package raftstore

import (
	"sync/atomic"
	"time"

	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"
	trackerPkg "go.etcd.io/etcd/raft/v3/tracker"
	"go.uber.org/zap"

	"github.com/fagongzi/util/protoc"
	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/components/prophet/pb/rpcpb"
	"github.com/matrixorigin/matrixcube/logdb"
	"github.com/matrixorigin/matrixcube/metric"
	"github.com/matrixorigin/matrixcube/pb/meta"
	"github.com/matrixorigin/matrixcube/pb/rpc"
	"github.com/matrixorigin/matrixcube/util"
	"github.com/matrixorigin/matrixcube/util/uuid"
)

const (
	readyBatchSize = 1024
)

type action struct {
	actionType         actionType
	snapshotCompaction snapshotCompactionDetails
	splitCheckData     splitCheckData
	targetIndex        uint64
	readMetrics        readMetrics
	epoch              Epoch
	actionCallback     func(interface{})
}

type readMetrics struct {
	readBytes uint64
	readKeys  uint64
}

type splitCheckData struct {
	keys      uint64
	size      uint64
	splitKeys [][]byte
	splitIDs  []rpcpb.SplitID
	ctx       []byte
}

type snapshotCompactionDetails struct {
	snapshot           raftpb.Snapshot
	persistentLogIndex uint64
}

type actionType int

const (
	campaignAction actionType = iota
	checkSplitAction
	checkCompactLogAction
	splitAction
	heartbeatAction
	updateReadMetrics
	checkLogCommittedAction
	checkLogAppliedAction
	logCompactionAction
	snapshotCompactionAction
)

func (pr *replica) addAdminRequest(adminType rpc.AdminCmdType, request protoc.PB) {
	shard := pr.getShard()
	pr.addRequest(newReqCtx(rpc.Request{
		ID:         uuid.NewV4().Bytes(),
		Group:      shard.Group,
		ToShard:    shard.ID,
		Type:       rpc.CmdType_Admin,
		CustomType: uint64(adminType),
		Epoch:      shard.Epoch,
		Cmd:        protoc.MustMarshal(request),
	}, nil))
}

func (pr *replica) addRequest(req reqCtx) error {
	if err := pr.requests.Put(req); err != nil {
		return err
	}
	pr.notifyWorker()
	return nil
}

// addAction adds the specified action to the actions queue so it will be
// scheduled to execute in the raft worker thread.
func (pr *replica) addAction(act action) {
	if err := pr.actions.Put(act); err != nil {
		return
	}
	pr.notifyWorker()
}

func (pr *replica) addMessage(msg meta.RaftMessage) {
	if err := pr.messages.Put(msg); err != nil {
		pr.logger.Info("raft step stopped")
		return
	}
	pr.notifyWorker()
}

func (pr *replica) addFeedback(feedback interface{}) {
	if err := pr.feedbacks.Put(feedback); err != nil {
		pr.logger.Info("raft feedback stopped")
	}
	pr.notifyWorker()
}

func (pr *replica) addSnapshotStatus(ss snapshotStatus) {
	if err := pr.snapshotStatus.Put(ss); err != nil {
		pr.logger.Info("snapshot status stopped")
	}
	pr.notifyWorker()
}

func (pr *replica) addRaftTick() bool {
	if err := pr.ticks.Put(struct{}{}); err != nil {
		return false
	}
	atomic.AddUint64(&pr.tickTotalCount, 1)
	pr.notifyWorker()
	return true
}

func (pr *replica) onRaftTick(arg interface{}) {
	if pr.addRaftTick() {
		metric.SetRaftTickQueueMetric(pr.ticks.Len())
		util.DefaultTimeoutWheel().Schedule(pr.cfg.Raft.TickInterval.Duration, pr.onRaftTick, nil)
		return
	}
	pr.logger.Info("raft tick stopped")
}

func (pr *replica) shutdown() {
	pr.metrics.flush()
	pr.actions.Dispose()
	pr.ticks.Dispose()
	pr.messages.Dispose()
	pr.feedbacks.Dispose()

	// resp all stale requests in batch and queue
	for {
		if pr.incomingProposals.isEmpty() {
			break
		}
		if c, ok := pr.incomingProposals.pop(); ok {
			for _, req := range c.requestBatch.Requests {
				respStoreNotMatch(errStoreNotMatch, req, c.cb)
			}
		}
	}

	// resp all pending proposals
	pr.pendingProposals.close()

	// resp all pending requests in batch and queue
	for _, rr := range pr.pendingReads.reads {
		for _, req := range rr.batch.Requests {
			respStoreNotMatch(errStoreNotMatch, req, pr.store.shardsProxy.OnResponse)
		}
	}
	pr.pendingReads.reset()

	requests := pr.requests.Dispose()
	for _, r := range requests {
		req := r.(reqCtx)
		if req.cb != nil {
			respStoreNotMatch(errStoreNotMatch, req.req, req.cb)
		}
	}

	// This replica won't be processed by the eventWorker again.
	// This means no further read requests will be started using the stopper.
	pr.readStopper.Stop()
	pr.sm.close()
	pr.logger.Info("replica shutdown completed")
}

func (pr *replica) handleEvent(wc *logdb.WorkerContext) (hasEvent bool, err error) {
	select {
	case <-pr.closedC:
		if !pr.unloaded() {
			pr.shutdown()
			pr.confirmUnloaded()
		}
		pr.logger.Debug("skip handling events on stopped replica")
		return false, nil
	default:
	}

	hasEvent, err = pr.handleInitializedState()
	if err != nil {
		return hasEvent, err
	}
	if hasEvent {
		return hasEvent, nil
	}
	if pr.handleMessage(pr.items) {
		hasEvent = true
	}
	if pr.handleTick(pr.items) {
		hasEvent = true
	}
	if pr.handleFeedback(pr.items) {
		hasEvent = true
	}
	if pr.handleSnapshotStatus(pr.items) {
		hasEvent = true
	}
	if pr.handleRequest(pr.items) {
		hasEvent = true
	}
	if pr.rn.HasReady() {
		hasEvent = true
		if err := pr.handleRaftReady(wc); err != nil {
			return hasEvent, err
		}
	}
	if pr.handleAction(pr.items) {
		hasEvent = true
	}
	return hasEvent, nil
}

// apply the already received snapshot
// for safety, we have to apply the snapshot once it is received and acked. it
// would corrupt the raft state if we just ignore such snapshots.
func (pr *replica) handleInitializedState() (bool, error) {
	if pr.initialized {
		return false, nil
	}
	pr.initialized = true
	pr.logger.Debug("checking initial snapshot")
	ss, err := pr.logdb.GetSnapshot(pr.shardID)
	if err == logdb.ErrNoSnapshot {
		pr.logger.Info("no initial snapshot")
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if raft.IsEmptySnap(ss) {
		// should never be empty here
		panic("unexpected empty snapshot")
	}
	index, err := pr.getPersistentLogIndex()
	if err != nil {
		return false, err
	}
	pr.logger.Info("initial snapshot available",
		zap.Uint64("persistent-log-index", index),
		log.SnapshotField(ss))
	if ss.Metadata.Index > index {
		pr.logger.Info("applying initial snapshot",
			log.IndexField(ss.Metadata.Index))
		if err := pr.applySnapshot(ss); err != nil {
			return false, err
		}
		pr.pushedIndex = ss.Metadata.Index
	} else {
		// snapshot is out of date, remove the disk image as we will never apply
		// the snapshot in the future. keep the logdb record as LogReader needs
		// this snapshot record to establish the starting point of the persisted
		// raft log entries.
		pr.logger.Info("skipped applying initial snapshot",
			log.IndexField(ss.Metadata.Index))
		if err := pr.removeSnapshot(ss, false); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (pr *replica) handleAction(items []interface{}) bool {
	if size := pr.actions.Len(); size == 0 {
		return false
	}
	n, err := pr.actions.Get(readyBatchSize, items)
	if err != nil {
		return false
	}

	for i := int64(0); i < n; i++ {
		act := items[i].(action)
		switch act.actionType {
		case checkSplitAction:
			pr.tryCheckSplit(act)
		case splitAction:
			pr.doSplit(act)
		case campaignAction:
			if err := pr.doCampaign(); err != nil {
				pr.logger.Fatal("failed to do campaign",
					zap.Error(err))
			}
		case heartbeatAction:
			pr.prophetHeartbeat()
		case updateReadMetrics:
			pr.doUpdateReadMetrics(act)
		case checkLogCommittedAction:
			pr.doCheckLogCommitted(act)
		case checkLogAppliedAction:
			pr.doCheckLogApplied(act)
		case checkCompactLogAction:
			pr.doCheckLogCompact(pr.rn.Status().Progress, pr.rn.LastIndex())
		case logCompactionAction:
			pr.doLogCompaction(act.targetIndex)
		case snapshotCompactionAction:
			pr.doSnapshotCompaction(act.snapshotCompaction.snapshot,
				act.snapshotCompaction.persistentLogIndex)
		}
	}

	if pr.actions.Len() > 0 {
		pr.notifyWorker()
	}
	return true
}

func (pr *replica) doUpdateReadMetrics(act action) {
	pr.stats.readBytes += act.readMetrics.readBytes
	pr.stats.readKeys += act.readMetrics.readKeys
}

func (pr *replica) handleMessage(items []interface{}) bool {
	if size := pr.messages.Len(); size == 0 {
		return false
	}

	n, err := pr.messages.Get(readyBatchSize, items)
	if err != nil {
		return false
	}
	for i := int64(0); i < n; i++ {
		raftMsg := items[i].(meta.RaftMessage)
		msg := raftMsg.Message
		pr.updateReplicasCommittedIndex(raftMsg)

		if pr.isLeader() && msg.From != 0 {
			pr.replicaHeartbeatsMap.Store(msg.From, time.Now())
		}

		if err := pr.rn.Step(msg); err != nil {
			pr.logger.Error("fail to step raft",
				zap.Error(err))
		}
	}

	size := pr.messages.Len()
	metric.SetRaftStepQueueMetric(size)
	if size > 0 {
		pr.notifyWorker()
	}
	return true
}

func (pr *replica) updateReplicasCommittedIndex(msg meta.RaftMessage) {
	pr.committedIndexes[msg.From.ID] = msg.CommitIndex
}

func (pr *replica) handleTick(items []interface{}) bool {
	if size := pr.ticks.Len(); size == 0 {
		pr.metrics.flush()
		metric.SetRaftTickQueueMetric(size)
		return false
	}

	n, err := pr.ticks.Get(readyBatchSize, items)
	if err != nil {
		return false
	}
	for i := int64(0); i < n; i++ {
		pr.rn.Tick()
		atomic.AddUint64(&pr.tickHandledCount, 1)
	}

	return true
}

func (pr *replica) handleFeedback(items []interface{}) bool {
	if size := pr.feedbacks.Len(); size == 0 {
		return false
	}

	n, err := pr.feedbacks.Get(readyBatchSize, items)
	if err != nil {
		return false
	}
	for i := int64(0); i < n; i++ {
		if replicaID, ok := items[i].(uint64); ok {
			pr.rn.ReportUnreachable(replicaID)
		}
	}

	size := pr.feedbacks.Len()
	metric.SetRaftReportQueueMetric(size)
	if size > 0 {
		pr.notifyWorker()
	}

	return true
}

func (pr *replica) handleSnapshotStatus(items []interface{}) bool {
	if size := pr.snapshotStatus.Len(); size == 0 {
		return false
	}

	n, err := pr.snapshotStatus.Get(readyBatchSize, items)
	if err != nil {
		return false
	}
	for i := int64(0); i < n; i++ {
		if ss, ok := items[i].(snapshotStatus); ok {
			rss := raft.SnapshotFinish
			if ss.rejected {
				rss = raft.SnapshotFailure
			}
			pr.rn.ReportSnapshot(ss.to, rss)
		}
	}

	size := pr.snapshotStatus.Len()
	metric.SetRaftReportQueueMetric(size)
	if size > 0 {
		pr.notifyWorker()
	}

	return true
}

func (pr *replica) prophetHeartbeat() {
	if !pr.isLeader() {
		return
	}
	shard := pr.getShard()
	req := rpcpb.ResourceHeartbeatReq{
		Term:            pr.rn.BasicStatus().Term,
		Leader:          &pr.replica,
		ContainerID:     pr.storeID,
		DownReplicas:    pr.collectDownReplicas(),
		PendingReplicas: pr.collectPendingReplicas(),
		Stats:           pr.stats.heartbeatState(),
		GroupKey:        pr.groupController.getShardGroupKey(shard),
	}

	resource := NewResourceAdapterWithShard(shard)
	if err := pr.prophetClient.ResourceHeartbeat(resource, req); err != nil {
		pr.logger.Error("fail to send heartbeat to prophet",
			zap.Error(err))
	}
}

func (pr *replica) doCheckLogCompact(progresses map[uint64]trackerPkg.Progress, lastIndex uint64) {
	if !pr.isLeader() {
		return
	}
	var minReplicatedIndex uint64
	for _, p := range progresses {
		if minReplicatedIndex == 0 {
			minReplicatedIndex = p.Match
		}
		if p.Match < minReplicatedIndex {
			minReplicatedIndex = p.Match
		}
	}
	// When an election happened or a new replica is added, replicatedIdx can be 0.
	if minReplicatedIndex > 0 {
		if lastIndex < minReplicatedIndex {
			pr.logger.Fatal("invalid replicated index",
				zap.Uint64("replicated", minReplicatedIndex),
				zap.Uint64("last", lastIndex))
		}
		metric.ObserveRaftLogLag(lastIndex - minReplicatedIndex)
	}

	compactIndex := minReplicatedIndex
	appliedIndex := pr.appliedIndex
	firstIndex := pr.getFirstIndex()
	if minReplicatedIndex < firstIndex ||
		minReplicatedIndex-firstIndex <= pr.store.cfg.Raft.RaftLog.CompactThreshold {
		pr.logger.Debug("maybe skip requesting log compaction",
			zap.Uint64("min-replicated-index", minReplicatedIndex),
			zap.Uint64("applied-index", minReplicatedIndex),
			zap.Uint64("last-index", lastIndex),
			zap.Uint64("first-index", firstIndex),
			zap.Uint64("threshold", pr.store.cfg.Raft.RaftLog.CompactThreshold))
		compactIndex = 0
	}

	// check wether to force compaction or not
	if compactIndex == 0 &&
		appliedIndex > firstIndex &&
		appliedIndex-firstIndex >= pr.store.cfg.Raft.RaftLog.ForceCompactCount {
		compactIndex = appliedIndex
	} else if compactIndex == 0 &&
		pr.stats.raftLogSizeHint >= pr.store.cfg.Raft.RaftLog.ForceCompactBytes {
		compactIndex = appliedIndex
	}

	if compactIndex == 0 {
		pr.logger.Debug("requesting log compaction skipped",
			zap.Uint64("min-replicated-index", minReplicatedIndex),
			zap.Uint64("applied-index", minReplicatedIndex),
			zap.Uint64("last-index", lastIndex),
			zap.Uint64("first-index", firstIndex),
			zap.Uint64("threshold", pr.store.cfg.Raft.RaftLog.CompactThreshold))
		return
	}

	if compactIndex > minReplicatedIndex {
		pr.logger.Debug("some replica lag is too large, maybe sent a snapshot later",
			zap.Uint64("lag", compactIndex-minReplicatedIndex))
	}
	compactIndex--
	if compactIndex < firstIndex {
		return
	}
	pr.logger.Info("requesting log compaction",
		log.IndexField(compactIndex))
	pr.addAdminRequest(rpc.AdminCmdType_CompactLog, &rpc.CompactLogRequest{
		CompactIndex: compactIndex,
	})
}

func (pr *replica) doLogCompaction(index uint64) {
	if index == 0 {
		return
	}
	pr.logger.Info("log compaction action handled",
		log.IndexField(index))
	// generate a dummy snapshot so we can run the log compaction.
	// this dummy snapshot will be used to establish the marker position of
	// the LogReader on startup.
	// such dummy snapshot will never be loaded, as its Index value is not
	// greater than data storage's persistentLogIndex value.
	term, err := pr.lr.Term(index)
	if err != nil {
		pr.logger.Error("failed to get term value",
			zap.Error(err),
			log.IndexField(index))
		if err == raft.ErrCompacted || err == raft.ErrUnavailable {
			// skip this compaction operation as we can't establish the marker
			// position.
			pr.logger.Info("skipped a compaction action",
				log.IndexField(index))
			return
		}
		panic(err)
	}
	// this is a dummy snapshot meaning there is no on disk snapshot image.
	// we are not supposed to apply such dummy snapshot. the dummy flag is
	// used for debugging purposes.
	si := meta.SnapshotInfo{
		Dummy: true,
	}
	rd := raft.Ready{
		Snapshot: raftpb.Snapshot{
			Metadata: raftpb.SnapshotMetadata{
				Index: index,
				Term:  term,
			},
			Data: protoc.MustMarshal(&si),
		},
	}
	wc := pr.logdb.NewWorkerContext()
	defer wc.Close()
	if err := pr.logdb.SaveRaftState(pr.shardID, pr.replicaID, rd, wc); err != nil {
		panic(err)
	}
	pr.logger.Info("dummy snapshot saved",
		log.IndexField(index))
	// update LogReader's range info to make the compacted entries invisible to
	// raft.
	if err := pr.lr.Compact(index); err != nil {
		if err != raft.ErrCompacted {
			// TODO: check whether any error should be tolerated.
			panic(err)
		}
	}
	if err := pr.logdb.RemoveEntriesTo(pr.shardID, pr.replicaID, index); err != nil {
		panic(err)
	}
	pr.logger.Info("compaction completed",
		log.IndexField(index))
}

func (pr *replica) doSnapshotCompaction(ss raftpb.Snapshot, index uint64) {
	if err := pr.snapshotCompaction(ss, index); err != nil {
		panic(err)
	}
}
