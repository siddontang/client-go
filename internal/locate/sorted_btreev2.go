// Copyright 2022 TiKV Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package locate

import (
	"bytes"
	"time"

	"github.com/tidwall/btree"
	"github.com/tikv/client-go/v2/internal/logutil"
	"go.uber.org/zap"
)

// SortedRegionsV2 is a sorted btree.
type SortedRegionsV2 struct {
	b *btree.BTreeG[*btreeItem]
}

// NewSortedRegions returns a new SortedRegions.
func NewSortedRegionsV2(_ int) *SortedRegionsV2 {
	return &SortedRegionsV2{
		b: btree.NewBTreeG(
			func(a, b *btreeItem) bool { return a.Less(b) }),
	}
}

// ReplaceOrInsert inserts a new item into the btree.
func (s *SortedRegionsV2) ReplaceOrInsert(cachedRegion *Region) *Region {
	old, ok := s.b.Set(newBtreeItem(cachedRegion))
	if !ok {
		return nil
	}

	return old.cachedRegion
}

// SearchByKey returns the region which contains the key. Note that the region might be expired and it's caller's duty to check the region TTL.
func (s *SortedRegionsV2) SearchByKey(key []byte, isEndKey bool) (r *Region) {
	s.b.Descend(newBtreeSearchItem(key), func(item *btreeItem) bool {
		region := item.cachedRegion
		if isEndKey && bytes.Equal(region.StartKey(), key) {
			return true // iterate next item
		}
		if !isEndKey && region.Contains(key) || isEndKey && region.ContainsByEnd(key) {
			r = region
		}
		return false
	})
	return
}

// AscendGreaterOrEqual returns all items that are greater than or equal to the key.
// It is the caller's responsibility to make sure that startKey is a node in the B-tree, otherwise, the startKey will not be included in the return regions.
func (s *SortedRegionsV2) AscendGreaterOrEqual(startKey, endKey []byte, limit int) (regions []*Region) {
	now := time.Now().Unix()
	lastStartKey := startKey

	s.b.Ascend(newBtreeSearchItem(startKey), func(item *btreeItem) bool {
		region := item.cachedRegion
		if len(endKey) > 0 && bytes.Compare(region.StartKey(), endKey) >= 0 {
			return false
		}
		if !region.checkRegionCacheTTL(now) {
			return false
		}
		if !region.Contains(lastStartKey) { // uncached hole
			return false
		}
		lastStartKey = region.EndKey()
		regions = append(regions, region)
		return len(regions) < limit
	})
	return regions
}

// removeIntersecting removes all items that have intersection with the key range of given region.
// If the region itself is in the cache, it's not removed.
func (s *SortedRegionsV2) removeIntersecting(r *Region, verID RegionVerID) ([]*btreeItem, bool) {
	var deleted []*btreeItem
	var stale bool

	s.b.Ascend(newBtreeSearchItem(r.StartKey()), func(item *btreeItem) bool {
		if len(r.EndKey()) > 0 && bytes.Compare(item.cachedRegion.StartKey(), r.EndKey()) >= 0 {
			return false
		}
		if item.cachedRegion.meta.GetRegionEpoch().GetVersion() > verID.ver {
			logutil.BgLogger().Debug("get stale region",
				zap.Uint64("region", verID.GetID()), zap.Uint64("ver", verID.GetVer()), zap.Uint64("conf", verID.GetConfVer()),
				zap.Uint64("intersecting-ver", item.cachedRegion.meta.GetRegionEpoch().GetVersion()))
			stale = true
			return false
		}
		deleted = append(deleted, item)
		return true
	})
	if stale {
		return nil, true
	}
	for _, item := range deleted {
		s.b.Delete(item)
	}
	return deleted, false
}

// Clear removes all items from the btree.
func (s *SortedRegionsV2) Clear() {
	s.b.Clear()
}

// ValidRegionsInBtree returns the number of valid regions in the btree.
func (s *SortedRegionsV2) ValidRegionsInBtree(ts int64) (len int) {
	s.b.Reverse(func(item *btreeItem) bool {
		r := item.cachedRegion
		if !r.checkRegionCacheTTL(ts) {
			return true
		}
		len++
		return true
	})
	return
}