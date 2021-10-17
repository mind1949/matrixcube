// Copyright 2021 MatrixOrigin.
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
	"testing"

	"github.com/fagongzi/util/task"
	"github.com/matrixorigin/matrixcube/pb/meta"
	"github.com/stretchr/testify/assert"
	"go.etcd.io/etcd/raft/v3/raftpb"
	"go.uber.org/zap"
)

func TestHandleSplitCheck(t *testing.T) {
	cases := []struct {
		pr        *replica
		action    action
		hasAction bool
	}{
		{
			pr:        &replica{leaderID: 1, startedC: make(chan struct{}), actions: task.New(32)},
			hasAction: false,
		},
		{
			pr:        &replica{startedC: make(chan struct{}), actions: task.New(32)},
			hasAction: false,
		},
		{
			pr:        &replica{startedC: make(chan struct{}), sizeDiffHint: 1024 * 1024 * 1024, actions: task.New(32)},
			hasAction: true,
			action:    action{actionType: checkSplitAction},
		},
	}

	for idx, c := range cases {
		s := NewSingleTestClusterStore(t, WithDisableTestParallel()).GetStore(0).(*store)
		c.pr.store = s
		c.pr.sm = &stateMachine{}
		c.pr.sm.metadataMu.shard = Shard{}
		close(c.pr.startedC)
		s.addReplica(c.pr)
		s.handleSplitCheck()
		assert.Equal(t, c.hasAction, c.pr.actions.Len() > 0, "index %d", idx)
		if c.hasAction {
			v, err := c.pr.actions.Peek()
			assert.NoError(t, err, "index %d", idx)
			assert.Equal(t, c.action, v, "index %d", idx)
		}
	}
}

func TestTryToCreateReplicate(t *testing.T) {
	cases := []struct {
		name       string
		pr         *replica
		start, end []byte
		msg        meta.RaftMessage
		ok         bool
		checkCache bool
	}{
		{
			name: "normal",
			pr:   &replica{shardID: 1, replica: Replica{ID: 1}},
			msg:  meta.RaftMessage{To: Replica{ID: 1}, ShardID: 1},
			ok:   true,
		},

		{
			name: "msg stale",
			pr:   &replica{shardID: 1, replica: Replica{ID: 2}},
			msg:  meta.RaftMessage{To: Replica{ID: 1}, ShardID: 1},
			ok:   false,
		},
		{
			name: "current stale",
			pr:   &replica{shardID: 1, replica: Replica{ID: 1}},
			msg:  meta.RaftMessage{To: Replica{ID: 2}, ShardID: 1},
			ok:   false,
		},
		{
			name: "not create raft message type",
			msg:  meta.RaftMessage{To: Replica{ID: 2}, ShardID: 1, Message: raftpb.Message{Type: raftpb.MsgApp}},
			ok:   false,
		},
		{
			name:       "create raft message type, has overlap",
			pr:         &replica{shardID: 2, replica: Replica{ID: 1}},
			start:      []byte("a"),
			end:        []byte("c"),
			msg:        meta.RaftMessage{To: Replica{ID: 2}, ShardID: 1, Message: raftpb.Message{Type: raftpb.MsgVote}, Start: []byte("b"), End: []byte("c")},
			ok:         false,
			checkCache: true,
		},
		{
			name:  "create",
			pr:    &replica{shardID: 2, replica: Replica{ID: 1}},
			start: []byte("a"),
			end:   []byte("b"),
			msg:   meta.RaftMessage{To: Replica{ID: 2}, ShardID: 1, Message: raftpb.Message{Type: raftpb.MsgVote}, Start: []byte("b"), End: []byte("c")},
			ok:    true,
		},
	}

	for idx, c := range cases {
		s := NewSingleTestClusterStore(t, WithDisableTestParallel()).GetStore(0).(*store)
		if c.pr != nil {
			c.pr.startedC = make(chan struct{})
			c.pr.closedC = make(chan struct{})
			c.pr.store = s
			c.pr.logger = s.logger
			c.pr.sm = newStateMachine(c.pr.logger, s.DataStorageByGroup(0), Shard{ID: c.pr.shardID, Start: c.start, End: c.end, Replicas: []Replica{c.pr.replica}}, c.pr.replica.ID, nil)
			close(c.pr.startedC)
			s.addReplica(c.pr)
			s.updateShardKeyRange(c.pr.getShard())
		}

		assert.Equal(t, c.ok, s.tryToCreateReplicate(c.msg), "index %d", idx)
		if c.checkCache {
			msg, ok := s.removeDroppedVoteMsg(c.msg.ShardID)
			assert.True(t, ok)
			assert.Equal(t, c.msg.Message, msg)
		}
	}
}

func TestHandleGCPeerMsg(t *testing.T) {
	s := NewSingleTestClusterStore(t).GetStore(0).(*store)
	pr := &replica{shardID: 1, replica: Replica{ID: 1}}
	pr.startedC = make(chan struct{})
	pr.closedC = make(chan struct{})
	pr.store = s
	pr.logger = s.logger
	pr.sm = newStateMachine(pr.logger, s.DataStorageByGroup(0), Shard{ID: pr.shardID, Replicas: []Replica{pr.replica}}, pr.replica.ID, nil)
	close(pr.startedC)
	s.addReplica(pr)

	assert.NotNil(t, s.getReplica(1, false))
	s.handleGCPeerMsg(meta.RaftMessage{IsTombstone: true, ShardID: 1, ShardEpoch: Epoch{Version: 1}})
	assert.Nil(t, s.getReplica(1, false))
}

func TestIsRaftMsgValid(t *testing.T) {
	s := &store{meta: &containerAdapter{meta: meta.Store{ID: 1}}, logger: zap.L()}
	assert.True(t, s.isRaftMsgValid(meta.RaftMessage{To: Replica{ContainerID: 1}}))
	assert.False(t, s.isRaftMsgValid(meta.RaftMessage{To: Replica{ContainerID: 2}}))
}