// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package state

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/asim/config"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/asim/workload"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/liveness/livenesspb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/load"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/stretchr/testify/require"
)

func TestStateUpdates(t *testing.T) {
	s := NewState(config.DefaultSimulationSettings())
	node := s.AddNode()
	s.AddStore(node.NodeID())
	require.Equal(t, 1, len(s.Nodes()))
	require.Equal(t, 1, len(s.Stores()))
}

// TestRangeSplit asserts that splitting the first range creates new replicas
// for any replicas that existed on the pre-split range. It also checks that
// the post-split keys are correct.
func TestRangeSplit(t *testing.T) {
	s := newState(config.DefaultSimulationSettings())
	k1 := MinKey
	r1 := s.rangeFor(k1)

	n1 := s.AddNode()
	s1, _ := s.AddStore(n1.NodeID())

	repl1, _ := s.AddReplica(r1.RangeID(), s1.StoreID())

	// Set the replica load of the existing replica to 100 write keys, to assert
	// on the post split 50/50 load distribution.
	s.load[r1.rangeID].ApplyLoad(workload.LoadEvent{Writes: 100, Reads: 100, WriteSize: 100, ReadSize: 100})

	k2 := Key(1)
	lhs, rhs, ok := s.SplitRange(k2)
	require.True(t, ok)

	// The lhs inherits the pre-split range id.
	require.Equal(t, lhs.RangeID(), r1.RangeID())
	// The end key of the lhs should be the start key of the rhs.
	require.Equal(t, lhs.Descriptor().EndKey, rhs.Descriptor().StartKey)
	// The lhs inherits the pre-split replicas.
	lhsRepl, ok := lhs.Replica(s1.StoreID())
	require.True(t, ok)
	require.Equal(t, repl1, lhsRepl)
	// The rhs should have a replica added to it as well. It should hold the
	// lease if the lhs replica does.
	newRepl, ok := rhs.Replica(s1.StoreID())
	require.True(t, ok)
	require.Equal(t, repl1.HoldsLease(), newRepl.HoldsLease())
	// Assert that the lhs now has half the previous load counters.
	lhsLoad := s.load[lhs.RangeID()].(*ReplicaLoadCounter)
	rhsLoad := s.load[rhs.RangeID()].(*ReplicaLoadCounter)
	lhsQPS := lhsLoad.loadStats.TestingGetSum(load.Queries)
	rhsQPS := rhsLoad.loadStats.TestingGetSum(load.Queries)
	require.Equal(t, int64(50), lhsLoad.ReadKeys)
	require.Equal(t, int64(50), lhsLoad.WriteKeys)
	require.Equal(t, int64(50), lhsLoad.WriteBytes)
	require.Equal(t, int64(50), lhsLoad.ReadBytes)
	require.Equal(t, float64(100), lhsQPS)
	// Assert that the rhs load is identical to the lhs load.
	require.Equal(t, lhsLoad.ReadKeys, rhsLoad.ReadKeys)
	require.Equal(t, lhsLoad.WriteKeys, rhsLoad.WriteKeys)
	require.Equal(t, lhsLoad.WriteBytes, lhsLoad.WriteBytes)
	require.Equal(t, lhsLoad.ReadBytes, rhsLoad.ReadBytes)
	require.Equal(t, lhsQPS, rhsQPS)
}

func TestRangeMap(t *testing.T) {
	s := newState(config.DefaultSimulationSettings())

	// Assert that the first range is correctly initialized upon creation of a
	// new state.
	require.Len(t, s.ranges.rangeMap, 1)
	require.Equal(t, s.ranges.rangeTree.Max(), s.ranges.rangeTree.Min())
	firstRange := s.ranges.rangeMap[1]
	require.Equal(t, s.rangeFor(MinKey), firstRange)
	require.Equal(t, firstRange.startKey, MinKey)
	require.Equal(t, firstRange.desc.StartKey, MinKey.ToRKey())
	require.Equal(t, firstRange.desc.EndKey, MaxKey.ToRKey())
	require.Equal(t, defaultSpanConfig, firstRange.SpanConfig())

	k2 := Key(1)
	k3 := Key(2)
	k4 := Key(3)

	r1, r2, ok := s.SplitRange(k2)
	require.True(t, ok)
	_, r3, ok := s.SplitRange(k3)
	require.True(t, ok)
	_, r4, ok := s.SplitRange(k4)
	require.True(t, ok)

	// Assert that the range is segmented into [MinKey, EndKey) intervals.
	require.Equal(t, k2.ToRKey(), r1.Descriptor().EndKey)
	require.Equal(t, k3.ToRKey(), r2.Descriptor().EndKey)
	require.Equal(t, k4.ToRKey(), r3.Descriptor().EndKey)

	require.Equal(t, r2.RangeID(), s.rangeFor(k2).rangeID)
	require.Equal(t, r3.RangeID(), s.rangeFor(k3).rangeID)
	require.Equal(t, r4.RangeID(), s.rangeFor(k4).rangeID)
}

// TestValidTransfer asserts that ValidTransfer behaves correctly.
func TestValidTransfer(t *testing.T) {
	s := NewState(config.DefaultSimulationSettings())

	_, r1, _ := s.SplitRange(1)

	n1 := s.AddNode()
	s1, _ := s.AddStore(n1.NodeID())
	s2, _ := s.AddStore(n1.NodeID())

	// Add replicas to store s2 on range r1.
	s.AddReplica(r1.RangeID(), s2.StoreID())

	// Transferring a lease for range that does't exist shouldn't be possible.
	require.False(t, s.ValidTransfer(100, s1.StoreID()))
	// Transferring a lease to a store that doesn't exist shouldn't be
	// possible.
	require.False(t, s.ValidTransfer(r1.RangeID(), 100))
	// Transferring a lease to a store that does not hold a replica should not
	// be possible.
	require.False(t, s.ValidTransfer(r1.RangeID(), s1.StoreID()))

	// Add replicas to store s1 on range r1.
	s.AddReplica(r1.RangeID(), s1.StoreID())

	// Transferring a lease to store s2 (from s2) should not be possible, as s2
	// already has the lease.
	require.False(t, s.ValidTransfer(r1.RangeID(), s2.StoreID()))
	// Transferring a lease to store s1 (from s2) is possible.
	require.True(t, s.ValidTransfer(r1.RangeID(), s1.StoreID()))
}

// TestTransferLease asserts that the state is correctly updated following a
// valid lease transfer.
func TestTransferLease(t *testing.T) {
	s := NewState(config.DefaultSimulationSettings())

	_, r1, _ := s.SplitRange(1)

	n1 := s.AddNode()
	s1, _ := s.AddStore(n1.NodeID())
	s2, _ := s.AddStore(n1.NodeID())

	// Add replicas to store s1,s2 on range r1.
	repl1, _ := s.AddReplica(r1.RangeID(), s1.StoreID())
	repl2, _ := s.AddReplica(r1.RangeID(), s2.StoreID())

	// Assert that the initial leaseholder is replica 1, on store 1.
	require.Equal(t, r1.Leaseholder(), repl1.ReplicaID())
	require.True(t, repl1.HoldsLease())

	s.TransferLease(r1.RangeID(), s2.StoreID())

	// Assert that replica 2 no longer thinks it is the leaseholder and that
	// both the range and replica 2 say the leaseholder is replica 2.
	require.Equal(t, r1.Leaseholder(), repl2.ReplicaID())
	require.False(t, repl1.HoldsLease())
	require.True(t, repl2.HoldsLease())
}

// TestValidReplicaTarget asserts that CanAddReplica and CanRemoveReplica
// behave correctly under various scenarios.
func TestValidReplicaTarget(t *testing.T) {
	s := NewState(config.DefaultSimulationSettings())

	_, r1, _ := s.SplitRange(1)

	n1 := s.AddNode()
	s1, _ := s.AddStore(n1.NodeID())
	s2, _ := s.AddStore(n1.NodeID())

	// Adding a replica for a range that doesn't exist should not be possilbe.
	require.False(t, s.CanAddReplica(100, s1.StoreID()))
	// Adding a replica to a store that doesn't exist should not be possible.
	require.False(t, s.CanAddReplica(r1.RangeID(), 100))
	// Adding a replica to a store, for a range that does exist is possible.
	require.True(t, s.CanAddReplica(r1.RangeID(), s1.StoreID()))

	// Removing a replica for a range that doesn't exist should not be possilbe.
	require.False(t, s.CanRemoveReplica(100, s1.StoreID()))
	// Removing a replica from a store that doesn't exist should not be possible.
	require.False(t, s.CanRemoveReplica(r1.RangeID(), 100))
	// Removing a replica from a store, for a range that both exist, however
	// the replica does not, is not possible.
	require.False(t, s.CanRemoveReplica(r1.RangeID(), s1.StoreID()))

	// Add replicas to store s1,s2 on range r1.
	s.AddReplica(r1.RangeID(), s1.StoreID())
	s.AddReplica(r1.RangeID(), s2.StoreID())

	// Ensure the lease is on s1.
	s.TransferLease(r1.RangeID(), s1.StoreID())

	// Cannot remove a replica that is the leaseholder.
	require.False(t, s.CanRemoveReplica(r1.RangeID(), s1.StoreID()))

	// Removing a replica on a valid range and store that holds a non
	// leaseholder replica is possible,
	require.True(t, s.CanRemoveReplica(r1.RangeID(), s2.StoreID()))
}

func TestAddReplica(t *testing.T) {
	s := NewState(config.DefaultSimulationSettings())

	_, r1, _ := s.SplitRange(1)
	_, r2, _ := s.SplitRange(2)

	n1 := s.AddNode()
	s1, _ := s.AddStore(n1.NodeID())
	s2, _ := s.AddStore(n1.NodeID())

	// Add two replicas on s1, one on s2.
	r1repl1, _ := s.AddReplica(r1.RangeID(), s1.StoreID())
	r2repl1, _ := s.AddReplica(r2.RangeID(), s1.StoreID())
	r2repl2, _ := s.AddReplica(r2.RangeID(), s2.StoreID())

	require.Equal(t, ReplicaID(1), r1repl1.ReplicaID())
	require.Equal(t, ReplicaID(1), r2repl1.ReplicaID())
	require.Equal(t, ReplicaID(2), r2repl2.ReplicaID())

	require.Len(t, s.Replicas(s1.StoreID()), 2)
	require.Len(t, s.Replicas(s2.StoreID()), 1)
}

// TestWorkloadApply asserts that applying workload on a key, will be reflected
// on the leaseholder for the range that key is contained within.
func TestWorkloadApply(t *testing.T) {
	s := NewState(config.DefaultSimulationSettings())

	n1 := s.AddNode()
	s1, _ := s.AddStore(n1.NodeID())
	s2, _ := s.AddStore(n1.NodeID())
	s3, _ := s.AddStore(n1.NodeID())

	_, r1, _ := s.SplitRange(100)
	_, r2, _ := s.SplitRange(1000)
	_, r3, _ := s.SplitRange(10000)

	s.AddReplica(r1.RangeID(), s1.StoreID())
	s.AddReplica(r2.RangeID(), s2.StoreID())
	s.AddReplica(r3.RangeID(), s3.StoreID())

	applyLoadToStats := func(key int64, count int) {
		for i := 0; i < count; i++ {
			s.ApplyLoad(workload.LoadBatch{workload.LoadEvent{Key: key, Writes: 1}})
		}
	}

	applyLoadToStats(100, 100)
	applyLoadToStats(1000, 1000)
	applyLoadToStats(10000, 10000)

	// Assert that the leaseholder replica load correctly matches the number of
	// requests made.
	require.Equal(t, float64(100), s.ReplicaLoad(r1.RangeID(), s1.StoreID()).Load().WritesPerSecond)
	require.Equal(t, float64(1000), s.ReplicaLoad(r2.RangeID(), s2.StoreID()).Load().WritesPerSecond)
	require.Equal(t, float64(10000), s.ReplicaLoad(r3.RangeID(), s3.StoreID()).Load().WritesPerSecond)

	expectedLoad := roachpb.StoreCapacity{WritesPerSecond: 100, LeaseCount: 1, RangeCount: 1}
	sc1 := Capacity(s, s1.StoreID())
	sc2 := Capacity(s, s2.StoreID())
	sc3 := Capacity(s, s3.StoreID())

	// Assert that the store load is also updated upon request GetStoreLoad.
	require.Equal(t, expectedLoad, sc1)
	expectedLoad.WritesPerSecond *= 10
	require.Equal(t, expectedLoad, sc2)
	expectedLoad.WritesPerSecond *= 10
	require.Equal(t, expectedLoad, sc3)
}

// TestReplicaLoadQPS asserts that the rated replica load accounting maintains
// the average per second corresponding to the tick clock.
func TestReplicaLoadQPS(t *testing.T) {
	settings := config.DefaultSimulationSettings()
	s := NewState(settings)
	start := settings.StartTime

	n1 := s.AddNode()
	k1 := Key(100)
	qps := 1000
	s1, _ := s.AddStore(n1.NodeID())
	_, r1, _ := s.SplitRange(k1)
	s.AddReplica(r1.RangeID(), s1.StoreID())

	applyLoadToStats := func(key int64, count int) {
		for i := 0; i < count; i++ {
			s.ApplyLoad(workload.LoadBatch{workload.LoadEvent{Key: key, Writes: 1}})
		}
	}

	s.TickClock(start)
	s.ReplicaLoad(r1.RangeID(), s1.StoreID()).ResetLoad()
	for i := 1; i < 100; i++ {
		applyLoadToStats(int64(k1), qps)
		s.TickClock(OffsetTick(start, int64(i)))
	}

	// Assert that the rated avg comes out to rate of queries applied per
	// second.
	require.Equal(t, float64(qps), s.ReplicaLoad(r1.RangeID(), s1.StoreID()).Load().QueriesPerSecond)
}

// TestKeyTranslation asserts that key encoding between roachpb keys and
// simulator int64 keys are correct.
func TestKeyTranslation(t *testing.T) {
	for add := Key(1); add <= MaxKey; add *= 2 {
		key := MinKey + add
		rkey := key.ToRKey()
		mappedKey := ToKey(rkey.AsRawKey())
		require.Equal(
			t,
			key,
			mappedKey,
			"unexpected mapping %d (key) -> %s (rkey) -> %d (mapped)",
			key,
			rkey,
			mappedKey,
		)
	}
}

func TestOrderedStateLists(t *testing.T) {
	assertListsOrdered := func(s State) {
		rangeIDs := []RangeID{}
		for _, rng := range s.Ranges() {
			rangeIDs = append(rangeIDs, rng.RangeID())
		}
		require.IsIncreasing(t, rangeIDs, "range list is not sorted %v", rangeIDs)

		storeIDs := []StoreID{}
		for _, store := range s.Stores() {
			storeIDs = append(storeIDs, store.StoreID())
		}
		require.IsIncreasing(t, storeIDs, "store list is not sorted %v", storeIDs)

		nodeIDs := []NodeID{}
		for _, node := range s.Nodes() {
			nodeIDs = append(nodeIDs, node.NodeID())
		}
		require.IsIncreasing(t, nodeIDs, "node list is not sorted %v", nodeIDs)

		for _, storeID := range storeIDs {
			storeRangeIDs := []RangeID{}
			for _, repl := range s.Replicas(storeID) {
				storeRangeIDs = append(storeRangeIDs, repl.Range())
			}
			require.IsIncreasing(
				t,
				storeRangeIDs,
				"replica (rangeID) list for a store is not sorted %v", storeRangeIDs,
			)
		}
	}
	settings := config.DefaultSimulationSettings()

	// Test an empty state, where there should be nothing.
	s := NewState(settings)
	assertListsOrdered(s)
	// Test an even distribution with 100 stores, 10k ranges and 1m keyspace.
	s = NewStateEvenDistribution(100, 10000, 3, 1000000, settings)
	assertListsOrdered(s)
	// Test a skewed distribution with 100 stores, 10k ranges and 1m keyspace.
	s = NewStateSkewedDistribution(100, 10000, 3, 1000000, settings)
	assertListsOrdered(s)
}

// TestNewStateDeterministic asserts that the state returned from the new state
// utility functions is deterministic.
func TestNewStateDeterministic(t *testing.T) {
	settings := config.DefaultSimulationSettings()

	testCases := []struct {
		desc       string
		newStateFn func() State
	}{
		{
			desc:       "even distribution",
			newStateFn: func() State { return NewStateEvenDistribution(7, 1400, 3, 10000, settings) },
		},
		{
			desc:       "skewed distribution",
			newStateFn: func() State { return NewStateSkewedDistribution(7, 1400, 3, 10000, settings) },
		},
		{
			desc: "replica distribution raw ",
			newStateFn: func() State {
				return NewStateWithDistribution([]float64{0.2, 0.2, 0.2, 0.2, 0.2}, 5, 3, 10000, settings)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ref := tc.newStateFn()
			for i := 0; i < 5; i++ {
				require.Equal(t, ref.Ranges(), tc.newStateFn().Ranges())
			}
		})
	}
}

// TestSplitRangeDeterministic asserts that range splits are deterministic.
func TestSplitRangeDeterministic(t *testing.T) {
	settings := config.DefaultSimulationSettings()
	run := func() (State, func(key Key) (Range, Range, bool)) {
		s := NewStateWithDistribution(
			[]float64{0.2, 0.2, 0.2, 0.2, 0.2},
			5,
			3,
			10000,
			settings,
		)
		return s, func(key Key) (Range, Range, bool) {
			return s.SplitRange(key)
		}
	}
	stateA, runA := run()
	stateB, runB := run()

	// Check that the states are initially equal.
	require.Equal(t, stateA.Ranges(), stateB.Ranges(), "initial states for testing splits are not equal")
	rand := rand.New(rand.NewSource(42))
	for i := 1; i < 1000; i++ {
		splitKey := rand.Intn(10000)
		lhsA, rhsA, okA := runA(Key(splitKey))
		lhsB, rhsB, okB := runB(Key(splitKey))
		require.Equal(t, okA, okB, "ok not equal, failed after %d splits", i)
		require.Equal(t, lhsA, lhsB, "lhs not equal, failed after %d splits", i)
		require.Equal(t, rhsA, rhsB, "rhs not equal, failed after %d splits", i)
	}
}

func TestSetNodeLiveness(t *testing.T) {
	t.Run("liveness func", func(t *testing.T) {
		s := LoadClusterInfo(
			ClusterInfoWithStoreCount(3),
			config.DefaultSimulationSettings(),
		)

		liveFn := s.NodeLivenessFn()

		s.SetNodeLiveness(1, livenesspb.NodeLivenessStatus_LIVE)
		s.SetNodeLiveness(2, livenesspb.NodeLivenessStatus_DEAD)
		s.SetNodeLiveness(3, livenesspb.NodeLivenessStatus_DECOMMISSIONED)

		// Liveness status returend should ignore time till store dead or the
		// timestamp given.
		require.Equal(t, livenesspb.NodeLivenessStatus_LIVE, liveFn(1, time.Time{}, math.MaxInt64))
		require.Equal(t, livenesspb.NodeLivenessStatus_DEAD, liveFn(2, time.Time{}, math.MaxInt64))
		require.Equal(t, livenesspb.NodeLivenessStatus_DECOMMISSIONED, liveFn(3, time.Time{}, math.MaxInt64))
	})

	t.Run("node count fn", func(t *testing.T) {
		s := LoadClusterInfo(
			ClusterInfoWithStoreCount(10),
			config.DefaultSimulationSettings(),
		)

		countFn := s.NodeCountFn()

		// Set node 1-5 as decommissioned and nodes 6-10 as dead. There should be a
		// node count of 5.
		for i := 1; i <= 5; i++ {
			s.SetNodeLiveness(NodeID(i), livenesspb.NodeLivenessStatus_DECOMMISSIONED)
		}
		for i := 6; i <= 10; i++ {
			s.SetNodeLiveness(NodeID(i), livenesspb.NodeLivenessStatus_DEAD)
		}
		require.Equal(t, 5, countFn())
	})
}

// TestTopology loads cluster configurations and checks that the topology
// output matches expectations.
func TestTopology(t *testing.T) {
	singleRegionTopology := LoadClusterInfo(SingleRegionConfig, config.DefaultSimulationSettings()).Topology()
	require.Equal(t, `US
  US_1
    └── [1 2 3 4 5]
  US_2
    └── [6 7 8 9 10]
  US_3
    └── [11 12 13 14 15]
`, singleRegionTopology.String())

	multiRegionTopology := LoadClusterInfo(MultiRegionConfig, config.DefaultSimulationSettings()).Topology()
	require.Equal(t, `EU
  EU_1
  │ └── [25 26 27 28]
  EU_2
  │ └── [29 30 31 32]
  EU_3
  │ └── [33 34 35 36]
US_East
  US_East_1
  │ └── [1 2 3 4]
  US_East_2
  │ └── [5 6 7 8]
  US_East_3
  │ └── [9 10 11 12]
US_West
  US_West_1
    └── [13 14 15 16]
  US_West_2
    └── [17 18 19 20]
  US_West_3
    └── [21 22 23 24]
`, multiRegionTopology.String())

	complexTopology := LoadClusterInfo(ComplexConfig, config.DefaultSimulationSettings()).Topology()
	require.Equal(t, `EU
  EU_1
  │ └── [19 20 21]
  EU_2
  │ └── [22 23 24]
  EU_3
  │ └── [25 26 27 28]
US_East
  US_East_1
  │ └── [1]
  US_East_2
  │ └── [2 3]
  US_East_3
  │ └── [4 5 6 7 8 9 10 11 12 13 14 15 16]
US_West
  US_West_1
    └── [17 18]
`, complexTopology.String())

}
