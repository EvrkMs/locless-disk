// Package proto defines the image format and the wire protocol shared by the
// locless server and the WinPE agent.
//
// A disk image is a chain of files: one .ldisk with the full version and any
// number of .ldelta files, each holding only the blocks that differ from its
// parent. A file is laid out like this, all integers little-endian:
//
//	header   magic(4) version(2) kind(2) blockSize(4) id(16) parent(16)
//	         parentSum(32) crc32(4)
//	blocks   { stored(4) raw(4) codec(2) reserved(2) sha256(32) crc32(4) payload }...
//	index    manifestLen(4) manifest chainLen(2) ids(16 each) count(8)
//	         { partition(4) position(4) level(2) offset(8) sha256(32) }...
//	footer   indexOffset(8) indexLen(8) sha256(32) magic(4) crc32(4)
//
// Blocks are tied to their place on the partition, not packed into a stream, so
// a change in one place leaves all other blocks identical and they can be shared
// with the parent. A block is identified by the SHA-256 of its raw data; clusters
// the file system does not use must be zeroed first, or leftovers in free space
// would make equal blocks look different.
//
// There are two kinds of nothing. A block with no entry is free space: deploy
// leaves it alone, whatever the target disk holds there. A zero entry, with an
// empty hash, is data that happens to be all zeros: it stores nothing, but deploy
// must write zeros, or old data of the target would show through and corrupt the
// file system.
//
// The index of every file describes the whole disk: each entry says which file
// of the chain holds the block, so reading never walks the chain. The index sits
// at the end so blocks can be written as a stream; a file without a valid footer
// is an unfinished capture and is never mistaken for an image.
package proto

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// File name extensions.
const (
	ExtDisk  = ".ldisk"
	ExtDelta = ".ldelta"
)

// Format constants.
const (
	imageMagic  = "LDSK"
	footerMagic = "LDND"

	// ImageVersion is the current format version.
	ImageVersion uint16 = 2

	// HeaderSize, BlockHeaderSize and FooterSize are exact wire sizes.
	HeaderSize      = 80
	BlockHeaderSize = 48
	FooterSize      = 56

	entrySize = 50

	// DefaultBlockSize suits NTFS changes: small enough to keep deltas tight,
	// large enough to keep an index small.
	DefaultBlockSize = 1 << 20
	MinBlockSize     = 64 << 10
	MaxBlockSize     = 64 << 20

	// maxManifest bounds what a damaged index can make the reader allocate.
	maxManifest = 1 << 20
)

// Errors callers can tell apart.
var (
	// ErrIncomplete means the file has no valid footer: the capture did not end.
	ErrIncomplete = errors.New("proto: image is incomplete")
	// ErrCorrupt means data failed its checksum.
	ErrCorrupt = errors.New("proto: image is corrupt")
	// ErrBadChain means the files do not form one chain.
	ErrBadChain = errors.New("proto: files do not form a chain")
)

// ImageKind tells a full image from a delta.
type ImageKind uint16

// Image kinds.
const (
	KindDisk  ImageKind = 1
	KindDelta ImageKind = 2
)

// Codec is how a block payload is stored.
type Codec uint16

// Block codecs.
const (
	CodecNone Codec = 0
	CodecZstd Codec = 1
)

// ID identifies one image file.
type ID [16]byte

// NewID returns a random ID.
func NewID() ID {
	var id ID
	rand.Read(id[:]) // never fails, see crypto/rand
	return id
}

// Sum is a SHA-256 hash.
type Sum [sha256.Size]byte

// Header is the fixed start of an image file.
type Header struct {
	Kind      ImageKind
	BlockSize uint32
	ID        ID
	Parent    ID  // zero for KindDisk
	ParentSum Sum // index checksum of the parent, zero for KindDisk
}

// Validate reports whether the header describes a readable file.
func (h Header) Validate() error {
	if h.BlockSize < MinBlockSize || h.BlockSize > MaxBlockSize || h.BlockSize&(h.BlockSize-1) != 0 {
		return fmt.Errorf("proto: bad block size %d", h.BlockSize)
	}
	if h.ID == (ID{}) {
		return errors.New("proto: image has no id")
	}
	switch h.Kind {
	case KindDisk:
		if h.Parent != (ID{}) || h.ParentSum != (Sum{}) {
			return errors.New("proto: a full image cannot have a parent")
		}
	case KindDelta:
		if h.Parent == (ID{}) {
			return errors.New("proto: a delta needs a parent")
		}
	default:
		return fmt.Errorf("proto: unknown image kind %d", h.Kind)
	}
	return nil
}

// MarshalBinary encodes the header with its checksum.
func (h Header) MarshalBinary() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, HeaderSize)
	copy(b, imageMagic)
	binary.LittleEndian.PutUint16(b[4:], ImageVersion)
	binary.LittleEndian.PutUint16(b[6:], uint16(h.Kind))
	binary.LittleEndian.PutUint32(b[8:], h.BlockSize)
	copy(b[12:], h.ID[:])
	copy(b[28:], h.Parent[:])
	copy(b[44:], h.ParentSum[:])
	binary.LittleEndian.PutUint32(b[76:], crc32.ChecksumIEEE(b[:76]))
	return b, nil
}

// ParseHeader decodes and validates a header.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: short header", ErrIncomplete)
	}
	if string(b[:4]) != imageMagic {
		return Header{}, errors.New("proto: not a locless image")
	}
	if binary.LittleEndian.Uint32(b[76:]) != crc32.ChecksumIEEE(b[:76]) {
		return Header{}, fmt.Errorf("%w: header checksum", ErrCorrupt)
	}
	if v := binary.LittleEndian.Uint16(b[4:]); v != ImageVersion {
		return Header{}, fmt.Errorf("proto: unsupported image version %d", v)
	}
	h := Header{
		Kind:      ImageKind(binary.LittleEndian.Uint16(b[6:])),
		BlockSize: binary.LittleEndian.Uint32(b[8:]),
	}
	copy(h.ID[:], b[12:])
	copy(h.Parent[:], b[28:])
	copy(h.ParentSum[:], b[44:])
	return h, h.Validate()
}

// BlockHeader precedes every stored block. Hash covers the raw data; CRC32
// covers this header and the payload, so a block can be checked before it is
// decompressed, for example as it arrives from the network.
type BlockHeader struct {
	Stored uint32 // payload size
	Raw    uint32 // size after decoding
	Codec  Codec
	Hash   Sum
	CRC32  uint32
}

// MarshalBinary encodes the block header.
func (h BlockHeader) MarshalBinary() []byte {
	b := make([]byte, BlockHeaderSize)
	binary.LittleEndian.PutUint32(b[0:], h.Stored)
	binary.LittleEndian.PutUint32(b[4:], h.Raw)
	binary.LittleEndian.PutUint16(b[8:], uint16(h.Codec))
	copy(b[12:], h.Hash[:])
	binary.LittleEndian.PutUint32(b[44:], h.CRC32)
	return b
}

// ParseBlockHeader decodes a block header.
func ParseBlockHeader(b []byte) (BlockHeader, error) {
	if len(b) < BlockHeaderSize {
		return BlockHeader{}, fmt.Errorf("%w: short block header", ErrCorrupt)
	}
	h := BlockHeader{
		Stored: binary.LittleEndian.Uint32(b[0:]),
		Raw:    binary.LittleEndian.Uint32(b[4:]),
		Codec:  Codec(binary.LittleEndian.Uint16(b[8:])),
		CRC32:  binary.LittleEndian.Uint32(b[44:]),
	}
	copy(h.Hash[:], b[12:])
	return h, nil
}

// checksum computes the CRC32 of the header fields and the payload.
func (h BlockHeader) checksum(payload []byte) uint32 {
	return crc32.Update(crc32.ChecksumIEEE(h.MarshalBinary()[:44]), crc32.IEEETable, payload)
}

// Verify checks the header and the payload against the stored checksum.
func (h BlockHeader) Verify(payload []byte) error {
	if uint64(len(payload)) != uint64(h.Stored) {
		return fmt.Errorf("%w: block payload is %d bytes, header says %d", ErrCorrupt, len(payload), h.Stored)
	}
	if h.checksum(payload) != h.CRC32 {
		return fmt.Errorf("%w: block checksum", ErrCorrupt)
	}
	return nil
}

var (
	codecOnce sync.Once
	encoder   *zstd.Encoder
	decoder   *zstd.Decoder
	errCodec  error
)

// codecs returns the shared zstd state. EncodeAll and DecodeAll are safe for
// concurrent use, so blocks can be compressed in parallel.
func codecs() (*zstd.Encoder, *zstd.Decoder, error) {
	codecOnce.Do(func() {
		encoder, errCodec = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if errCodec != nil {
			return
		}
		// Without the concurrency option DecodeAll runs on four cores at most.
		decoder, errCodec = zstd.NewReader(nil, zstd.WithDecoderMaxMemory(2*MaxBlockSize), zstd.WithDecoderConcurrency(0))
	})
	return encoder, decoder, errCodec
}

// HashBlock returns the identity of a block.
func HashBlock(raw []byte) Sum { return sha256.Sum256(raw) }

// EncodeBlock compresses a block. Data that does not shrink is stored as it is.
func EncodeBlock(raw []byte) (BlockHeader, []byte, error) {
	return EncodeHashed(raw, HashBlock(raw))
}

// EncodeHashed is EncodeBlock for a caller that already has the hash of raw.
func EncodeHashed(raw []byte, hash Sum) (BlockHeader, []byte, error) {
	return EncodeHashedInto(nil, raw, hash)
}

// EncodeHashedInto is EncodeHashed that compresses into dst, an empty slice the
// caller reuses; with MaxEncodedSize of room it never has to grow. The payload is
// dst or, for data that does not shrink, raw itself.
func EncodeHashedInto(dst, raw []byte, hash Sum) (BlockHeader, []byte, error) {
	enc, _, err := codecs()
	if err != nil {
		return BlockHeader{}, nil, err
	}
	if dst == nil {
		// Room for the worst case: a buffer that has to grow is copied each time,
		// which on data that does not compress cost more than the compression.
		dst = make([]byte, 0, enc.MaxEncodedSize(len(raw)))
	}
	h := BlockHeader{Raw: uint32(len(raw)), Codec: CodecZstd, Hash: hash}
	payload := enc.EncodeAll(raw, dst[:0])
	if len(payload) >= len(raw) {
		h.Codec, payload = CodecNone, raw
	}
	h.Stored = uint32(len(payload))
	h.CRC32 = h.checksum(payload)
	return h, payload, nil
}

// MaxEncodedSize is the room EncodeHashedInto may need for n bytes.
func MaxEncodedSize(n int) int {
	enc, _, err := codecs()
	if err != nil {
		return n + n/8 + 1024
	}
	return enc.MaxEncodedSize(n)
}

// DecodeBlock verifies a stored block and returns its raw data.
func DecodeBlock(h BlockHeader, payload []byte) ([]byte, error) {
	return DecodeBlockInto(nil, h, payload)
}

// DecodeBlockInto is DecodeBlock that unpacks into dst, an empty slice with room for
// the block, instead of a new one. A block stored as it is stays in payload.
func DecodeBlockInto(dst []byte, h BlockHeader, payload []byte) ([]byte, error) {
	if err := h.Verify(payload); err != nil {
		return nil, err
	}
	if h.Raw > MaxBlockSize {
		return nil, fmt.Errorf("%w: block of %d bytes", ErrCorrupt, h.Raw)
	}
	var raw []byte
	switch h.Codec {
	case CodecNone:
		raw = payload
	case CodecZstd:
		_, dec, err := codecs()
		if err != nil {
			return nil, err
		}
		if dst == nil {
			dst = make([]byte, 0, h.Raw)
		}
		if raw, err = dec.DecodeAll(payload, dst); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
	default:
		return nil, fmt.Errorf("%w: unknown codec %d", ErrCorrupt, h.Codec)
	}
	if uint64(len(raw)) != uint64(h.Raw) || HashBlock(raw) != h.Hash {
		return nil, fmt.Errorf("%w: block hash", ErrCorrupt)
	}
	return raw, nil
}

// Entry says where one non-empty block of the disk lives.
type Entry struct {
	Partition uint32 // Partition.Number from the manifest
	Position  uint32 // block number within the partition
	Level     uint16 // file in the chain, 0 is the .ldisk
	Offset    uint64 // of the block header in that file
	Hash      Sum
}

// Zero reports whether the block is data of all zeros, stored as this entry alone.
func (e Entry) Zero() bool { return e.Hash == Sum{} }

// Index describes a whole disk as of one file of the chain.
type Index struct {
	Manifest Manifest
	Chain    []ID // file ids from the .ldisk to this file
	Entries  []Entry
}

// encode serializes the index.
func (x *Index) encode() ([]byte, error) {
	m, err := json.Marshal(x.Manifest)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 0, 4+len(m)+2+16*len(x.Chain)+8+entrySize*len(x.Entries))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(m)))
	b = append(b, m...)
	b = binary.LittleEndian.AppendUint16(b, uint16(len(x.Chain)))
	for _, id := range x.Chain {
		b = append(b, id[:]...)
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(len(x.Entries)))
	for _, e := range x.Entries {
		b = binary.LittleEndian.AppendUint32(b, e.Partition)
		b = binary.LittleEndian.AppendUint32(b, e.Position)
		b = binary.LittleEndian.AppendUint16(b, e.Level)
		b = binary.LittleEndian.AppendUint64(b, e.Offset)
		b = append(b, e.Hash[:]...)
	}
	return b, nil
}

// parseIndex decodes an index whose checksum already matched.
func parseIndex(b []byte) (*Index, error) {
	bad := fmt.Errorf("%w: malformed index", ErrCorrupt)
	if len(b) < 4 {
		return nil, bad
	}
	n := uint64(binary.LittleEndian.Uint32(b))
	b = b[4:]
	if n > maxManifest || n > uint64(len(b)) {
		return nil, bad
	}
	x := &Index{}
	if err := json.Unmarshal(b[:n], &x.Manifest); err != nil {
		return nil, bad
	}
	b = b[n:]
	if len(b) < 2 {
		return nil, bad
	}
	chain := int(binary.LittleEndian.Uint16(b))
	b = b[2:]
	if len(b) < chain*16+8 {
		return nil, bad
	}
	x.Chain = make([]ID, chain)
	for i := range x.Chain {
		copy(x.Chain[i][:], b[i*16:])
	}
	b = b[chain*16:]
	count := binary.LittleEndian.Uint64(b)
	b = b[8:]
	if count != uint64(len(b))/entrySize || uint64(len(b))%entrySize != 0 {
		return nil, bad
	}
	x.Entries = make([]Entry, count)
	for i := range x.Entries {
		e := &x.Entries[i]
		e.Partition = binary.LittleEndian.Uint32(b[0:])
		e.Position = binary.LittleEndian.Uint32(b[4:])
		e.Level = binary.LittleEndian.Uint16(b[8:])
		e.Offset = binary.LittleEndian.Uint64(b[10:])
		copy(e.Hash[:], b[18:])
		b = b[entrySize:]
	}
	return x, nil
}

// place is where a known block is stored.
type place struct {
	level  uint16
	offset uint64
}

// Writer creates an image file. Blocks arrive through WriteBlock, already
// encoded by the agent, or through Add; Put then places them on the disk.
type Writer struct {
	w      io.Writer
	header Header
	chain  []ID
	offset uint64
	known  map[Sum]place
	seen   map[[2]uint32]bool
	index  []Entry
}

// NewWriter starts a file. parent is nil for a .ldisk; for a .ldelta it is the
// index of the file named in h.Parent, whose blocks need not be sent again.
func NewWriter(w io.Writer, h Header, parent *Index) (*Writer, error) {
	b, err := h.MarshalBinary()
	if err != nil {
		return nil, err
	}
	iw := &Writer{w: w, header: h, known: map[Sum]place{}, seen: map[[2]uint32]bool{}}
	if h.Kind == KindDelta {
		if parent == nil || len(parent.Chain) == 0 || parent.Chain[len(parent.Chain)-1] != h.Parent {
			return nil, fmt.Errorf("%w: delta does not match its parent", ErrBadChain)
		}
		iw.chain = slices.Clone(parent.Chain)
		for _, e := range parent.Entries {
			if !e.Zero() {
				iw.known[e.Hash] = place{e.Level, e.Offset}
			}
		}
	} else if parent != nil {
		return nil, errors.New("proto: a full image cannot have a parent")
	}
	iw.chain = append(iw.chain, h.ID)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	iw.offset = HeaderSize
	return iw, nil
}

// level is the place of this file in the chain.
func (w *Writer) level() uint16 { return uint16(len(w.chain) - 1) }

// Has reports whether a block is already stored here or in a parent. The server
// uses it to tell the agent which blocks to send.
func (w *Writer) Has(hash Sum) bool {
	_, ok := w.known[hash]
	return ok
}

// WriteBlock appends an encoded block. A block already known is skipped.
func (w *Writer) WriteBlock(h BlockHeader, payload []byte) error {
	if err := h.Verify(payload); err != nil {
		return err
	}
	if h.Raw == 0 || h.Raw > w.header.BlockSize {
		return fmt.Errorf("proto: block of %d bytes in an image of %d-byte blocks", h.Raw, w.header.BlockSize)
	}
	if w.Has(h.Hash) {
		return nil
	}
	if _, err := w.w.Write(h.MarshalBinary()); err != nil {
		return err
	}
	if _, err := w.w.Write(payload); err != nil {
		return err
	}
	w.known[h.Hash] = place{w.level(), w.offset}
	w.offset += BlockHeaderSize + uint64(len(payload))
	return nil
}

// Put says that a known block is the content of one place on the disk. An empty
// hash means the place holds zeros.
func (w *Writer) Put(partition, position uint32, hash Sum) error {
	p, ok := w.known[hash]
	if !ok && hash != (Sum{}) {
		return fmt.Errorf("proto: block %x was never written", hash[:8])
	}
	key := [2]uint32{partition, position}
	if w.seen[key] {
		return fmt.Errorf("proto: block %d of partition %d was placed twice", position, partition)
	}
	w.seen[key] = true
	w.index = append(w.index, Entry{partition, position, p.level, p.offset, hash})
	return nil
}

// Add stores data read from a used place on the disk.
func (w *Writer) Add(partition, position uint32, raw []byte) error {
	if isZero(raw) {
		return w.Put(partition, position, Sum{})
	}
	hash := HashBlock(raw)
	if !w.Has(hash) {
		h, payload, err := EncodeBlock(raw)
		if err != nil {
			return err
		}
		if err := w.WriteBlock(h, payload); err != nil {
			return err
		}
	}
	return w.Put(partition, position, hash)
}

// isZero reports whether every byte is zero.
func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// Close finishes the file. It returns the index checksum, which a child delta
// records as ParentSum. The file is an image only after this succeeds.
func (w *Writer) Close(m Manifest) (Sum, error) {
	if err := m.Validate(); err != nil {
		return Sum{}, err
	}
	if err := checkEntries(m, w.index, w.header.BlockSize); err != nil {
		return Sum{}, err
	}
	slices.SortFunc(w.index, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(a.Partition, b.Partition), cmp.Compare(a.Position, b.Position))
	})
	x := Index{Manifest: m, Chain: w.chain, Entries: w.index}
	b, err := x.encode()
	if err != nil {
		return Sum{}, err
	}
	sum := Sum(sha256.Sum256(b))
	if _, err := w.w.Write(b); err != nil {
		return Sum{}, err
	}
	f := make([]byte, FooterSize)
	binary.LittleEndian.PutUint64(f[0:], w.offset)
	binary.LittleEndian.PutUint64(f[8:], uint64(len(b)))
	copy(f[16:], sum[:])
	copy(f[48:], footerMagic)
	binary.LittleEndian.PutUint32(f[52:], crc32.ChecksumIEEE(f[:52]))
	_, err = w.w.Write(f)
	return sum, err
}

// checkEntries refuses blocks outside the partitions that store content.
func checkEntries(m Manifest, entries []Entry, blockSize uint32) error {
	blocks := map[uint32]uint64{}
	for _, p := range m.Partitions {
		if p.Content == ContentBlocks {
			blocks[p.Number] = (p.Size + uint64(blockSize) - 1) / uint64(blockSize)
		}
	}
	for _, e := range entries {
		n, ok := blocks[e.Partition]
		if !ok {
			return fmt.Errorf("proto: block for partition %d, which stores no content", e.Partition)
		}
		if uint64(e.Position) >= n {
			return fmt.Errorf("proto: block %d is past the end of partition %d", e.Position, e.Partition)
		}
	}
	return nil
}

// File is one opened image file.
type File struct {
	Header Header
	Index  *Index
	Sum    Sum // index checksum
	r      io.ReaderAt

	indexOffset uint64 // where the blocks end
}

// OpenFile reads and verifies the header, footer and index of a file.
func OpenFile(r io.ReaderAt, size int64) (*File, error) {
	if size < HeaderSize+FooterSize {
		return nil, fmt.Errorf("%w: file of %d bytes", ErrIncomplete, size)
	}
	hb := make([]byte, HeaderSize)
	if _, err := r.ReadAt(hb, 0); err != nil {
		return nil, err
	}
	h, err := ParseHeader(hb)
	if err != nil {
		return nil, err
	}
	f := make([]byte, FooterSize)
	if _, err := r.ReadAt(f, size-FooterSize); err != nil {
		return nil, err
	}
	if string(f[48:52]) != footerMagic || binary.LittleEndian.Uint32(f[52:]) != crc32.ChecksumIEEE(f[:52]) {
		return nil, ErrIncomplete
	}
	offset := binary.LittleEndian.Uint64(f[0:])
	length := binary.LittleEndian.Uint64(f[8:])
	if offset < HeaderSize || offset > uint64(size) || length != uint64(size)-FooterSize-offset {
		return nil, fmt.Errorf("%w: footer", ErrCorrupt)
	}
	b := make([]byte, length)
	if _, err := r.ReadAt(b, int64(offset)); err != nil {
		return nil, err
	}
	sum := Sum(sha256.Sum256(b))
	if !bytes.Equal(sum[:], f[16:48]) {
		return nil, fmt.Errorf("%w: index checksum", ErrCorrupt)
	}
	x, err := parseIndex(b)
	if err != nil {
		return nil, err
	}
	if len(x.Chain) == 0 || x.Chain[len(x.Chain)-1] != h.ID {
		return nil, fmt.Errorf("%w: index belongs to another file", ErrCorrupt)
	}
	if err := x.Manifest.Validate(); err != nil {
		return nil, err
	}
	return &File{Header: h, Index: x, Sum: sum, r: r, indexOffset: offset}, nil
}

// Chain is a verified sequence of files ending in the version to read.
type Chain struct {
	files []*File

	endsOnce sync.Once
	ends     []map[uint64]uint64 // see blockEnds
}

// NewChain checks that files, from the .ldisk to the wanted version, belong
// together: each delta names its parent and the checksum of its index.
func NewChain(files ...*File) (*Chain, error) {
	if len(files) == 0 || files[0].Header.Kind != KindDisk {
		return nil, fmt.Errorf("%w: it must start with a full image", ErrBadChain)
	}
	for i, f := range files {
		if f.Header.BlockSize != files[0].Header.BlockSize {
			return nil, fmt.Errorf("%w: block sizes differ", ErrBadChain)
		}
		if i == 0 {
			continue
		}
		prev := files[i-1]
		if f.Header.Kind != KindDelta || f.Header.Parent != prev.Header.ID || f.Header.ParentSum != prev.Sum {
			return nil, fmt.Errorf("%w: file %d is not a delta of file %d", ErrBadChain, i, i-1)
		}
	}
	tip := files[len(files)-1].Index
	if len(tip.Chain) != len(files) {
		return nil, fmt.Errorf("%w: expected %d files, got %d", ErrBadChain, len(tip.Chain), len(files))
	}
	for i, f := range files {
		if tip.Chain[i] != f.Header.ID {
			return nil, fmt.Errorf("%w: file %d is not the one the index names", ErrBadChain, i)
		}
	}
	return &Chain{files: files}, nil
}

// Layer is the place of a file in its chain, 1 for the .ldisk. It is written in the
// file itself: the index lists the ids of every file up to this one, in order, so
// files cannot be mixed up or reordered without NewChain noticing.
func (f *File) Layer() int { return len(f.Index.Chain) }

// Index returns the description of the disk at the end of the chain.
func (c *Chain) Index() *Index { return c.files[len(c.files)-1].Index }

// BlockSize returns the size of a full block.
func (c *Chain) BlockSize() uint32 { return c.files[0].Header.BlockSize }

// Stored returns one stored block as it is in the file, ready to send over the
// network. The caller verifies it with DecodeBlock.
func (c *Chain) Stored(e Entry) (BlockHeader, []byte, error) {
	if e.Zero() {
		return BlockHeader{}, nil, errors.New("proto: a zero block has no stored data")
	}
	if int(e.Level) >= len(c.files) {
		return BlockHeader{}, nil, fmt.Errorf("%w: entry points past the chain", ErrCorrupt)
	}
	r := c.files[e.Level].r
	// A stored block is never larger than a raw one, so a block that ends close
	// enough is read with its header at once. A longer gap holds blocks the index
	// no longer uses, and reading them would cost more than a second read.
	if end := c.blockEnds()[e.Level][e.Offset]; end > e.Offset+BlockHeaderSize && end-e.Offset <= BlockHeaderSize+uint64(c.BlockSize()) {
		b := make([]byte, end-e.Offset)
		if _, err := r.ReadAt(b, int64(e.Offset)); err != nil {
			return BlockHeader{}, nil, err
		}
		h, err := c.checkStored(e, b[:BlockHeaderSize])
		if err != nil {
			return BlockHeader{}, nil, err
		}
		if uint64(h.Stored) > uint64(len(b)-BlockHeaderSize) {
			return BlockHeader{}, nil, fmt.Errorf("%w: block runs into the next one", ErrCorrupt)
		}
		return h, b[BlockHeaderSize : BlockHeaderSize+h.Stored], nil
	}
	hb := make([]byte, BlockHeaderSize)
	if _, err := r.ReadAt(hb, int64(e.Offset)); err != nil {
		return BlockHeader{}, nil, err
	}
	h, err := c.checkStored(e, hb)
	if err != nil {
		return BlockHeader{}, nil, err
	}
	payload := make([]byte, h.Stored)
	if _, err := r.ReadAt(payload, int64(e.Offset)+BlockHeaderSize); err != nil {
		return BlockHeader{}, nil, err
	}
	return h, payload, nil
}

func (c *Chain) checkStored(e Entry, hb []byte) (BlockHeader, error) {
	h, err := ParseBlockHeader(hb)
	if err != nil {
		return BlockHeader{}, err
	}
	if h.Hash != e.Hash || h.Stored > MaxBlockSize+MaxBlockSize/8 {
		return BlockHeader{}, fmt.Errorf("%w: block header", ErrCorrupt)
	}
	return h, nil
}

// blockEnds returns, for each file and each block the index uses, where the
// next such block starts, or the index for the last one: nothing of the block
// lies past that.
func (c *Chain) blockEnds() []map[uint64]uint64 {
	c.endsOnce.Do(func() {
		starts := make([][]uint64, len(c.files))
		for _, e := range c.Index().Entries {
			if !e.Zero() && int(e.Level) < len(c.files) {
				starts[e.Level] = append(starts[e.Level], e.Offset)
			}
		}
		c.ends = make([]map[uint64]uint64, len(c.files))
		for level, s := range starts {
			slices.Sort(s)
			s = slices.Compact(s)
			ends := make(map[uint64]uint64, len(s))
			for i, offset := range s {
				ends[offset] = c.files[level].indexOffset
				if i+1 < len(s) {
					ends[offset] = s[i+1]
				}
			}
			c.ends[level] = ends
		}
	})
	return c.ends
}

// Read returns the raw data of a block, verified against its hash.
func (c *Chain) Read(e Entry) ([]byte, error) {
	h, payload, err := c.Stored(e)
	if err != nil {
		return nil, err
	}
	return DecodeBlock(h, payload)
}
