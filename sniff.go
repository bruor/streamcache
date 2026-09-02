package main

import (
	"encoding/binary"
)

// IsMp4FastStart parses the start of an MP4/MOV container.
// It returns:
// - isFastStart: true if 'moov' appears before 'mdat'.
// - moovOffset: the estimated byte offset where 'moov' starts, if it is at the end.
// - hasMdatSize: true if the 'mdat' size is explicit, allowing exact calculation of the tail start.
func IsMp4FastStart(data []byte) (isFastStart bool, moovOffset int64, hasMdatSize bool) {
	var currentOffset int64 = 0
	length := int64(len(data))

	for currentOffset+8 <= length {
		boxSize := int64(binary.BigEndian.Uint32(data[currentOffset : currentOffset+4]))
		boxType := string(data[currentOffset+4 : currentOffset+8])

		if boxSize == 1 {
			// 64-bit size box
			if currentOffset+16 > length {
				// We don't have enough data to read the 64-bit size.
				// Since we saw mdat size of 1, it's not faststart (mdat is here).
				return false, 0, false
			}
			boxSize = int64(binary.BigEndian.Uint64(data[currentOffset+8 : currentOffset+16]))
		}

		if boxType == "moov" {
			// Found moov before mdat!
			return true, 0, true
		}

		if boxType == "mdat" {
			// Found mdat first, meaning moov is at the end.
			if boxSize <= 0 {
				// Box goes to EOF, so moov cannot follow mdat
				return false, 0, false
			}
			// moovOffset is right after mdat
			return false, currentOffset + boxSize, true
		}

		if boxSize <= 0 {
			break
		}
		currentOffset += boxSize
	}

	// Default fallback (e.g. invalid file or couldn't determine)
	return false, 0, false
}

const (
	ID_EBML      = 0x1A45DFA3
	ID_SEGMENT   = 0x18538067
	ID_SEEKHEAD  = 0x114D9B74
	ID_SEEK      = 0x4DBB
	ID_SEEKID    = 0x53AB
	ID_SEEKPOS   = 0x53AC
	ID_CUES      = 0x1C53BB6B
)

// ReadVint reads an EBML variable-length integer from data starting at offset.
// It returns the value (with leading size bits cleared), the number of bytes read, and an error/success boolean.
func ReadVint(data []byte, offset int) (val int64, bytesRead int, ok bool) {
	if offset >= len(data) {
		return 0, 0, false
	}
	firstByte := data[offset]
	var size int
	if firstByte&0x80 != 0 {
		size = 1
	} else if firstByte&0x40 != 0 {
		size = 2
	} else if firstByte&0x20 != 0 {
		size = 3
	} else if firstByte&0x10 != 0 {
		size = 4
	} else if firstByte&0x08 != 0 {
		size = 5
	} else if firstByte&0x04 != 0 {
		size = 6
	} else if firstByte&0x02 != 0 {
		size = 7
	} else if firstByte&0x01 != 0 {
		size = 8
	} else {
		return 0, 0, false // Invalid VINT length
	}

	if offset+size > len(data) {
		return 0, 0, false
	}

	var res int64 = 0
	// Clear the first set bit for the first byte
	mask := byte(0xFF) >> size
	res = int64(firstByte & mask)

	for i := 1; i < size; i++ {
		res = (res << 8) | int64(data[offset+i])
	}

	return res, size, true
}

// ReadVintID reads an EBML ID (variable-length integer keeping the size bits).
func ReadVintID(data []byte, offset int) (id uint32, bytesRead int, ok bool) {
	if offset >= len(data) {
		return 0, 0, false
	}
	firstByte := data[offset]
	var size int
	if firstByte&0x80 != 0 {
		size = 1
	} else if firstByte&0x40 != 0 {
		size = 2
	} else if firstByte&0x20 != 0 {
		size = 3
	} else if firstByte&0x10 != 0 {
		size = 4
	} else {
		// ID max size is 4 bytes in standard MKV
		return 0, 0, false
	}

	if offset+size > len(data) {
		return 0, 0, false
	}

	var res uint32 = 0
	for i := 0; i < size; i++ {
		res = (res << 8) | uint32(data[offset+i])
	}

	return res, size, true
}

// GetMkvCuesOffset parses the MKV/WebM EBML headers in data (first 128KB).
// It returns:
// - hasCues: true if Cues offset was successfully found.
// - cuesOffset: the absolute byte offset of the Cues element in the file.
func GetMkvCuesOffset(data []byte) (hasCues bool, cuesOffset int64) {
	offset := 0
	length := len(data)

	// Step 1: Parse EBML Header
	id, n, ok := ReadVintID(data, offset)
	if !ok || id != ID_EBML {
		return false, 0
	}
	offset += n

	size, n, ok := ReadVint(data, offset)
	if !ok {
		return false, 0
	}
	offset += n + int(size) // Skip the entire EBML header

	// Step 2: Parse Segment Header
	id, n, ok = ReadVintID(data, offset)
	if !ok || id != ID_SEGMENT {
		return false, 0
	}
	offset += n

	_, n, ok = ReadVint(data, offset)
	if !ok {
		return false, 0
	}
	offset += n
	segmentDataStart := int64(offset) // This is where relative SeekPosition offsets start!

	// Step 3: Scan child elements of Segment to find SeekHead
	for offset < length {
		childID, n, ok := ReadVintID(data, offset)
		if !ok {
			break
		}
		offset += n

		childSize, n, ok := ReadVint(data, offset)
		if !ok {
			break
		}
		offset += n

		if childID == ID_SEEKHEAD {
			// Found SeekHead! Let's parse its contents.
			seekHeadEnd := offset + int(childSize)
			if seekHeadEnd > length {
				seekHeadEnd = length
			}

			// Scan the SeekHead for Seek elements
			seekOffset := offset
			for seekOffset < seekHeadEnd {
				seekElID, sn, sok := ReadVintID(data, seekOffset)
				if !sok {
					break
				}
				seekOffset += sn

				seekElSize, sn, sok := ReadVint(data, seekOffset)
				if !sok {
					break
				}
				seekOffset += sn

				if seekElID == ID_SEEK {
					// Parse a Seek element
					seekEnd := seekOffset + int(seekElSize)
					if seekEnd > seekHeadEnd {
						seekEnd = seekHeadEnd
					}

					var currentSeekID uint32 = 0
					var currentSeekPos int64 = -1

					seekChildOffset := seekOffset
					for seekChildOffset < seekEnd {
						scID, scn, scok := ReadVintID(data, seekChildOffset)
						if !scok {
							break
						}
						seekChildOffset += scn

						scSize, scn, scok := ReadVint(data, seekChildOffset)
						if !scok {
							break
						}
						seekChildOffset += scn

						if scID == ID_SEEKID {
							// Parse binary payload of SeekID
							if seekChildOffset+int(scSize) <= length {
								var parsedID uint32 = 0
								for i := 0; i < int(scSize); i++ {
									parsedID = (parsedID << 8) | uint32(data[seekChildOffset+i])
								}
								currentSeekID = parsedID
							}
						} else if scID == ID_SEEKPOS {
							// Parse integer payload of SeekPosition
							if seekChildOffset+int(scSize) <= length {
								var parsedPos int64 = 0
								for i := 0; i < int(scSize); i++ {
									parsedPos = (parsedPos << 8) | int64(data[seekChildOffset+i])
								}
								currentSeekPos = parsedPos
							}
						}
						seekChildOffset += int(scSize)
					}

					// Check if this Seek entry points to Cues
					if currentSeekID == ID_CUES && currentSeekPos >= 0 {
						// Absolute offset = Segment data start + SeekPosition
						return true, segmentDataStart + currentSeekPos
					}
				}
				seekOffset += int(seekElSize)
			}
		}

		offset += int(childSize) // Skip to next top-level Segment child
	}

	return false, 0
}
