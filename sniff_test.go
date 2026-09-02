package main

import (
	"encoding/binary"
	"testing"
)

func buildMp4Box(boxType string, size int64, bodySize int) []byte {
	var header []byte
	if size >= 4294967296 {
		header = make([]byte, 16)
		binary.BigEndian.PutUint32(header[0:4], 1)
		copy(header[4:8], boxType)
		binary.BigEndian.PutUint64(header[8:16], uint64(size))
	} else {
		header = make([]byte, 8)
		binary.BigEndian.PutUint32(header[0:4], uint32(size))
		copy(header[4:8], boxType)
	}
	body := make([]byte, bodySize)
	return append(header, body...)
}

func TestIsMp4FastStart(t *testing.T) {
	// 1. Optimized MP4 (moov before mdat)
	ftyp := buildMp4Box("ftyp", 16, 8)
	moov := buildMp4Box("moov", 8, 0)
	mdat := buildMp4Box("mdat", 8, 0)
	data := append(ftyp, moov...)
	data = append(data, mdat...)

	isFastStart, _, _ := IsMp4FastStart(data)
	if !isFastStart {
		t.Errorf("Expected optimized MP4, got isFastStart=false")
	}

	// 2. Non-optimized MP4 (mdat before moov)
	ftyp2 := buildMp4Box("ftyp", 16, 8)
	mdat2 := buildMp4Box("mdat", 500, 0)
	moov2 := buildMp4Box("moov", 8, 0)
	data2 := append(ftyp2, mdat2...)
	data2 = append(data2, moov2...)

	isFastStart2, moovOffset2, hasMdatSize2 := IsMp4FastStart(data2)
	if isFastStart2 {
		t.Errorf("Expected non-optimized MP4, got isFastStart=true")
	}
	expectedOffset := int64(len(ftyp2)) + 500
	if moovOffset2 != expectedOffset {
		t.Errorf("Expected moovOffset=%d, got %d", expectedOffset, moovOffset2)
	}
	if !hasMdatSize2 {
		t.Errorf("Expected hasMdatSize=true")
	}

	// 3. Non-optimized MP4 with 64-bit size (mdat size = 1, real size in next 8 bytes)
	ftyp3 := buildMp4Box("ftyp", 16, 8)
	mdat3 := buildMp4Box("mdat", 5000000000, 0) // 5GB
	moov3 := buildMp4Box("moov", 8, 0)
	data3 := append(ftyp3, mdat3...)
	data3 = append(data3, moov3...)

	isFastStart3, moovOffset3, hasMdatSize3 := IsMp4FastStart(data3)
	if isFastStart3 {
		t.Errorf("Expected non-optimized MP4, got isFastStart=true")
	}
	expectedOffset3 := int64(len(ftyp3)) + 5000000000
	if moovOffset3 != expectedOffset3 {
		t.Errorf("Expected moovOffset=%d, got %d", expectedOffset3, moovOffset3)
	}
	if !hasMdatSize3 {
		t.Errorf("Expected hasMdatSize=true")
	}

	// 4. Non-optimized MP4 with wide box preceding mdat
	ftyp4 := buildMp4Box("ftyp", 16, 8)
	wide4 := buildMp4Box("wide", 8, 0)
	mdat4 := buildMp4Box("mdat", 1000, 0)
	moov4 := buildMp4Box("moov", 8, 0)
	data4 := append(ftyp4, wide4...)
	data4 = append(data4, mdat4...)
	data4 = append(data4, moov4...)

	isFastStart4, moovOffset4, hasMdatSize4 := IsMp4FastStart(data4)
	if isFastStart4 {
		t.Errorf("Expected non-optimized MP4, got isFastStart=true")
	}
	expectedOffset4 := int64(len(ftyp4)) + int64(len(wide4)) + 1000
	if moovOffset4 != expectedOffset4 {
		t.Errorf("Expected moovOffset=%d, got %d", expectedOffset4, moovOffset4)
	}
	if !hasMdatSize4 {
		t.Errorf("Expected hasMdatSize=true")
	}
}

// Helpers for EBML generation
func makeVint(val int64) []byte {
	var size int
	if val < 0x7F {
		size = 1
	} else if val < 0x3FFF {
		size = 2
	} else if val < 0x1FFFFF {
		size = 3
	} else {
		size = 4
	}
	b := make([]byte, size)
	var mask byte = 0x80 >> (size - 1)
	temp := val
	for i := size - 1; i > 0; i-- {
		b[i] = byte(temp & 0xFF)
		temp >>= 8
	}
	b[0] = byte(temp) | mask
	return b
}

func makeVintID(id uint32) []byte {
	// Check size of ID based on its value
	if id&0xFF000000 != 0 {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, id)
		return b
	} else if id&0x00FF0000 != 0 {
		b := make([]byte, 3)
		b[0] = byte((id >> 16) & 0xFF)
		b[1] = byte((id >> 8) & 0xFF)
		b[2] = byte(id & 0xFF)
		return b
	} else if id&0x0000FF00 != 0 {
		b := make([]byte, 2)
		b[0] = byte((id >> 8) & 0xFF)
		b[1] = byte(id & 0xFF)
		return b
	} else {
		b := make([]byte, 1)
		b[0] = byte(id & 0xFF)
		return b
	}
}

func makeEbmlElement(id uint32, payload []byte) []byte {
	bID := makeVintID(id)
	bSize := makeVint(int64(len(payload)))
	res := append(bID, bSize...)
	return append(res, payload...)
}

func TestGetMkvCuesOffset(t *testing.T) {
	// EBML Header
	ebmlHeader := makeEbmlElement(ID_EBML, []byte{0x42, 0x86, 0x81, 0x01})

	// SeekHead -> Seek -> SeekID (Cues) & SeekPos (500)
	seekID := makeEbmlElement(ID_SEEKID, []byte{0x1C, 0x53, 0xBB, 0x6B})
	seekPosVal := make([]byte, 2)
	binary.BigEndian.PutUint16(seekPosVal, 500)
	seekPos := makeEbmlElement(ID_SEEKPOS, seekPosVal)
	
	seekPayload := append(seekID, seekPos...)
	seek := makeEbmlElement(ID_SEEK, seekPayload)
	seekHead := makeEbmlElement(ID_SEEKHEAD, seek)

	// Segment payload starts immediately after the Segment element size
	// Let's build Segment element
	segmentID := makeVintID(ID_SEGMENT)
	segmentSize := makeVint(2000)
	segmentHeader := append(segmentID, segmentSize...)
	
	segmentDataStart := len(ebmlHeader) + len(segmentHeader)
	
	data := append(ebmlHeader, segmentHeader...)
	data = append(data, seekHead...)

	hasCues, cuesOffset := GetMkvCuesOffset(data)
	if !hasCues {
		t.Fatalf("Expected MKV cues to be found")
	}

	expectedCuesOffset := int64(segmentDataStart) + 500
	if cuesOffset != expectedCuesOffset {
		t.Errorf("Expected cuesOffset=%d, got %d", expectedCuesOffset, cuesOffset)
	}
}

func TestGetMkvCuesOffsetPrefetchCase(t *testing.T) {
	ebmlHeader := makeEbmlElement(ID_EBML, []byte{0x42, 0x86, 0x81, 0x01})
	seekID := makeEbmlElement(ID_SEEKID, []byte{0x1C, 0x53, 0xBB, 0x6B})
	seekPosVal := []byte{0x03, 0x20} // 800
	seekPos := makeEbmlElement(ID_SEEKPOS, seekPosVal)
	seekPayload := append(seekID, seekPos...)
	seek := makeEbmlElement(ID_SEEK, seekPayload)
	seekHead := makeEbmlElement(ID_SEEKHEAD, seek)
	segmentID := makeVintID(ID_SEGMENT)
	segmentSize := makeVint(2000)
	segmentHeader := append(segmentID, segmentSize...)
	headerBytes := append(ebmlHeader, segmentHeader...)
	headerBytes = append(headerBytes, seekHead...)

	responseBytes := make([]byte, 1000)
	copy(responseBytes, headerBytes)

	hasCues, cuesOffset := GetMkvCuesOffset(responseBytes)
	t.Logf("hasCues: %v, cuesOffset: %d", hasCues, cuesOffset)
	if !hasCues {
		t.Errorf("Failed to find Cues in prefetch mock")
	}
}
