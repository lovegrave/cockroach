// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Ben Darnell

package multiraft

import (
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
)

var rand = util.NewPseudoRand()

func makeCommandID() string {
	return util.RandString(rand, commandIDLen)
}

type testCluster struct {
	t        *testing.T
	nodes    []*state
	tickers  []*manualTicker
	events   []*eventDemux
	storages []*BlockableStorage
}

func newTestCluster(size int, t *testing.T) *testCluster {
	transport := NewLocalRPCTransport()
	cluster := &testCluster{t: t}
	for i := 0; i < size; i++ {
		ticker := newManualTicker()
		storage := &BlockableStorage{storage: NewMemoryStorage()}
		config := &Config{
			Transport:              transport,
			Storage:                storage,
			Ticker:                 ticker,
			ElectionTimeoutTicks:   1,
			HeartbeatIntervalTicks: 1,
			TickInterval:           time.Millisecond,
			Strict:                 true,
		}
		mr, err := NewMultiRaft(uint64(i+1), config)
		if err != nil {
			t.Fatal(err)
		}
		state := newState(mr)
		demux := newEventDemux(state.Events)
		demux.start()
		cluster.nodes = append(cluster.nodes, state)
		cluster.tickers = append(cluster.tickers, ticker)
		cluster.events = append(cluster.events, demux)
		cluster.storages = append(cluster.storages, storage)
	}
	// Let all the states listen before starting any.
	for _, node := range cluster.nodes {
		go node.start()
	}
	return cluster
}

func (c *testCluster) stop() {
	for _, node := range c.nodes {
		node.Stop()
	}
	for _, demux := range c.events {
		demux.stop()
	}
}

// createGroup replicates a group among the first numReplicas nodes in the cluster
func (c *testCluster) createGroup(groupID uint64, numReplicas int) {
	var replicaIDs []uint64
	for i := 0; i < numReplicas; i++ {
		replicaIDs = append(replicaIDs, c.nodes[i].nodeID)
	}
	for i := 0; i < numReplicas; i++ {
		gs := c.storages[i].GroupStorage(groupID)
		memStorage := gs.(*blockableGroupStorage).s.(*raft.MemoryStorage)
		memStorage.SetHardState(raftpb.HardState{
			Commit: 10,
			Term:   5,
		})
		memStorage.ApplySnapshot(raftpb.Snapshot{
			Metadata: raftpb.SnapshotMetadata{
				ConfState: raftpb.ConfState{
					Nodes: replicaIDs,
				},
				Index: 10,
				Term:  5,
			},
		})

		node := c.nodes[i]
		err := node.CreateGroup(groupID)
		if err != nil {
			c.t.Fatal(err)
		}
	}
}

// Trigger an election on node i and wait for it to complete.
// TODO(bdarnell): once we have better leader discovery and forwarding/queuing, remove this.
func (c *testCluster) waitForElection(i int) *EventLeaderElection {
	// Elections are currently triggered after ElectionTimeoutTicks+1 ticks.
	c.tickers[i].Tick()
	c.tickers[i].Tick()
	for {
		e := <-c.events[i].LeaderElection
		// Ignore events with NodeID 0; these mark elections that are in progress.
		if e.NodeID != 0 {
			return e
		}
	}
}

func TestInitialLeaderElection(t *testing.T) {
	// Run the test three times, each time triggering a different node's election clock.
	// The node that requests an election first should win.
	for leaderIndex := 0; leaderIndex < 3; leaderIndex++ {
		log.Infof("testing leader election for node %v", leaderIndex)
		cluster := newTestCluster(3, t)
		groupID := uint64(1)
		cluster.createGroup(groupID, 3)

		event := cluster.waitForElection(leaderIndex)
		if event.GroupID != groupID {
			t.Fatalf("election event had incorrect group id %v", event.GroupID)
		}
		if event.NodeID != cluster.nodes[leaderIndex].nodeID {
			t.Fatalf("expected %v to win election, but was %v", cluster.nodes[leaderIndex].nodeID,
				event.NodeID)
		}
		cluster.stop()
	}
}

func TestCommand(t *testing.T) {
	cluster := newTestCluster(3, t)
	defer cluster.stop()
	groupID := uint64(1)
	cluster.createGroup(groupID, 3)
	cluster.waitForElection(0)

	// Submit a command to the leader
	cluster.nodes[0].SubmitCommand(groupID, makeCommandID(), []byte("command"))

	// The command will be committed on each node.
	for i, events := range cluster.events {
		log.Infof("waiting for event to be committed on node %v", i)
		commit := <-events.CommandCommitted
		if string(commit.Command) != "command" {
			t.Errorf("unexpected value in committed command: %v", commit.Command)
		}
	}
}

func TestSlowStorage(t *testing.T) {
	cluster := newTestCluster(3, t)
	defer cluster.stop()
	groupID := uint64(1)
	cluster.createGroup(groupID, 3)

	cluster.waitForElection(0)

	// Block the storage on the last node.
	// TODO(bdarnell): there appear to still be issues if the storage is blocked during
	// the election.
	cluster.storages[2].Block()

	// Submit a command to the leader
	cluster.nodes[0].SubmitCommand(groupID, makeCommandID(), []byte("command"))

	// Even with the third node blocked, the other nodes can make progress.
	for i := 0; i < 2; i++ {
		events := cluster.events[i]
		log.Infof("waiting for event to be commited on node %v", i)
		commit := <-events.CommandCommitted
		if string(commit.Command) != "command" {
			t.Errorf("unexpected value in committed command: %v", commit.Command)
		}
	}

	// Ensure that node 2 is in fact blocked.
	time.Sleep(time.Millisecond)
	select {
	case commit := <-cluster.events[2].CommandCommitted:
		t.Errorf("didn't expect commits on node 2 but got %v", commit)
	default:
	}

	// After unblocking the third node, it will catch up.
	cluster.storages[2].Unblock()
	cluster.tickers[0].Tick()
	log.Infof("waiting for event to be commited on node 2")
	commit := <-cluster.events[2].CommandCommitted
	if string(commit.Command) != "command" {
		t.Errorf("unexpected value in committed command: %v", commit.Command)
	}
}

func TestMembershipChange(t *testing.T) {
	t.Skip("TODO(bdarnell): arrange for createGroup to be called on joining nodes")
	cluster := newTestCluster(4, t)
	defer cluster.stop()

	// Create a group with a single member, cluster.nodes[0].
	groupID := uint64(1)
	cluster.createGroup(groupID, 1)
	cluster.waitForElection(0)

	// Add each of the other three nodes to the cluster.
	for i := 1; i < 4; i++ {
		ch := cluster.nodes[0].ChangeGroupMembership(groupID, makeCommandID(),
			raftpb.ConfChangeAddNode,
			cluster.nodes[i].nodeID)
		<-ch
	}
}
