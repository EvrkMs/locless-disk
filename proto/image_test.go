package proto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

const testBlock = MinBlockSize

// imageManifest is a disk with one stored partition and one that is only created.
func imageManifest() Manifest {
	m := testManifest()
	m.DiskSize = 16 * mib
	m.Partitions = []Partition{
		{Number: 1, TypeGUID: testGUID, GUID: testGUID, Offset: 1 * mib, Size: 1 * mib, Policy: PolicyFixed, Content: ContentNone},
		{Number: 3, TypeGUID: testGUID, GUID: testGUID, Offset: 2 * mib, Size: 8*mib + 512, Policy: PolicyFill, Content: ContentBlocks},
	}
	return m
}

// noise returns data that does not compress, the same for the same seed.
func noise(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, 1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// text returns data that compresses well, the same for the same seed.
func text(seed uint64, n int) []byte {
	line := strings.Repeat("locless block ", 8) + string(rune('a'+seed%26))
	return []byte(strings.Repeat(line, n/len(line)+1))[:n]
}

// disk maps a block position of partition 3 to its data.
type disk map[uint32][]byte

func write(t *testing.T, h Header, parent *File, d disk) (*File, []byte) {
	t.Helper()
	var out bytes.Buffer
	var index *Index
	if parent != nil {
		index = parent.Index
	}
	w, err := NewWriter(&out, h, index)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for pos, raw := range d {
		if err := w.Add(3, pos, raw); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if _, err := w.Close(imageManifest()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b := out.Bytes()
	f, err := OpenFile(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	return f, b
}

func base(t *testing.T, d disk) (*File, []byte) {
	t.Helper()
	return write(t, Header{Kind: KindDisk, BlockSize: testBlock, ID: NewID()}, nil, d)
}

func delta(t *testing.T, parent *File, d disk) (*File, []byte) {
	t.Helper()
	h := Header{Kind: KindDelta, BlockSize: testBlock, ID: NewID(), Parent: parent.Header.ID, ParentSum: parent.Sum}
	return write(t, h, parent, d)
}

// readAll restores the disk a chain describes.
func readAll(t *testing.T, c *Chain) disk {
	t.Helper()
	d := disk{}
	for _, e := range c.Index().Entries {
		if e.Zero() {
			d[e.Position] = nil
			continue
		}
		raw, err := c.Read(e)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		d[e.Position] = raw
	}
	return d
}

func sameDisk(t *testing.T, got, want disk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d blocks, want %d", len(got), len(want))
	}
	for pos, raw := range want {
		if !bytes.Equal(got[pos], raw) {
			t.Fatalf("block %d differs", pos)
		}
	}
}

func chain(t *testing.T, files ...*File) *Chain {
	t.Helper()
	c, err := NewChain(files...)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	return c
}

func TestHeaderGolden(t *testing.T) {
	h := Header{Kind: KindDelta, BlockSize: 1 << 20}
	h.ID[0], h.Parent[0], h.ParentSum[0] = 0xaa, 0xbb, 0xcc
	b, err := h.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	if len(b) != HeaderSize {
		t.Fatalf("%d bytes", len(b))
	}
	const want = "4c44534b" + "0200" + "0200" + "00001000" +
		"aa000000000000000000000000000000" + "bb000000000000000000000000000000" +
		"cc00000000000000000000000000000000000000000000000000000000000000"
	if got := hex.EncodeToString(b[:76]); got != want {
		t.Fatalf("header:\n got %s\nwant %s", got, want)
	}
	got, err := ParseHeader(b)
	if err != nil || got != h {
		t.Fatalf("round trip: %+v, err %v", got, err)
	}
}

func TestBlockHeaderGolden(t *testing.T) {
	h := BlockHeader{Stored: 5, Raw: 4096, Codec: CodecZstd, CRC32: 0x11223344}
	h.Hash[0] = 0xdd
	b := h.MarshalBinary()
	const want = "05000000" + "00100000" + "0100" + "0000" +
		"dd00000000000000000000000000000000000000000000000000000000000000" + "44332211"
	if got := hex.EncodeToString(b); got != want {
		t.Fatalf("block header:\n got %s\nwant %s", got, want)
	}
	if got, err := ParseBlockHeader(b); err != nil || got != h {
		t.Fatalf("round trip: %+v, err %v", got, err)
	}
}

func TestHeaderValidate(t *testing.T) {
	id := NewID()
	tests := []struct {
		name string
		h    Header
	}{
		{"no id", Header{Kind: KindDisk, BlockSize: testBlock}},
		{"unknown kind", Header{Kind: 9, BlockSize: testBlock, ID: id}},
		{"small block", Header{Kind: KindDisk, BlockSize: 4096, ID: id}},
		{"big block", Header{Kind: KindDisk, BlockSize: 128 << 20, ID: id}},
		{"odd block", Header{Kind: KindDisk, BlockSize: 100000, ID: id}},
		{"disk with parent", Header{Kind: KindDisk, BlockSize: testBlock, ID: id, Parent: id}},
		{"delta without parent", Header{Kind: KindDelta, BlockSize: testBlock, ID: id}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.h.Validate(); err == nil {
				t.Fatal("Validate accepted a bad header")
			}
		})
	}
}

func TestBlockCodec(t *testing.T) {
	compressible := text(1, testBlock)
	h, payload, err := EncodeBlock(compressible)
	if err != nil {
		t.Fatalf("EncodeBlock: %v", err)
	}
	if h.Codec != CodecZstd || len(payload) >= len(compressible) {
		t.Fatalf("codec %d, %d bytes", h.Codec, len(payload))
	}
	if raw, err := DecodeBlock(h, payload); err != nil || !bytes.Equal(raw, compressible) {
		t.Fatalf("decode: %v", err)
	}

	random := noise(1, testBlock)
	h, payload, err = EncodeBlock(random)
	if err != nil {
		t.Fatalf("EncodeBlock: %v", err)
	}
	if h.Codec != CodecNone || len(payload) != len(random) {
		t.Fatalf("incompressible data: codec %d, %d bytes", h.Codec, len(payload))
	}
	if raw, err := DecodeBlock(h, payload); err != nil || !bytes.Equal(raw, random) {
		t.Fatalf("decode: %v", err)
	}
}

func TestDecodeRejectsDamage(t *testing.T) {
	h, payload, err := EncodeBlock(text(1, testBlock))
	if err != nil {
		t.Fatalf("EncodeBlock: %v", err)
	}
	reseal := func(h BlockHeader, p []byte) BlockHeader {
		h.CRC32 = h.checksum(p)
		return h
	}

	flipped := bytes.Clone(payload)
	flipped[len(flipped)/2] ^= 0xff
	wrongHash := h
	wrongHash.Hash[0] ^= 0xff
	wrongRaw := h
	wrongRaw.Raw++
	wrongCodec := h
	wrongCodec.Codec = 7

	tests := []struct {
		name string
		h    BlockHeader
		p    []byte
	}{
		{"flipped payload", h, flipped},
		{"short payload", h, payload[:10]},
		{"changed header", wrongRaw, payload},
		{"hash of other data, crc resealed", reseal(wrongHash, payload), payload},
		{"raw size lie, crc resealed", reseal(wrongRaw, payload), payload},
		{"unknown codec, crc resealed", reseal(wrongCodec, payload), payload},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeBlock(tc.h, tc.p); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestFullImageRoundTrip(t *testing.T) {
	d := disk{0: text(1, testBlock), 5: noise(2, testBlock), 128: text(3, 512)} // the last block is short
	f, _ := base(t, d)
	c := chain(t, f)
	sameDisk(t, readAll(t, c), d)
	if c.BlockSize() != testBlock || len(c.Index().Manifest.Partitions) != 2 {
		t.Fatalf("index: %+v", c.Index().Manifest)
	}
}

// TestZeroBlocksStoreOnlyAnEntry: deploy must overwrite these places, so the index
// has to name them, but no data is kept.
func TestZeroBlocksStoreOnlyAnEntry(t *testing.T) {
	f, b := base(t, disk{0: make([]byte, testBlock), 1: make([]byte, testBlock)})
	if len(f.Index.Entries) != 2 || !f.Index.Entries[0].Zero() || !f.Index.Entries[1].Zero() {
		t.Fatalf("entries: %+v", f.Index.Entries)
	}
	if len(b) > 2048 {
		t.Fatalf("an all-zero disk took %d bytes", len(b))
	}
	if _, err := chain(t, f).Read(f.Index.Entries[0]); err == nil {
		t.Fatal("Read returned data for a zero block")
	}
}

// TestDataThatBecameZeroIsNotInherited: a delta must say zero, not fall back to
// the parent's block.
func TestDataThatBecameZeroIsNotInherited(t *testing.T) {
	f1, _ := base(t, disk{0: noise(1, testBlock)})
	f2, _ := delta(t, f1, disk{0: make([]byte, testBlock)})
	if len(f2.Index.Entries) != 1 || !f2.Index.Entries[0].Zero() {
		t.Fatalf("entries: %+v", f2.Index.Entries)
	}
	f3, _ := delta(t, f2, disk{0: noise(1, testBlock), 1: make([]byte, testBlock)})
	if len(f3.Index.Entries) != 2 || f3.Index.Entries[0].Zero() || !f3.Index.Entries[1].Zero() {
		t.Fatalf("entries: %+v", f3.Index.Entries)
	}
}

func TestSameDataIsStoredOnce(t *testing.T) {
	data := noise(1, testBlock)
	f, b := base(t, disk{0: data, 1: data, 2: data})
	if len(b) > testBlock+4096 {
		t.Fatalf("three equal blocks took %d bytes", len(b))
	}
	sameDisk(t, readAll(t, chain(t, f)), disk{0: data, 1: data, 2: data})
}

// TestDeltaHoldsOnlyChanges is the point of the format.
func TestDeltaHoldsOnlyChanges(t *testing.T) {
	v1 := disk{}
	for pos := range uint32(20) {
		v1[pos] = noise(uint64(pos), testBlock)
	}
	f1, b1 := base(t, v1)

	v2 := disk{}
	for pos, raw := range v1 {
		v2[pos] = raw
	}
	v2[7] = noise(100, testBlock)  // changed
	v2[25] = noise(101, testBlock) // added
	delete(v2, 3)                  // became empty
	f2, b2 := delta(t, f1, v2)

	if len(b2) > len(b1)/5 {
		t.Fatalf("delta is %d bytes of a %d-byte image", len(b2), len(b1))
	}
	sameDisk(t, readAll(t, chain(t, f1, f2)), v2)

	// The parent still reads as it was.
	sameDisk(t, readAll(t, chain(t, f1)), v1)
}

func TestLongChainReadsFromEveryLevel(t *testing.T) {
	d := disk{0: noise(1, testBlock), 1: noise(2, testBlock), 2: noise(3, testBlock)}
	f1, _ := base(t, d)
	files := []*File{f1}
	for i := range uint32(4) {
		next := disk{}
		for pos, raw := range d {
			next[pos] = raw
		}
		next[i+3] = noise(uint64(50+i), testBlock)
		d = next
		f, _ := delta(t, files[len(files)-1], d)
		files = append(files, f)
	}

	c := chain(t, files...)
	sameDisk(t, readAll(t, c), d)
	levels := map[uint16]bool{}
	for _, e := range c.Index().Entries {
		levels[e.Level] = true
	}
	if len(levels) != 5 {
		t.Fatalf("blocks live on %d levels, want 5", len(levels))
	}
}

// TestChangeBackStoresBlockAgain pins a deliberate limit: a delta depends only on
// the index of its parent, so a block that left the disk and came back is stored
// again. It costs one copy in a rare case and keeps merging simple.
func TestChangeBackStoresBlockAgain(t *testing.T) {
	a, b := noise(1, testBlock), noise(2, testBlock)
	f1, _ := base(t, disk{0: a})
	f2, _ := delta(t, f1, disk{0: b})
	f3, _ := delta(t, f2, disk{0: a}) // restored to the first content
	if got := f3.Index.Entries[0].Level; got != 2 {
		t.Fatalf("block lives on level %d, want 2", got)
	}
	sameDisk(t, readAll(t, chain(t, f1, f2, f3)), disk{0: a})
}

// TestBlockKeptFromGrandparent is the usual case: a block unchanged for several
// versions stays in the file that first stored it.
func TestBlockKeptFromGrandparent(t *testing.T) {
	a := noise(1, testBlock)
	f1, _ := base(t, disk{0: a})
	f2, _ := delta(t, f1, disk{0: a, 1: noise(2, testBlock)})
	f3, b3 := delta(t, f2, disk{0: a, 1: noise(3, testBlock)})
	if got := f3.Index.Entries[0].Level; got != 0 {
		t.Fatalf("block lives on level %d, want 0", got)
	}
	if len(b3) > testBlock+4096 {
		t.Fatalf("delta took %d bytes", len(b3))
	}
}

func TestUploadPathStoresOnlyMissingBlocks(t *testing.T) {
	old, changed := noise(1, testBlock), noise(2, testBlock)
	f1, _ := base(t, disk{0: old})

	var out bytes.Buffer
	h := Header{Kind: KindDelta, BlockSize: testBlock, ID: NewID(), Parent: f1.Header.ID, ParentSum: f1.Sum}
	w, err := NewWriter(&out, h, f1.Index)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if !w.Has(HashBlock(old)) || w.Has(HashBlock(changed)) {
		t.Fatal("Has does not tell what the agent must send")
	}
	bh, payload, _ := EncodeBlock(changed)
	if err := w.WriteBlock(bh, payload); err != nil {
		t.Fatalf("WriteBlock: %v", err)
	}
	if err := w.Put(3, 0, HashBlock(old)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := w.Put(3, 1, HashBlock(changed)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := w.Close(imageManifest()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f2, err := OpenFile(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	sameDisk(t, readAll(t, chain(t, f1, f2)), disk{0: old, 1: changed})
}

func TestWriterRejects(t *testing.T) {
	newWriter := func(t *testing.T) *Writer {
		t.Helper()
		w, err := NewWriter(&bytes.Buffer{}, Header{Kind: KindDisk, BlockSize: testBlock, ID: NewID()}, nil)
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		return w
	}
	raw := text(1, testBlock)

	t.Run("placing an unknown block", func(t *testing.T) {
		if err := newWriter(t).Put(3, 0, HashBlock(raw)); err == nil {
			t.Fatal("Put accepted an unknown block")
		}
	})
	t.Run("placing a position twice", func(t *testing.T) {
		w := newWriter(t)
		if err := w.Add(3, 0, raw); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := w.Add(3, 0, raw); err == nil {
			t.Fatal("Add accepted the same position twice")
		}
	})
	t.Run("damaged block from the network", func(t *testing.T) {
		h, payload, _ := EncodeBlock(raw)
		payload[0] ^= 0xff
		if err := newWriter(t).WriteBlock(h, payload); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("block larger than the image allows", func(t *testing.T) {
		h, payload, _ := EncodeBlock(text(1, testBlock*2))
		if err := newWriter(t).WriteBlock(h, payload); err == nil {
			t.Fatal("WriteBlock accepted an oversized block")
		}
	})
	t.Run("block in a partition without content", func(t *testing.T) {
		w := newWriter(t)
		if err := w.Add(1, 0, raw); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := w.Close(imageManifest()); err == nil {
			t.Fatal("Close accepted a block for an empty partition")
		}
	})
	t.Run("block past the end of the partition", func(t *testing.T) {
		w := newWriter(t)
		if err := w.Add(3, 129, raw); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if _, err := w.Close(imageManifest()); err == nil {
			t.Fatal("Close accepted a block past the end")
		}
	})
	t.Run("delta with a foreign parent", func(t *testing.T) {
		f, _ := base(t, disk{})
		h := Header{Kind: KindDelta, BlockSize: testBlock, ID: NewID(), Parent: NewID()}
		if _, err := NewWriter(&bytes.Buffer{}, h, f.Index); !errors.Is(err, ErrBadChain) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestUnfinishedFileIsNotAnImage(t *testing.T) {
	_, b := base(t, disk{0: noise(1, testBlock)})
	for _, n := range []int{0, 10, HeaderSize, len(b) / 2, len(b) - 1} {
		cut := b[:n]
		if _, err := OpenFile(bytes.NewReader(cut), int64(len(cut))); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("%d bytes: got %v, want ErrIncomplete", n, err)
		}
	}
}

func TestOpenFileRejectsDamage(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{"foreign file", func(b []byte) { b[0] = 'X' }},
		{"header", func(b []byte) { b[20] ^= 0xff }},
		{"index", func(b []byte) { b[len(b)-FooterSize-10] ^= 0xff }},
		{"footer", func(b []byte) { b[len(b)-20] ^= 0xff }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, b := base(t, disk{0: text(1, testBlock)})
			tc.mutate(b)
			if _, err := OpenFile(bytes.NewReader(b), int64(len(b))); err == nil {
				t.Fatal("OpenFile accepted a damaged file")
			}
		})
	}
}

func TestDamagedBlockIsCaughtOnRead(t *testing.T) {
	f, b := base(t, disk{0: text(1, testBlock)})
	b[HeaderSize+BlockHeaderSize+5] ^= 0xff
	if _, err := chain(t, f).Read(f.Index.Entries[0]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestChainRejects(t *testing.T) {
	f1, _ := base(t, disk{0: noise(1, testBlock)})
	other, _ := base(t, disk{0: noise(2, testBlock)})
	f2, _ := delta(t, f1, disk{0: noise(3, testBlock)})
	f3, _ := delta(t, f2, disk{0: noise(4, testBlock)})

	replaced := *f1
	replaced.Sum[0] ^= 0xff

	tests := []struct {
		name  string
		files []*File
	}{
		{"nothing", nil},
		{"starts with a delta", []*File{f2}},
		{"wrong parent", []*File{other, f2}},
		{"missing link", []*File{f1, f3}},
		{"wrong order", []*File{f1, f3, f2}},
		{"parent was rewritten", []*File{&replaced, f2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewChain(tc.files...); !errors.Is(err, ErrBadChain) {
				t.Fatalf("got %v, want ErrBadChain", err)
			}
		})
	}
}

// TestLayerIsInTheFile: the place of a file in its chain comes from the file.
func TestLayerIsInTheFile(t *testing.T) {
	f := &File{Index: &Index{Chain: []ID{NewID(), NewID(), NewID()}}}
	if f.Layer() != 3 {
		t.Fatalf("layer %d", f.Layer())
	}
}
