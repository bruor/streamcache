package main

import (
	"sync"
	"testing"
)

func TestRangeSetBasicMerging(t *testing.T) {
	rs := NewRangeSet()

	// 1. Add single range [0, 100)
	rs.Add(0, 100)
	if rs.TotalBytes() != 100 {
		t.Errorf("Expected 100 bytes, got %d", rs.TotalBytes())
	}
	if !rs.Contains(0) || !rs.Contains(99) || rs.Contains(100) {
		t.Errorf("Unexpected Contains result for [0, 100)")
	}

	// 2. Add contiguous range [100, 200) -> Should merge into [0, 200)
	rs.Add(100, 200)
	if rs.TotalBytes() != 200 {
		t.Errorf("Expected 200 bytes after adjacent add, got %d", rs.TotalBytes())
	}
	slices := rs.ToSlices()
	if len(slices) != 1 || slices[0][0] != 0 || slices[0][1] != 200 {
		t.Errorf("Expected single merged range [0, 200), got %v", slices)
	}

	// 3. Add disjoint range [500, 600)
	rs.Add(500, 600)
	if rs.TotalBytes() != 300 {
		t.Errorf("Expected 300 bytes, got %d", rs.TotalBytes())
	}
	slices = rs.ToSlices()
	if len(slices) != 2 {
		t.Errorf("Expected 2 ranges, got %d: %v", len(slices), slices)
	}

	// 4. Add bridging range [150, 550) -> Should merge everything into [0, 600)
	rs.Add(150, 550)
	if rs.TotalBytes() != 600 {
		t.Errorf("Expected 600 bytes after bridge, got %d", rs.TotalBytes())
	}
	slices = rs.ToSlices()
	if len(slices) != 1 || slices[0][0] != 0 || slices[0][1] != 600 {
		t.Errorf("Expected single merged range [0, 600), got %v", slices)
	}
}

func TestRangeSetOutOrderWrites(t *testing.T) {
	rs := NewRangeSet()

	// Add tail first: [800, 1000)
	rs.Add(800, 1000)
	// Add head: [0, 200)
	rs.Add(0, 200)
	// Add middle: [400, 600)
	rs.Add(400, 600)

	slices := rs.ToSlices()
	if len(slices) != 3 {
		t.Fatalf("Expected 3 ranges, got %d", len(slices))
	}
	// Verify sorted order
	if slices[0][0] != 0 || slices[1][0] != 400 || slices[2][0] != 800 {
		t.Errorf("Ranges are not sorted properly: %v", slices)
	}

	// Verify Gap calculations
	gapStart, gapEnd, found := rs.FindNextGap(0, 1000)
	if !found || gapStart != 200 || gapEnd != 400 {
		t.Errorf("Expected gap [200, 400), got [%d, %d), found=%t", gapStart, gapEnd, found)
	}

	gapStart, gapEnd, found = rs.FindNextGap(300, 1000)
	if !found || gapStart != 300 || gapEnd != 400 {
		t.Errorf("Expected gap [300, 400), got [%d, %d), found=%t", gapStart, gapEnd, found)
	}

	gapStart, gapEnd, found = rs.FindNextGap(600, 1000)
	if !found || gapStart != 600 || gapEnd != 800 {
		t.Errorf("Expected gap [600, 800), got [%d, %d), found=%t", gapStart, gapEnd, found)
	}

	// Fill gaps
	rs.Add(200, 400)
	rs.Add(600, 800)

	if !rs.IsComplete(1000) {
		t.Errorf("Expected RangeSet to be complete for 1000 bytes, got ranges: %v", rs.ToSlices())
	}
}

func TestRangeSetHasAvailableAndAvailableFrom(t *testing.T) {
	rs := NewRangeSet()
	rs.Add(100, 500)

	if !rs.HasAvailable(100, 400) {
		t.Errorf("Expected [100, 500) to be available")
	}
	if !rs.HasAvailable(150, 100) {
		t.Errorf("Expected [150, 250) to be available")
	}
	if rs.HasAvailable(50, 100) {
		t.Errorf("Did not expect [50, 150) to be available")
	}
	if rs.HasAvailable(400, 200) {
		t.Errorf("Did not expect [400, 600) to be available")
	}

	if rs.AvailableFrom(100) != 400 {
		t.Errorf("Expected AvailableFrom(100) == 400, got %d", rs.AvailableFrom(100))
	}
	if rs.AvailableFrom(200) != 300 {
		t.Errorf("Expected AvailableFrom(200) == 300, got %d", rs.AvailableFrom(200))
	}
	if rs.AvailableFrom(50) != 0 {
		t.Errorf("Expected AvailableFrom(50) == 0, got %d", rs.AvailableFrom(50))
	}
	if rs.AvailableFrom(500) != 0 {
		t.Errorf("Expected AvailableFrom(500) == 0, got %d", rs.AvailableFrom(500))
	}
}

func TestRangeSetConcurrentAccess(t *testing.T) {
	rs := NewRangeSet()
	var wg sync.WaitGroup

	// 10 concurrent writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			start := int64(workerID * 1000)
			for j := int64(0); j < 1000; j += 100 {
				rs.Add(start+j, start+j+100)
			}
		}(i)
	}

	// 10 concurrent readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = rs.TotalBytes()
				_ = rs.Contains(500)
				_ = rs.HasAvailable(0, 100)
				_ = rs.ToSlices()
			}
		}()
	}

	wg.Wait()

	if !rs.IsComplete(10000) {
		t.Errorf("Expected complete 10000 bytes after concurrent writes, got total=%d ranges=%v",
			rs.TotalBytes(), rs.ToSlices())
	}
}

func TestRangeSetAddAndCount(t *testing.T) {
	rs := NewRangeSet()

	// 1. Initial fresh write: [0, 100) -> +100
	n := rs.AddAndCount(0, 100)
	if n != 100 {
		t.Fatalf("Expected 100 new bytes, got %d", n)
	}
	if rs.TotalBytes() != 100 {
		t.Fatalf("Expected total 100, got %d", rs.TotalBytes())
	}

	// 2. Exact duplicate retry (e.g. reconnect): [0, 100) -> +0
	n = rs.AddAndCount(0, 100)
	if n != 0 {
		t.Fatalf("Expected 0 new bytes on exact duplicate retry, got %d", n)
	}

	// 3. Inner subset (e.g. retry overlap): [20, 80) -> +0
	n = rs.AddAndCount(20, 80)
	if n != 0 {
		t.Fatalf("Expected 0 new bytes on inner subset retry, got %d", n)
	}

	// 4. Forward overlap extension: [80, 150) -> +50 new bytes
	n = rs.AddAndCount(80, 150)
	if n != 50 {
		t.Fatalf("Expected 50 new bytes on forward extension, got %d", n)
	}
	if rs.TotalBytes() != 150 {
		t.Fatalf("Expected total 150, got %d", rs.TotalBytes())
	}

	// 5. Disjoint block downstream: [500, 600) -> +100 new bytes
	n = rs.AddAndCount(500, 600)
	if n != 100 {
		t.Fatalf("Expected 100 new bytes on disjoint block, got %d", n)
	}
	if rs.TotalBytes() != 250 {
		t.Fatalf("Expected total 250, got %d", rs.TotalBytes())
	}

	// 6. Bridging block spanning [100, 550) -> previous was [0, 150) and [500, 600)
	// New coverage will be [0, 600) = 600 total. Previous total was 250. Delta = 350.
	n = rs.AddAndCount(100, 550)
	if n != 350 {
		t.Fatalf("Expected 350 new bytes on bridge merge, got %d", n)
	}
	if rs.TotalBytes() != 600 {
		t.Fatalf("Expected total 600, got %d", rs.TotalBytes())
	}

	// 7. Invalid ranges (start >= end) -> 0
	if rs.AddAndCount(50, 50) != 0 {
		t.Fatalf("Expected 0 new bytes for zero-length range")
	}
	if rs.AddAndCount(100, 50) != 0 {
		t.Fatalf("Expected 0 new bytes for inverted range")
	}
}
