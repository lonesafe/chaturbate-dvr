package channel

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// MP4 validation must not decode every sample or fragment. A multi-gigabyte
// recording can contain enough trun/stbl entries for a full mp4ff decode to
// retain gigabytes of metadata and be killed by the host OOM killer. The
// scanner below only reads box headers and a few fixed-size fields.
const maxMP4Boxes = 2_000_000

type mp4BoxSpan struct {
	typ         string
	start       uint64
	end         uint64
	headerBytes uint64
}

func (b mp4BoxSpan) payloadStart() uint64 {
	return b.start + b.headerBytes
}

func (b mp4BoxSpan) payloadSize() uint64 {
	return b.end - b.payloadStart()
}

type mp4Layout struct {
	ftypCount             int
	moovCount             int
	trackCount            int
	hasMovieHeader        bool
	movieDuration         uint64
	hasMediaData          bool
	hasFragments          bool
	hasCompleteAVFragment bool
	moovBeforeMediaData   bool
}

func inspectMP4File(path string) (mp4Layout, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return mp4Layout{}, 0, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return mp4Layout{}, 0, err
	}
	layout, err := inspectMP4(file, info.Size())
	if err != nil {
		return mp4Layout{}, info.Size(), err
	}
	return layout, info.Size(), nil
}

func inspectMP4(reader io.ReaderAt, fileSize int64) (mp4Layout, error) {
	if fileSize <= 0 {
		return mp4Layout{}, fmt.Errorf("MP4 文件为空")
	}

	var layout mp4Layout
	boxCount := 0
	var firstMoovAt uint64
	var firstMdatAt uint64
	hasFirstMoov := false
	hasFirstMdat := false
	var pendingFragmentTracks bool
	var previousType string
	err := walkMP4Boxes(reader, 0, uint64(fileSize), &boxCount, func(box mp4BoxSpan) error {
		switch box.typ {
		case "ftyp":
			layout.ftypCount++
		case "moov":
			layout.moovCount++
			if !hasFirstMoov {
				firstMoovAt = box.start
				hasFirstMoov = true
			}
			if err := inspectMovieBox(reader, box, &boxCount, &layout); err != nil {
				return err
			}
		case "moof":
			layout.hasFragments = true
			tracks, err := inspectFragmentBox(reader, box, &boxCount)
			if err != nil {
				return err
			}
			pendingFragmentTracks = tracks
		case "mdat":
			if box.payloadSize() > 0 {
				layout.hasMediaData = true
				if !hasFirstMdat {
					firstMdatAt = box.start
					hasFirstMdat = true
				}
				if previousType == "moof" && pendingFragmentTracks {
					layout.hasCompleteAVFragment = true
				}
			}
			pendingFragmentTracks = false
		default:
			pendingFragmentTracks = false
		}
		previousType = box.typ
		return nil
	})
	if err != nil {
		return mp4Layout{}, err
	}
	if hasFirstMoov && hasFirstMdat {
		layout.moovBeforeMediaData = firstMoovAt < firstMdatAt
	}
	return layout, nil
}

func inspectMovieBox(reader io.ReaderAt, moov mp4BoxSpan, boxCount *int, layout *mp4Layout) error {
	return walkMP4Boxes(reader, moov.payloadStart(), moov.end, boxCount, func(box mp4BoxSpan) error {
		switch box.typ {
		case "trak":
			layout.trackCount++
		case "mvhd":
			duration, err := readMovieDuration(reader, box)
			if err != nil {
				return err
			}
			layout.hasMovieHeader = true
			layout.movieDuration = duration
		}
		return nil
	})
}

func readMovieDuration(reader io.ReaderAt, mvhd mp4BoxSpan) (uint64, error) {
	var fields [32]byte
	if mvhd.payloadSize() < 20 {
		return 0, fmt.Errorf("mvhd 数据过短：%d", mvhd.payloadSize())
	}
	if err := readMP4At(reader, fields[:20], mvhd.payloadStart()); err != nil {
		return 0, fmt.Errorf("读取 mvhd：%w", err)
	}
	switch fields[0] {
	case 0:
		return uint64(binary.BigEndian.Uint32(fields[16:20])), nil
	case 1:
		if mvhd.payloadSize() < uint64(len(fields)) {
			return 0, fmt.Errorf("版本 1 的 mvhd 数据过短：%d", mvhd.payloadSize())
		}
		if err := readMP4At(reader, fields[:], mvhd.payloadStart()); err != nil {
			return 0, fmt.Errorf("读取版本 1 的 mvhd：%w", err)
		}
		return binary.BigEndian.Uint64(fields[24:32]), nil
	default:
		return 0, fmt.Errorf("不支持的 mvhd 版本 %d", fields[0])
	}
}

func inspectFragmentBox(reader io.ReaderAt, moof mp4BoxSpan, boxCount *int) (bool, error) {
	var firstTrackID uint32
	hasFirstTrack := false
	hasDifferentTrack := false
	err := walkMP4Boxes(reader, moof.payloadStart(), moof.end, boxCount, func(box mp4BoxSpan) error {
		if box.typ != "traf" {
			return nil
		}
		trackID, valid, err := inspectTrackFragment(reader, box, boxCount)
		if err != nil {
			return err
		}
		if !valid {
			return nil
		}
		if !hasFirstTrack {
			firstTrackID = trackID
			hasFirstTrack = true
		} else if trackID != firstTrackID {
			hasDifferentTrack = true
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return hasFirstTrack && hasDifferentTrack, nil
}

func inspectTrackFragment(reader io.ReaderAt, traf mp4BoxSpan, boxCount *int) (uint32, bool, error) {
	var trackID uint32
	hasTrackHeader := false
	hasRun := false
	err := walkMP4Boxes(reader, traf.payloadStart(), traf.end, boxCount, func(box mp4BoxSpan) error {
		switch box.typ {
		case "tfhd":
			if box.payloadSize() < 8 {
				return fmt.Errorf("tfhd 数据过短：%d", box.payloadSize())
			}
			var fields [8]byte
			if err := readMP4At(reader, fields[:], box.payloadStart()); err != nil {
				return fmt.Errorf("读取 tfhd：%w", err)
			}
			trackID = binary.BigEndian.Uint32(fields[4:8])
			hasTrackHeader = trackID != 0
		case "trun":
			hasRun = true
		}
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return trackID, hasTrackHeader && hasRun, nil
}

func walkMP4Boxes(reader io.ReaderAt, start, end uint64, boxCount *int, visit func(mp4BoxSpan) error) error {
	if start > end {
		return fmt.Errorf("MP4 box 范围无效：%d..%d", start, end)
	}
	for offset := start; offset < end; {
		*boxCount++
		if *boxCount > maxMP4Boxes {
			return fmt.Errorf("MP4 包含超过 %d 个 box", maxMP4Boxes)
		}
		box, err := readMP4BoxHeader(reader, offset, end)
		if err != nil {
			return fmt.Errorf("偏移 %d 处的 box：%w", offset, err)
		}
		if err := visit(box); err != nil {
			return fmt.Errorf("偏移 %d 处的 box %s：%w", offset, box.typ, err)
		}
		offset = box.end
	}
	return nil
}

func readMP4BoxHeader(reader io.ReaderAt, offset, limit uint64) (mp4BoxSpan, error) {
	if offset > limit || limit-offset < 8 {
		return mp4BoxSpan{}, io.ErrUnexpectedEOF
	}
	var header [16]byte
	if err := readMP4At(reader, header[:8], offset); err != nil {
		return mp4BoxSpan{}, err
	}

	size := uint64(binary.BigEndian.Uint32(header[:4]))
	headerBytes := uint64(8)
	switch size {
	case 0:
		size = limit - offset
	case 1:
		if limit-offset < 16 {
			return mp4BoxSpan{}, io.ErrUnexpectedEOF
		}
		if err := readMP4At(reader, header[8:16], offset+8); err != nil {
			return mp4BoxSpan{}, err
		}
		size = binary.BigEndian.Uint64(header[8:16])
		headerBytes = 16
	}
	if size < headerBytes {
		return mp4BoxSpan{}, fmt.Errorf("box %q 大小 %d 小于头部大小 %d", string(header[4:8]), size, headerBytes)
	}
	if size > limit-offset {
		return mp4BoxSpan{}, fmt.Errorf("box %q 大小 %d 超过剩余空间 %d", string(header[4:8]), size, limit-offset)
	}
	return mp4BoxSpan{
		typ:         string(header[4:8]),
		start:       offset,
		end:         offset + size,
		headerBytes: headerBytes,
	}, nil
}

func readMP4At(reader io.ReaderAt, destination []byte, offset uint64) error {
	n, err := reader.ReadAt(destination, int64(offset))
	if n != len(destination) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

func validateInterruptedMP4(path string) error {
	layout, _, err := inspectMP4File(path)
	if err != nil {
		return err
	}
	return validateInterruptedLayout(layout)
}

func validateInterruptedLayout(layout mp4Layout) error {
	if layout.ftypCount != 1 || layout.moovCount != 1 {
		return fmt.Errorf("分片 MP4 包含 ftyp=%d、moov=%d，应各有一个", layout.ftypCount, layout.moovCount)
	}
	if layout.trackCount != 2 {
		return fmt.Errorf("分片 MP4 包含 %d 条轨道，应为 2 条", layout.trackCount)
	}
	if !layout.hasCompleteAVFragment || !layout.hasMediaData {
		return fmt.Errorf("分片 MP4 没有完整的双轨 moof/mdat 组合")
	}
	return nil
}
