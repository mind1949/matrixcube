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
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/fagongzi/util/collection/deque"
	"github.com/fagongzi/util/protoc"
	"github.com/matrixorigin/matrixcube/components/keys"
	"github.com/matrixorigin/matrixcube/components/log"
	"github.com/matrixorigin/matrixcube/components/prophet/pb/metapb"
	"github.com/matrixorigin/matrixcube/pb/meta"
	"github.com/matrixorigin/matrixcube/pb/rpc"
	"github.com/matrixorigin/matrixcube/storage"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func (d *stateMachine) execAdminRequest(ctx *applyContext) (rpc.ResponseBatch, error) {
	cmdType := ctx.req.AdminRequest.CmdType
	switch cmdType {
	case rpc.AdminCmdType_ConfigChange:
		return d.doExecChangeReplica(ctx)
	case rpc.AdminCmdType_ConfigChangeV2:
		return d.doExecChangeReplicaV2(ctx)
	case rpc.AdminCmdType_BatchSplit:
		return d.doExecSplit(ctx)
	}

	return rpc.ResponseBatch{}, nil
}

func (d *stateMachine) doExecChangeReplica(ctx *applyContext) (rpc.ResponseBatch, error) {
	req := ctx.req.AdminRequest.ConfigChange
	replica := req.Replica
	current := d.getShard()

	d.logger.Info("begin to apply change replica",
		zap.Uint64("index", ctx.entry.Index),
		log.ShardField("current", current),
		log.ConfigChangeField("request", req))

	res := Shard{}
	protoc.MustUnmarshal(&res, protoc.MustMarshal(&current))
	res.Epoch.ConfVer++

	p := findReplica(&res, req.Replica.ContainerID)
	switch req.ChangeType {
	case metapb.ConfigChangeType_AddNode:
		exists := false
		if p != nil {
			exists = true
			if p.Role != metapb.ReplicaRole_Learner || p.ID != replica.ID {
				return rpc.ResponseBatch{}, fmt.Errorf("shard %d can't add duplicated replica %+v",
					res.ID,
					replica)
			}
			p.Role = metapb.ReplicaRole_Voter
		}

		if !exists {
			res.Replicas = append(res.Replicas, replica)
		}
	case metapb.ConfigChangeType_RemoveNode:
		if p != nil {
			if p.ID != replica.ID || p.ContainerID != replica.ContainerID {
				return rpc.ResponseBatch{}, fmt.Errorf("shard %+v ignore remove unmatched replica %+v",
					res.ID,
					replica)
			}

			if d.replicaID == replica.ID {
				// Remove ourself, we will destroy all shard data later.
				// So we need not to apply following logs.
				d.setPendingRemove()
			}
		} else {
			return rpc.ResponseBatch{}, fmt.Errorf("shard %+v remove missing replica %+v",
				res.ID,
				replica)
		}
	case metapb.ConfigChangeType_AddLearnerNode:
		if p != nil {
			return rpc.ResponseBatch{}, fmt.Errorf("shard-%d can't add duplicated learner %+v",
				res.ID,
				replica)
		}

		res.Replicas = append(res.Replicas, replica)
	}

	state := meta.ReplicaState_Normal
	if d.isPendingRemove() {
		state = meta.ReplicaState_Tombstone
	}

	d.updateShard(res)
	err := d.saveShardMetedata(ctx.entry.Index, res, state)
	if err != nil {
		d.logger.Fatal("fail to save metadata",
			zap.Error(err))
	}

	d.logger.Info("apply change replica complete",
		log.ShardField("metadata", res),
		zap.String("state", state.String()))

	resp := newAdminResponseBatch(rpc.AdminCmdType_ConfigChange, &rpc.ConfigChangeResponse{
		Shard: res,
	})
	ctx.adminResult = &adminExecResult{
		adminType: rpc.AdminCmdType_ConfigChange,
		configChangeResult: &configChangeResult{
			index:   ctx.entry.Index,
			changes: []rpc.ConfigChangeRequest{*req},
			shard:   res,
		},
	}
	return resp, nil
}

func (d *stateMachine) doExecChangeReplicaV2(ctx *applyContext) (rpc.ResponseBatch, error) {
	req := ctx.req.AdminRequest.ConfigChangeV2
	changes := req.Changes
	current := d.getShard()

	d.logger.Info("begin to apply change replica v2",
		zap.Uint64("index", ctx.entry.Index),
		log.ShardField("current", current),
		log.ConfigChangesField("requests", changes))

	var res Shard
	var err error
	kind := getConfChangeKind(len(changes))
	if kind == leaveJointKind {
		res, err = d.applyLeaveJoint()
	} else {
		res, err = d.applyConfChangeByKind(kind, changes)
	}

	if err != nil {
		return rpc.ResponseBatch{}, err
	}

	state := meta.ReplicaState_Normal
	if d.isPendingRemove() {
		state = meta.ReplicaState_Tombstone
	}

	d.updateShard(res)
	err = d.saveShardMetedata(ctx.entry.Index, res, state)
	if err != nil {
		d.logger.Fatal("fail to save metadata",
			zap.Error(err))
	}

	d.logger.Info("apply change replica v2 complete",
		log.ShardField("metadata", res),
		zap.String("state", state.String()))

	resp := newAdminResponseBatch(rpc.AdminCmdType_ConfigChange, &rpc.ConfigChangeResponse{
		Shard: res,
	})
	ctx.adminResult = &adminExecResult{
		adminType: rpc.AdminCmdType_ConfigChange,
		configChangeResult: &configChangeResult{
			index:   ctx.entry.Index,
			changes: changes,
			shard:   res,
		},
	}
	return resp, nil
}

func (d *stateMachine) applyConfChangeByKind(kind confChangeKind, changes []rpc.ConfigChangeRequest) (Shard, error) {
	res := Shard{}
	current := d.getShard()
	protoc.MustUnmarshal(&res, protoc.MustMarshal(&current))

	for _, cp := range changes {
		change_type := cp.ChangeType
		replica := cp.Replica
		store_id := replica.ContainerID

		exist_replica := findReplica(&current, replica.ContainerID)
		if exist_replica != nil {
			r := exist_replica.Role
			if r == metapb.ReplicaRole_IncomingVoter || r == metapb.ReplicaRole_DemotingVoter {
				d.logger.Fatal("can't apply confchange because configuration is still in joint state")
			}
		}

		if exist_replica == nil && change_type == metapb.ConfigChangeType_AddNode {
			if kind == simpleKind {
				replica.Role = metapb.ReplicaRole_Voter
			} else if kind == enterJointKind {
				replica.Role = metapb.ReplicaRole_IncomingVoter
			}

			res.Replicas = append(res.Replicas, replica)
		} else if exist_replica == nil && change_type == metapb.ConfigChangeType_AddLearnerNode {
			replica.Role = metapb.ReplicaRole_Learner
			res.Replicas = append(res.Replicas, replica)
		} else if exist_replica == nil && change_type == metapb.ConfigChangeType_RemoveNode {
			return res, fmt.Errorf("remove missing replica %+v", replica)
		} else if exist_replica != nil &&
			(change_type == metapb.ConfigChangeType_AddNode || change_type == metapb.ConfigChangeType_AddLearnerNode) {
			// add node
			role := exist_replica.Role
			exist_id := exist_replica.ID
			incoming_id := replica.ID

			// Add replica with different id to the same store
			if exist_id != incoming_id ||
				// The replica is already the requested role
				(role == metapb.ReplicaRole_Voter && change_type == metapb.ConfigChangeType_AddNode) ||
				(role == metapb.ReplicaRole_Learner && change_type == metapb.ConfigChangeType_AddLearnerNode) {
				return res, fmt.Errorf("can't add duplicated replica %+v, duplicated with exist replica %+v",
					replica, exist_replica)
			}

			if role == metapb.ReplicaRole_Voter && change_type == metapb.ConfigChangeType_AddLearnerNode {
				switch kind {
				case simpleKind:
					exist_replica.Role = metapb.ReplicaRole_Learner
				case enterJointKind:
					exist_replica.Role = metapb.ReplicaRole_DemotingVoter
				}
			} else if role == metapb.ReplicaRole_Learner && change_type == metapb.ConfigChangeType_AddNode {
				switch kind {
				case simpleKind:
					exist_replica.Role = metapb.ReplicaRole_Voter
				case enterJointKind:
					exist_replica.Role = metapb.ReplicaRole_IncomingVoter
				}
			}
		} else if exist_replica != nil && change_type == metapb.ConfigChangeType_RemoveNode {
			// Remove node
			if kind == enterJointKind && exist_replica.Role == metapb.ReplicaRole_Voter {
				return res, fmt.Errorf("can't remove voter replica %+v directly",
					replica)
			}

			p := removeReplica(&res, store_id)
			if p != nil {
				if p.ID != replica.ID || p.ContainerID != replica.ContainerID {
					return res, fmt.Errorf("ignore remove unmatched replica %+v", replica)
				}

				if d.replicaID == replica.ID {
					// Remove ourself, we will destroy all region data later.
					// So we need not to apply following logs.
					d.setPendingRemove()
				}
			}
		}
	}

	res.Epoch.ConfVer += uint64(len(changes))
	return res, nil
}

func (d *stateMachine) applyLeaveJoint() (Shard, error) {
	shard := Shard{}
	current := d.getShard()
	protoc.MustUnmarshal(&shard, protoc.MustMarshal(&current))

	change_num := uint64(0)
	for idx := range shard.Replicas {
		if shard.Replicas[idx].Role == metapb.ReplicaRole_IncomingVoter {
			shard.Replicas[idx].Role = metapb.ReplicaRole_Voter
			continue
		}

		if shard.Replicas[idx].Role == metapb.ReplicaRole_DemotingVoter {
			shard.Replicas[idx].Role = metapb.ReplicaRole_Learner
			continue
		}

		change_num += 1
	}
	if change_num == 0 {
		d.logger.Fatal("can't leave a non-joint config",
			log.ShardField("shard", shard))
	}
	shard.Epoch.ConfVer += change_num
	return shard, nil
}

func (d *stateMachine) doExecSplit(ctx *applyContext) (rpc.ResponseBatch, error) {
	ctx.metrics.admin.split++
	splitReqs := ctx.req.AdminRequest.Splits

	if len(splitReqs.Requests) == 0 {
		d.logger.Error("missing splits request")
		return rpc.ResponseBatch{}, errors.New("missing splits request")
	}

	newShardsCount := len(splitReqs.Requests)
	derived := Shard{}
	current := d.getShard()
	protoc.MustUnmarshal(&derived, protoc.MustMarshal(&current))
	var shards []Shard
	rangeKeys := deque.New()

	for _, req := range splitReqs.Requests {
		if len(req.SplitKey) == 0 {
			return rpc.ResponseBatch{}, errors.New("missing split key")
		}

		splitKey := keys.DecodeDataKey(req.SplitKey)
		v := derived.Start
		if e, ok := rangeKeys.Back(); ok {
			v = e.Value.([]byte)
		}
		if bytes.Compare(splitKey, v) <= 0 {
			return rpc.ResponseBatch{}, fmt.Errorf("invalid split key %+v", splitKey)
		}

		if len(req.NewReplicaIDs) != len(derived.Replicas) {
			return rpc.ResponseBatch{}, fmt.Errorf("invalid new replica id count, need %d, but got %d",
				len(derived.Replicas),
				len(req.NewReplicaIDs))
		}

		rangeKeys.PushBack(splitKey)
	}

	err := checkKeyInShard(rangeKeys.MustBack().Value.([]byte), &current)
	if err != nil {
		d.logger.Error("fail to split key",
			zap.String("err", err.Message))
		return rpc.ResponseBatch{}, nil
	}

	derived.Epoch.Version += uint64(newShardsCount)
	rangeKeys.PushBack(derived.End)
	derived.End = rangeKeys.MustFront().Value.([]byte)

	sort.Slice(derived.Replicas, func(i, j int) bool {
		return derived.Replicas[i].ID < derived.Replicas[j].ID
	})
	for _, req := range splitReqs.Requests {
		newShard := Shard{}
		newShard.ID = req.NewShardID
		newShard.Group = derived.Group
		newShard.Unique = derived.Unique
		newShard.RuleGroups = derived.RuleGroups
		newShard.DisableSplit = derived.DisableSplit
		newShard.Epoch = derived.Epoch
		newShard.Start = rangeKeys.PopFront().Value.([]byte)
		newShard.End = rangeKeys.MustFront().Value.([]byte)
		for idx, p := range derived.Replicas {
			newShard.Replicas = append(newShard.Replicas, Replica{
				ID:          req.NewReplicaIDs[idx],
				ContainerID: p.ContainerID,
			})
		}

		shards = append(shards, newShard)
		ctx.metrics.admin.splitSucceed++
	}

	// TODO(fagongzi): split with sync
	// e := d.dataStorage.Sync(d.shardID)
	// if e != nil {
	// 	logger.Fatalf("%s sync failed with %+v", d.pr.id(), e)
	// }

	// if d.store.cfg.Customize.CustomSplitCompletedFuncFactory != nil {
	// 	if fn := d.store.cfg.Customize.CustomSplitCompletedFuncFactory(derived.Group); fn != nil {
	// 		fn(&derived, shards)
	// 	}
	// }

	// d.updateShard(derived)
	// d.saveShardMetedata(d.shardID, d.getShard(), bhraftpb.ReplicaState_Normal)

	// d.store.updateReplicaState(derived, bhraftpb.ReplicaState_Normal, ctx.raftWB)
	// for _, shard := range shards {
	// 	d.store.updateReplicaState(shard, bhraftpb.ReplicaState_Normal, ctx.raftWB)
	// 	d.store.writeInitialState(shard.ID, ctx.raftWB)
	// }

	// rsp := newAdminResponseBatch(rpc.AdminCmdType_BatchSplit, &rpc.BatchSplitResponse{
	// 	Shards: shards,
	// })

	// result := &adminExecResult{
	// 	adminType: rpc.AdminCmdType_BatchSplit,
	// 	splitResult: &splitResult{
	// 		derived: derived,
	// 		shards:  shards,
	// 	},
	// }
	return rpc.ResponseBatch{}, nil
}

func (d *stateMachine) execWriteRequest(ctx *applyContext) rpc.ResponseBatch {
	ctx.writeCtx.reset(d.getShard())
	ctx.writeCtx.appendRequest(ctx.req)
	for _, req := range ctx.req.Requests {
		if ce := d.logger.Check(zapcore.DebugLevel, "begin to execute write"); ce != nil {
			ce.Write(log.HexField("id", req.ID))
		}
	}
	if err := d.dataStorage.GetExecutor().Write(ctx.writeCtx); err != nil {
		d.logger.Fatal("fail to exec read cmd",
			zap.Error(err))
	}
	for _, req := range ctx.req.Requests {
		d.logger.Debug("execute write completed",
			log.HexField("id", req.ID))
	}

	resp := rpc.ResponseBatch{}
	for _, v := range ctx.writeCtx.responses {
		ctx.metrics.writtenKeys++
		r := rpc.Response{}
		r.Value = v
		resp.Responses = append(resp.Responses, r)
	}
	d.updateWriteMetrics(ctx)
	return resp
}

func (d *stateMachine) updateWriteMetrics(ctx *applyContext) {
	ctx.metrics.writtenBytes += ctx.writeCtx.writtenBytes
	if ctx.writeCtx.diffBytes < 0 {
		v := uint64(math.Abs(float64(ctx.writeCtx.diffBytes)))
		if v >= ctx.metrics.sizeDiffHint {
			ctx.metrics.sizeDiffHint = 0
		} else {
			ctx.metrics.sizeDiffHint -= v
		}
	} else {
		ctx.metrics.sizeDiffHint += uint64(ctx.writeCtx.diffBytes)
	}
}

func (d *stateMachine) saveShardMetedata(index uint64, shard Shard, state meta.ReplicaState) error {
	return d.dataStorage.SaveShardMetadata([]storage.ShardMetadata{storage.ShardMetadata{
		ShardID:  shard.ID,
		LogIndex: index,
		Metadata: protoc.MustMarshal(&meta.ShardLocalState{
			State: state,
			Shard: shard,
		}),
	}})
}