package channel

import (
	"encoding/binary"
	"io"
	"testing"
)

type virtualMP4Reader struct {
	prefix    []byte
	size      int64
	readBytes int
}

func (r *virtualMP4Reader) ReadAt(destination []byte, offset int64) (int, error) {
	if offset < 0 || offset >= r.size {
		return 0, io.EOF
	}
	requested := len(destination)
	available := r.size - offset
	if int64(len(destination)) > available {
		destination = destination[:available]
	}
	clear(destination)
	if offset < int64(len(r.prefix)) {
		copy(destination, r.prefix[offset:])
	}
	r.readBytes += len(destination)
	if len(destination) < requested {
		return len(destination), io.EOF
	}
	return len(destination), nil
}

func makeTestBox(boxType string, payload ...[]byte) []byte {
	size := 8
	for _, part := range payload {
		size += len(part)
	}
	box := make([]byte, size)
	binary.BigEndian.PutUint32(box[:4], uint32(size))
	copy(box[4:8], boxType)
	offset := 8
	for _, part := range payload {
		copy(box[offset:], part)
		offset += len(part)
	}
	return box
}

func makeLargeTestMdat(size uint64) []byte {
	header := make([]byte, 16)
	binary.BigEndian.PutUint32(header[:4], 1)
	copy(header[4:8], "mdat")
	binary.BigEndian.PutUint64(header[8:16], size)
	return header
}

func TestInspectLargeIndexedMP4UsesFixedReads(t *testing.T) {
	mvhdPayload := make([]byte, 20)
	binary.BigEndian.PutUint32(mvhdPayload[16:20], 1)
	moov := makeTestBox("moov",
		makeTestBox("mvhd", mvhdPayload),
		makeTestBox("trak"),
		makeTestBox("trak"),
	)
	prefix := append(makeTestBox("ftyp"), moov...)
	const mdatSize = uint64(5<<30) + 16
	prefix = append(prefix, makeLargeTestMdat(mdatSize)...)
	reader := &virtualMP4Reader{prefix: prefix, size: int64(len(prefix)-16) + int64(mdatSize)}

	layout, err := inspectMP4(reader, reader.size)
	if err != nil {
		t.Fatalf("inspectMP4() error = %v", err)
	}
	if err := validateOptimizedLayout(layout, reader.size, reader.size); err != nil {
		t.Fatalf("validateOptimizedLayout() error = %v", err)
	}
	if reader.readBytes > 256 {
		t.Fatalf("scanner read %d bytes from a virtual 5 GiB file", reader.readBytes)
	}
}

func TestInspectLargeInterruptedMP4UsesFixedReads(t *testing.T) {
	trackFragment := func(trackID uint32) []byte {
		tfhdPayload := make([]byte, 8)
		binary.BigEndian.PutUint32(tfhdPayload[4:8], trackID)
		return makeTestBox("traf", makeTestBox("tfhd", tfhdPayload), makeTestBox("trun"))
	}
	moov := makeTestBox("moov", makeTestBox("trak"), makeTestBox("trak"))
	moof := makeTestBox("moof", trackFragment(1), trackFragment(2))
	prefix := append(makeTestBox("ftyp"), moov...)
	prefix = append(prefix, moof...)
	const mdatSize = uint64(5<<30) + 16
	prefix = append(prefix, makeLargeTestMdat(mdatSize)...)
	reader := &virtualMP4Reader{prefix: prefix, size: int64(len(prefix)-16) + int64(mdatSize)}

	layout, err := inspectMP4(reader, reader.size)
	if err != nil {
		t.Fatalf("inspectMP4() error = %v", err)
	}
	if err := validateInterruptedLayout(layout); err != nil {
		t.Fatalf("validateInterruptedLayout() error = %v", err)
	}
	if reader.readBytes > 512 {
		t.Fatalf("scanner read %d bytes from a virtual 5 GiB file", reader.readBytes)
	}
}

func TestInspectMP4RejectsTruncatedLargeBox(t *testing.T) {
	prefix := append(makeTestBox("ftyp"), makeLargeTestMdat(uint64(5<<30))...)
	reader := &virtualMP4Reader{prefix: prefix, size: int64(len(prefix))}
	if _, err := inspectMP4(reader, reader.size); err == nil {
		t.Fatal("inspectMP4() accepted an mdat extending beyond the file")
	}
	if reader.readBytes > 64 {
		t.Fatalf("scanner read %d bytes before rejecting a truncated box", reader.readBytes)
	}
}
