package main

import (
	"sync"
)

// ByteRange represents a half-open byte interval [Start, End)
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // Exclusive
}

// Length returns the number of bytes in this range
func (r ByteRange) Length() int64 {
	if r.End <= r.Start {
		return 0
	}
	return r.End - r.Start
}

// RangeSet tracks a sorted, non-overlapping, merged collection of byte ranges
type RangeSet struct {
	mu     sync.RWMutex
	ranges []ByteRange
}

// NewRangeSet creates an empty RangeSet
func NewRangeSet() *RangeSet {
	return &RangeSet{
		ranges: make([]ByteRange, 0),
	}
}

// Add inserts a byte range [start, end) and merges overlapping or adjacent intervals.
func (rs *RangeSet) Add(start, end int64) {
	rs.AddAndCount(start, end)
}

// AddAndCount inserts a byte range [start, end), merges overlapping intervals,
// and returns the exact number of net-new bytes added to the RangeSet.
func (rs *RangeSet) AddAndCount(start, end int64) int64 {
	if start >= end {
		return 0
	}
	if start < 0 {
		start = 0
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	var prevTotal int64
	for _, r := range rs.ranges {
		prevTotal += (r.End - r.Start)
	}

	newRange := ByteRange{Start: start, End: end}
	if len(rs.ranges) == 0 {
		rs.ranges = append(rs.ranges, newRange)
		return newRange.End - newRange.Start
	}

	// Insert and merge with existing ranges
	var merged []ByteRange
	inserted := false

	for _, r := range rs.ranges {
		if inserted {
			merged = append(merged, r)
			continue
		}

		if newRange.End < r.Start {
			// newRange is strictly before r (no overlap, not adjacent)
			merged = append(merged, newRange, r)
			inserted = true
		} else if newRange.Start > r.End {
			// newRange is strictly after r
			merged = append(merged, r)
		} else {
			// Overlapping or adjacent: merge into newRange
			if r.Start < newRange.Start {
				newRange.Start = r.Start
			}
			if r.End > newRange.End {
				newRange.End = r.End
			}
		}
	}

	if !inserted {
		merged = append(merged, newRange)
	}

	// Secondary pass to merge any cascading adjacent/overlapping intervals
	finalRanges := make([]ByteRange, 0, len(merged))
	for _, r := range merged {
		if len(finalRanges) == 0 {
			finalRanges = append(finalRanges, r)
			continue
		}
		last := &finalRanges[len(finalRanges)-1]
		if r.Start <= last.End {
			if r.End > last.End {
				last.End = r.End
			}
		} else {
			finalRanges = append(finalRanges, r)
		}
	}

	rs.ranges = finalRanges

	var newTotal int64
	for _, r := range finalRanges {
		newTotal += (r.End - r.Start)
	}

	delta := newTotal - prevTotal
	if delta < 0 {
		return 0
	}
	return delta
}

// Contains returns true if the specified byte offset is covered by any range
func (rs *RangeSet) Contains(offset int64) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	for _, r := range rs.ranges {
		if offset >= r.Start && offset < r.End {
			return true
		}
		if offset < r.Start {
			break // Since ranges are sorted
		}
	}
	return false
}

// HasAvailable returns true if the entire byte range [offset, offset+length) is available
func (rs *RangeSet) HasAvailable(offset, length int64) bool {
	if length <= 0 {
		return true
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	end := offset + length
	for _, r := range rs.ranges {
		if offset >= r.Start && end <= r.End {
			return true
		}
		if offset < r.Start {
			break
		}
	}
	return false
}

// AvailableFrom returns how many contiguous bytes are available starting from offset.
// Returns 0 if offset is in a gap.
func (rs *RangeSet) AvailableFrom(offset int64) int64 {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	for _, r := range rs.ranges {
		if offset >= r.Start && offset < r.End {
			return r.End - offset
		}
		if offset < r.Start {
			break
		}
	}
	return 0
}

// TotalBytes returns the total sum of all downloaded bytes
func (rs *RangeSet) TotalBytes() int64 {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	var total int64
	for _, r := range rs.ranges {
		total += (r.End - r.Start)
	}
	return total
}

// IsComplete returns true if [0, totalSize) is fully downloaded in a single contiguous range
func (rs *RangeSet) IsComplete(totalSize int64) bool {
	if totalSize <= 0 {
		return false
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	if len(rs.ranges) == 1 && rs.ranges[0].Start == 0 && rs.ranges[0].End >= totalSize {
		return true
	}
	return false
}

// FindNextGap finds the next un-downloaded gap starting at or after fromOffset, up to totalSize.
func (rs *RangeSet) FindNextGap(fromOffset, totalSize int64) (start, end int64, found bool) {
	if fromOffset >= totalSize {
		return 0, 0, false
	}
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	curr := fromOffset
	for _, r := range rs.ranges {
		if curr < r.Start {
			// Found a gap before range r
			gapEnd := r.Start
			if gapEnd > totalSize {
				gapEnd = totalSize
			}
			return curr, gapEnd, true
		}
		if curr >= r.Start && curr < r.End {
			// curr is inside r, skip past r
			curr = r.End
		}
	}

	if curr < totalSize {
		return curr, totalSize, true
	}

	return 0, 0, false
}

// FindCoveredRange returns the range that contains offset, or the first range after offset.
func (rs *RangeSet) FindCoveredRange(offset int64) (ByteRange, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	for _, r := range rs.ranges {
		if offset >= r.Start && offset < r.End {
			return r, true
		}
		if r.Start > offset {
			return r, true
		}
	}
	return ByteRange{}, false
}

// ToSlices returns a snapshot of ranges formatted as [][]int64{{start, end}, ...}
func (rs *RangeSet) ToSlices() [][]int64 {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	out := make([][]int64, len(rs.ranges))
	for i, r := range rs.ranges {
		out[i] = []int64{r.Start, r.End}
	}
	return out
}

// Clone returns a copy of the underlying ranges slice
func (rs *RangeSet) Clone() []ByteRange {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	out := make([]ByteRange, len(rs.ranges))
	copy(out, rs.ranges)
	return out
}

// Clear removes all ranges
func (rs *RangeSet) Clear() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.ranges = make([]ByteRange, 0)
}
