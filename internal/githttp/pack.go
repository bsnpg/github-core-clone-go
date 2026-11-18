// Pkt-line framing and packfile read/write for the git smart-HTTP
// surface.
//
// A pkt-line is 4 lowercase hex length bytes (payload length plus the 4
// header bytes) followed by the payload; "0000" is a flush, "0001" a
// section delimiter, "0002" a response end. A packfile is "PACK", a
// version (2), a big-endian object count, one entry per object, and a
// 20-byte SHA-1 trailer over everything before it. Each entry is a
// varint type-and-size header, then a zlib stream; delta entries add a
// base (an offset for OFS_DELTA, an oid for REF_DELTA) before the zlib
// stream, and the stored bytes are a delta against that base.

package githttp

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"

	"forge/internal/gitobj"
	"forge/internal/util"
)

// pktLineMax is git's LARGE_PACKET_MAX: the largest pkt-line including
// its 4-byte length.
const pktLineMax = 65520

var (
	pktFlush       = []byte("0000")
	pktDelim       = []byte("0001")
	pktResponseEnd = []byte("0002")
)

// packetKind is what a decoded pkt-line holds.
type packetKind int

const (
	packetData packetKind = iota
	packetFlush
	packetDelim
	packetResponseEnd
)

// packet is one decoded pkt-line. A data packet's payload slices the
// input buffer; callers never mutate it.
type packet struct {
	kind    packetKind
	payload []byte
}

// encodePktLine encodes one data packet: 4 hex length bytes (payload + 4)
// then the payload. It panics on an over-long payload; every caller
// bounds its payloads well under the limit.
func encodePktLine(payload []byte) []byte {
	if len(payload) > pktLineMax-4 {
		panic(fmt.Sprintf("pkt-line payload is %d bytes, over the limit", len(payload)))
	}
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	out = append(out, payload...)
	return out
}

// encodePktLineString is encodePktLine for a string payload.
func encodePktLineString(payload string) []byte {
	return encodePktLine([]byte(payload))
}

// decodePktLines decodes a whole buffer of pkt-lines into packets. It
// returns an error on a malformed frame.
func decodePktLines(buf []byte) ([]packet, error) {
	var packets []packet
	offset := 0
	for offset < len(buf) {
		if offset+4 > len(buf) {
			return nil, fmt.Errorf("truncated pkt-line header")
		}
		head := string(buf[offset : offset+4])
		switch head {
		case "0000":
			packets = append(packets, packet{kind: packetFlush})
			offset += 4
			continue
		case "0001":
			packets = append(packets, packet{kind: packetDelim})
			offset += 4
			continue
		case "0002":
			packets = append(packets, packet{kind: packetResponseEnd})
			offset += 4
			continue
		}
		length, err := strconv.ParseUint(head, 16, 32)
		if err != nil || length < 4 || offset+int(length) > len(buf) {
			return nil, fmt.Errorf("bad pkt-line length %q at offset %d", head, offset)
		}
		packets = append(packets, packet{kind: packetData, payload: buf[offset+4 : offset+int(length)]})
		offset += int(length)
	}
	return packets, nil
}

// -------------------------------------------------------------------------------------------
// pack writing (whole objects only, no deltas)
// -------------------------------------------------------------------------------------------

// pack type codes.
const (
	packCommit   = 1
	packTree     = 2
	packBlob     = 3
	packTag      = 4
	packOFSDelta = 6
	packREFDelta = 7
)

func packTypeCode(typ gitobj.ObjectType) int {
	switch typ {
	case gitobj.Commit:
		return packCommit
	case gitobj.Tree:
		return packTree
	case gitobj.Blob:
		return packBlob
	case gitobj.Tag:
		return packTag
	}
	return 0
}

// writePackEntryHeader renders one pack entry header (type + size varint).
func writePackEntryHeader(typeCode, size int) []byte {
	var out []byte
	first := (typeCode << 4) | (size & 0x0f)
	size >>= 4
	if size != 0 {
		first |= 0x80
	}
	out = append(out, byte(first))
	for size != 0 {
		b := size & 0x7f
		size >>= 7
		if size != 0 {
			b |= 0x80
		}
		out = append(out, byte(b))
	}
	return out
}

// writePack writes a packfile (version 2, plain objects only, no deltas)
// with its trailer checksum.
func writePack(objects []*gitobj.GitObject) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	buf.Write(header)
	for _, object := range objects {
		buf.Write(writePackEntryHeader(packTypeCode(object.Type), len(object.Data)))
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(object.Data); err != nil {
			panic(err)
		}
		if err := w.Close(); err != nil {
			panic(err)
		}
	}
	body := buf.Bytes()
	trailer := util.SHA1Digest(body)
	return append(body, trailer[:]...)
}

// -------------------------------------------------------------------------------------------
// pack reading (whole objects, OFS_DELTA, REF_DELTA)
// -------------------------------------------------------------------------------------------

// readPackEntryHeader reads one pack entry's varint size header.
func readPackEntryHeader(buf []byte, offset int) (typeCode, size, next int, err error) {
	if offset >= len(buf) {
		return 0, 0, 0, fmt.Errorf("pack entry header at offset %d is truncated", offset)
	}
	first := buf[offset]
	typeCode = int((first >> 4) & 0x07)
	size = int(first & 0x0f)
	shift := uint(4)
	at := offset + 1
	for first&0x80 != 0 {
		if at >= len(buf) {
			return 0, 0, 0, fmt.Errorf("pack entry header at offset %d is truncated", offset)
		}
		b := buf[at]
		size |= int(b&0x7f) << shift
		shift += 7
		at++
		first = b
	}
	return typeCode, size, at, nil
}

// readOFSDeltaBase reads an OFS_DELTA base offset encoding.
func readOFSDeltaBase(buf []byte, offset int) (baseOffset, next int, err error) {
	if offset >= len(buf) {
		return 0, 0, fmt.Errorf("ofs-delta base at offset %d is truncated", offset)
	}
	b := buf[offset]
	baseOffset = int(b & 0x7f)
	at := offset + 1
	for b&0x80 != 0 {
		if at >= len(buf) {
			return 0, 0, fmt.Errorf("ofs-delta base at offset %d is truncated", offset)
		}
		b = buf[at]
		baseOffset = ((baseOffset + 1) << 7) | int(b&0x7f)
		at++
	}
	return baseOffset, at, nil
}

// inflateAt inflates the zlib stream starting at offset in buf (bounded
// by end) and returns the inflated bytes and the offset just past the
// stream. The stream end is found by binary search: inflating
// buf[offset:e] fails while e is short of the true end and succeeds once
// e reaches it (a zlib reader ignores trailing bytes), so the predicate
// is monotone and the first succeeding e is the true end.
func inflateAt(buf []byte, offset, end int, wantSize int) (data []byte, next int, err error) {
	try := func(e int) ([]byte, bool) {
		r, err := zlib.NewReader(bytes.NewReader(buf[offset:e]))
		if err != nil {
			return nil, false
		}
		out, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || len(out) != wantSize {
			return nil, false
		}
		return out, true
	}
	if out, ok := try(end); ok {
		// The whole remaining slice inflates: the stream may still end
		// before `end`, so binary-search the true end below.
		_ = out
	} else {
		return nil, 0, fmt.Errorf("zlib stream at offset %d does not inflate to %d bytes", offset, wantSize)
	}
	lo, hi := offset, end
	for lo < hi {
		mid := (lo + hi) / 2
		if _, ok := try(mid); ok {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	out, ok := try(lo)
	if !ok {
		return nil, 0, fmt.Errorf("zlib stream at offset %d does not inflate to %d bytes", offset, wantSize)
	}
	return out, lo, nil
}

// applyDelta applies a git delta (copy/insert instructions) to a base.
func applyDelta(base, delta []byte) ([]byte, error) {
	at := 0
	readVarint := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if at >= len(delta) {
				return 0, fmt.Errorf("delta varint is truncated")
			}
			b := delta[at]
			at++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	baseSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	resultSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if baseSize != len(base) {
		return nil, fmt.Errorf("delta base size %d does not match %d", baseSize, len(base))
	}
	var parts [][]byte
	total := 0
	for at < len(delta) {
		op := delta[at]
		at++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if at >= len(delta) {
						return nil, fmt.Errorf("delta copy is truncated")
					}
					offset |= int(delta[at]) << (8 * uint(i))
					at++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if at >= len(delta) {
						return nil, fmt.Errorf("delta copy is truncated")
					}
					size |= int(delta[at]) << (8 * uint(i))
					at++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, fmt.Errorf("delta copy runs past the base")
			}
			parts = append(parts, base[offset:offset+size])
			total += size
		} else if op != 0 {
			if at+int(op) > len(delta) {
				return nil, fmt.Errorf("delta insert runs past the delta")
			}
			parts = append(parts, delta[at:at+int(op)])
			at += int(op)
			total += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0 is reserved")
		}
	}
	if total != resultSize {
		return nil, fmt.Errorf("delta produced %d bytes, expected %d", total, resultSize)
	}
	return bytes.Join(parts, nil), nil
}

type rawPackEntry struct {
	offset     int
	typeCode   int
	size       int
	data       []byte
	baseOffset int
	baseOID    string
	resolved   *gitobj.GitObject
	oid        string
}

func packObjectType(typeCode int) (gitobj.ObjectType, bool) {
	switch typeCode {
	case packCommit:
		return gitobj.Commit, true
	case packTree:
		return gitobj.Tree, true
	case packBlob:
		return gitobj.Blob, true
	case packTag:
		return gitobj.Tag, true
	}
	return "", false
}

// readPack parses a whole packfile. resolveBase supplies objects the
// pack's thin REF_DELTA entries name but the pack does not carry (the
// client's haves). It returns the pack's objects keyed by oid.
func readPack(pack []byte, resolveBase gitobj.Lookup) (map[string]*gitobj.GitObject, error) {
	if len(pack) < 32 {
		return nil, fmt.Errorf("pack is %d bytes, too short", len(pack))
	}
	if string(pack[0:4]) != "PACK" {
		return nil, fmt.Errorf("pack does not start with PACK")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	declaredCount := binary.BigEndian.Uint32(pack[8:12])
	trailerStart := len(pack) - 20
	trailer := hex.EncodeToString(pack[trailerStart:])
	if trailer != util.SHA1Hex(pack[:trailerStart]) {
		return nil, fmt.Errorf("pack trailer checksum mismatch")
	}
	var raw []*rawPackEntry
	byOffset := map[int]*rawPackEntry{}
	offset := 12
	for index := uint32(0); index < declaredCount; index++ {
		entryOffset := offset
		typeCode, size, next, err := readPackEntryHeader(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		entry := &rawPackEntry{offset: entryOffset, typeCode: typeCode, size: size, baseOffset: -1}
		switch typeCode {
		case packOFSDelta:
			base, nextBase, err := readOFSDeltaBase(pack, offset)
			if err != nil {
				return nil, err
			}
			offset = nextBase
			if base <= 0 || base > entryOffset {
				return nil, fmt.Errorf("ofs-delta at %d names an invalid base offset", entryOffset)
			}
			entry.baseOffset = entryOffset - base
		case packREFDelta:
			if offset+20 > trailerStart {
				return nil, fmt.Errorf("ref-delta base id is truncated")
			}
			entry.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		default:
			if _, ok := packObjectType(typeCode); !ok {
				return nil, fmt.Errorf("pack entry %d has unknown type code %d", index, typeCode)
			}
		}
		inflated, nextOffset, err := inflateAt(pack, offset, trailerStart, size)
		if err != nil {
			return nil, err
		}
		entry.data = inflated
		offset = nextOffset
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}

	objects := map[string]*gitobj.GitObject{}
	resolvedByOID := map[string]*rawPackEntry{}
	resolving := map[int]bool{}
	var resolve func(entry *rawPackEntry) (*gitobj.GitObject, error)
	resolve = func(entry *rawPackEntry) (*gitobj.GitObject, error) {
		if entry.resolved != nil {
			return entry.resolved, nil
		}
		if resolving[entry.offset] {
			return nil, fmt.Errorf("delta cycle at pack offset %d", entry.offset)
		}
		resolving[entry.offset] = true
		var typ gitobj.ObjectType
		var data []byte
		switch entry.typeCode {
		case packOFSDelta:
			baseEntry, ok := byOffset[entry.baseOffset]
			if !ok {
				resolving[entry.offset] = false
				return nil, fmt.Errorf("ofs-delta base at offset %d is not in the pack", entry.baseOffset)
			}
			base, err := resolve(baseEntry)
			if err != nil {
				resolving[entry.offset] = false
				return nil, err
			}
			typ = base.Type
			data, err = applyDelta(base.Data, entry.data)
			if err != nil {
				resolving[entry.offset] = false
				return nil, err
			}
		case packREFDelta:
			if baseEntry, ok := resolvedByOID[entry.baseOID]; ok {
				base, err := resolve(baseEntry)
				if err != nil {
					resolving[entry.offset] = false
					return nil, err
				}
				typ = base.Type
				data, err = applyDelta(base.Data, entry.data)
				if err != nil {
					resolving[entry.offset] = false
					return nil, err
				}
			} else {
				external := resolveBase(entry.baseOID)
				if external == nil {
					resolving[entry.offset] = false
					return nil, fmt.Errorf("ref-delta base %s is neither in the pack nor supplied", entry.baseOID)
				}
				typ = external.Type
				var err error
				data, err = applyDelta(external.Data, entry.data)
				if err != nil {
					resolving[entry.offset] = false
					return nil, err
				}
			}
		default:
			t, _ := packObjectType(entry.typeCode)
			typ = t
			data = entry.data
		}
		oid := gitobj.HashObject(typ, data)
		resolved := &gitobj.GitObject{Type: typ, Data: data}
		entry.resolved = resolved
		entry.oid = oid
		resolvedByOID[oid] = entry
		delete(resolving, entry.offset)
		return resolved, nil
	}
	for _, entry := range raw {
		resolved, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		if _, ok := objects[entry.oid]; !ok {
			objects[entry.oid] = resolved
		}
	}
	return objects, nil
}
