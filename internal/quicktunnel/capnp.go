package quicktunnel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Cap'n Proto 消息使用单段编码，段大小为 8 字节 word。这里实现协议所需的
// 最小子集：结构体指针、列表指针、文本/Data、复合列表与 far 指针读取。
//
// 参考 Cap'n Proto 编码规范：
// https://capnproto.org/encoding.html
const (
	maxCapnpSegments     = 16
	maxCapnpMessageBytes = 8 << 20
	capnpWordSize        = 8
)

// segment 是消息构建器；words[0] 固定留给根指针。
type segment struct {
	words []uint64
}

func newSegment() *segment {
	return &segment{words: make([]uint64, 1)}
}

// alloc 追加 n 个清零的 word，返回起始下标。
func (s *segment) alloc(n int) int {
	if n < 0 {
		panic("quicktunnel: negative capnp allocation")
	}
	start := len(s.words)
	s.words = append(s.words, make([]uint64, n)...)
	return start
}

// structPointer 构造结构体指针。offset 是目标数据相对“指针后一个 word”的
// 有符号偏移；dataWords/ptrWords 是数据段与指针段的大小。
func structPointer(offset, dataWords, ptrWords int) uint64 {
	return uint64(uint32(int32(offset))&0x3fffffff)<<2 |
		uint64(uint32(dataWords)&0xffff)<<32 |
		uint64(uint32(ptrWords)&0xffff)<<48
}

// listPointer 构造列表指针。elemSize 见 Cap'n Proto 编码规范：
// 0=void, 1=bit, 2=byte, 3=2 字节, 4=4 字节, 5=8 字节, 6=指针, 7=复合。
func listPointer(offset, elemSize, count int) uint64 {
	return uint64(uint32(int32(offset))&0x3fffffff)<<2 | 1 |
		uint64(uint32(elemSize)&7)<<32 |
		uint64(uint32(count)&0x1fffffff)<<35
}

func (s *segment) setStructPtr(ptrWord, target, dataWords, ptrWords int) {
	s.words[ptrWord] = structPointer(target-(ptrWord+1), dataWords, ptrWords)
}

func (s *segment) setListPtr(ptrWord, target, elemSize, count int) {
	s.words[ptrWord] = listPointer(target-(ptrWord+1), elemSize, count)
}

// setText 写入 Text（UTF-8 + NUL）。
func (s *segment) setText(ptrWord int, value string) {
	data := []byte(value)
	size := len(data) + 1
	start := s.alloc((size + capnpWordSize - 1) / capnpWordSize)
	s.setListPtr(ptrWord, start, 2, size)
	s.writeBytes(start, data)
}

// setData 写入 Data（原始字节，无 NUL）。
func (s *segment) setData(ptrWord int, value []byte) {
	size := len(value)
	start := s.alloc((size + capnpWordSize - 1) / capnpWordSize)
	s.setListPtr(ptrWord, start, 2, size)
	s.writeBytes(start, value)
}

// setTextList 写入 List(Text)。
func (s *segment) setTextList(ptrWord int, values []string) {
	start := s.alloc(len(values))
	s.setListPtr(ptrWord, start, 6, len(values))
	for i, value := range values {
		s.setText(start+i, value)
	}
}

// setCompositeList 写入 List(Struct)。返回第一个元素的数据起始下标。
func (s *segment) setCompositeList(ptrWord, count, dataWords, ptrWords int) int {
	elementWords := dataWords + ptrWords
	total := 1 + count*elementWords
	start := s.alloc(total)
	// 复合列表的 tag word 是一个结构体指针：offset 存元素数量。
	s.words[start] = structPointer(count, dataWords, ptrWords)
	// 列表指针的 count 字段是元素占用的 word 数（不含 tag）。
	s.setListPtr(ptrWord, start, 7, count*elementWords)
	return start + 1
}

func (s *segment) writeBytes(start int, data []byte) {
	for i, b := range data {
		word := start + i/capnpWordSize
		shift := uint(i%capnpWordSize) * 8
		s.words[word] |= uint64(b) << shift
	}
}

// setBits 在数据段指定字节偏移写入 bits 位小端整数。
func (s *segment) setBits(dataStart, byteOffset, bits int, value uint64) {
	word := dataStart + byteOffset/capnpWordSize
	shift := uint(byteOffset%capnpWordSize) * 8
	mask := uint64(math.MaxUint64)
	if bits < 64 {
		mask = (uint64(1) << uint(bits)) - 1
	}
	mask <<= shift
	s.words[word] = (s.words[word] &^ mask) | ((value << shift) & mask)
}

func (s *segment) setU8(dataStart, byteOffset int, value uint8) {
	s.setBits(dataStart, byteOffset, 8, uint64(value))
}

func (s *segment) setU16(dataStart, byteOffset int, value uint16) {
	s.setBits(dataStart, byteOffset, 16, uint64(value))
}

func (s *segment) setU32(dataStart, byteOffset int, value uint32) {
	s.setBits(dataStart, byteOffset, 32, uint64(value))
}

func (s *segment) setU64(dataStart, byteOffset int, value uint64) {
	s.setBits(dataStart, byteOffset, 64, value)
}

// setBit 写入数据段中的单个 bit（bitOffset 从数据段起点按 bit 计数）。
func (s *segment) setBit(dataStart, bitOffset int, value bool) {
	word := dataStart + bitOffset/64
	shift := uint(bitOffset % 64)
	mask := uint64(1) << shift
	if value {
		s.words[word] |= mask
	} else {
		s.words[word] &^= mask
	}
}

// encode 输出标准 Cap'n Proto 消息帧（单段）。
func (s *segment) encode() []byte {
	buf := make([]byte, capnpWordSize+len(s.words)*capnpWordSize)
	// 段数量 - 1 = 0。
	binary.LittleEndian.PutUint32(buf[0:], 0)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(s.words)))
	for i, word := range s.words {
		binary.LittleEndian.PutUint64(buf[capnpWordSize+i*capnpWordSize:], word)
	}
	return buf
}

// ---- 读取端 ----

type message struct {
	segments [][]uint64
}

func readMessage(r io.Reader) (*message, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, fmt.Errorf("capnp: read segment count: %w", err)
	}
	count := int(binary.LittleEndian.Uint32(head[:])) + 1
	if count < 1 || count > maxCapnpSegments {
		return nil, fmt.Errorf("capnp: invalid segment count %d", count)
	}

	sizeBytes := make([]byte, 4*count)
	if _, err := io.ReadFull(r, sizeBytes); err != nil {
		return nil, fmt.Errorf("capnp: read segment sizes: %w", err)
	}
	sizes := make([]uint32, count)
	// 用 uint64 累计：32 位平台上 int(size)*8 可能溢出成负数，绕过上限
	// 检查后让 make 触发 panic，而不是返回可处理的错误。
	var totalBytes uint64
	for i := range sizes {
		sizes[i] = binary.LittleEndian.Uint32(sizeBytes[i*4:])
		totalBytes += uint64(sizes[i]) * capnpWordSize
		if totalBytes > maxCapnpMessageBytes {
			return nil, fmt.Errorf("capnp: message exceeds %d bytes", maxCapnpMessageBytes)
		}
	}
	// 段表总长度补齐到 8 字节。
	if tableBytes := 4 + 4*count; tableBytes%capnpWordSize != 0 {
		if _, err := io.ReadFull(r, make([]byte, capnpWordSize-tableBytes%capnpWordSize)); err != nil {
			return nil, fmt.Errorf("capnp: read table padding: %w", err)
		}
	}

	segments := make([][]uint64, count)
	for i, size := range sizes {
		// 总长度已在上面按 uint64 校验过，逐段转换到 int 是安全的。
		n := int(uint64(size) * capnpWordSize)
		raw := make([]byte, n)
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, fmt.Errorf("capnp: read segment %d: %w", i, err)
		}
		words := make([]uint64, size)
		for j := range words {
			words[j] = binary.LittleEndian.Uint64(raw[j*capnpWordSize:])
		}
		segments[i] = words
	}
	return &message{segments: segments}, nil
}

type structRef struct {
	seg       int
	data      int
	dataWords int
	ptrWords  int
}

type listRef struct {
	seg       int
	start     int
	elemSize  int
	elemCount int
	dataWords int
	ptrWords  int
}

// pointer 解析指针并跟随 far 指针，返回 kind（0=struct, 1=list, 3=cap）。
func (m *message) pointer(seg, ptrWord int) (kind int, outSeg, outPtr int, err error) {
	for depth := 0; depth < 4; depth++ {
		if seg < 0 || seg >= len(m.segments) || ptrWord < 0 || ptrWord >= len(m.segments[seg]) {
			return 0, 0, 0, errors.New("capnp: pointer out of bounds")
		}
		word := m.segments[seg][ptrWord]
		switch word & 3 {
		case 0, 1:
			return int(word & 3), seg, ptrWord, nil
		case 2:
			// far 指针：bit 2 是 landing pad 是否占两个 word，bits 3..31
			// 才是 landing pad 的 word 偏移。从 bit 3 开始取，否则 pad 标志
			// 会被算进偏移。landing pad 的第一个 word 始终是真正的指针；
			// 双 word pad 的第二个 word 是列表 tag，本实现只处理结构体/
			// 文本列表，跟随第一个 word 即可。
			seg = int(word >> 32)
			ptrWord = int((word >> 3) & 0x1fffffff)
		default:
			return 3, seg, ptrWord, nil
		}
	}
	return 0, 0, 0, errors.New("capnp: far pointer loop")
}

func (m *message) structAt(seg, ptrWord int) (structRef, error) {
	kind, seg, ptrWord, err := m.pointer(seg, ptrWord)
	if err != nil {
		return structRef{}, err
	}
	if kind != 0 {
		return structRef{}, fmt.Errorf("capnp: expected struct pointer, got kind %d", kind)
	}
	word := m.segments[seg][ptrWord]
	offset := int(int32(uint32(word)) >> 2)
	data := ptrWord + 1 + offset
	dataWords := int((word >> 32) & 0xffff)
	ptrWords := int(word >> 48)
	if data < 0 || data+dataWords+ptrWords > len(m.segments[seg]) {
		return structRef{}, errors.New("capnp: struct out of bounds")
	}
	return structRef{seg: seg, data: data, dataWords: dataWords, ptrWords: ptrWords}, nil
}

func (m *message) listAt(seg, ptrWord int) (listRef, error) {
	kind, seg, ptrWord, err := m.pointer(seg, ptrWord)
	if err != nil {
		return listRef{}, err
	}
	if kind != 1 {
		return listRef{}, fmt.Errorf("capnp: expected list pointer, got kind %d", kind)
	}
	word := m.segments[seg][ptrWord]
	offset := int(int32(uint32(word)) >> 2)
	start := ptrWord + 1 + offset
	elemSize := int((word >> 32) & 7)
	count := int(word >> 35)
	words := len(m.segments[seg])

	if elemSize == 7 {
		// 复合列表：count 是元素占用的 word 数（不含 tag）。
		if start < 0 || start >= words || count < 0 || start+1+count > words {
			return listRef{}, errors.New("capnp: composite list out of bounds")
		}
		tag := m.segments[seg][start]
		elemCount := int(int32(uint32(tag)) >> 2)
		dataWords := int((tag >> 32) & 0xffff)
		ptrWords := int(tag >> 48)
		if dataWords < 0 || ptrWords < 0 || elemCount < 0 ||
			uint64(elemCount)*uint64(dataWords+ptrWords) > uint64(count) {
			return listRef{}, errors.New("capnp: invalid composite list tag")
		}
		return listRef{
			seg:       seg,
			start:     start + 1,
			elemSize:  elemSize,
			elemCount: elemCount,
			dataWords: dataWords,
			ptrWords:  ptrWords,
		}, nil
	}

	var needed int
	switch elemSize {
	case 0:
		needed = 0
	case 1:
		needed = (count + 63) / 64
	case 2:
		needed = (count + 7) / 8
	case 3:
		needed = (count*2 + 7) / 8
	case 4:
		needed = (count*4 + 7) / 8
	case 5, 6:
		needed = count
	default:
		return listRef{}, fmt.Errorf("capnp: unknown element size %d", elemSize)
	}
	if start < 0 || count < 0 || start+needed > words {
		return listRef{}, errors.New("capnp: list out of bounds")
	}
	return listRef{seg: seg, start: start, elemSize: elemSize, elemCount: count}, nil
}

// ptrAt 返回结构体第 index 个指针的 (段, 指针 word 下标)。
func (m *message) ptrAt(st structRef, index int) (int, int, error) {
	if index < 0 || index >= st.ptrWords {
		return 0, 0, fmt.Errorf("capnp: struct pointer index %d out of range", index)
	}
	return st.seg, st.data + st.dataWords + index, nil
}

func (m *message) textAt(st structRef, index int) (string, error) {
	seg, ptrWord, err := m.ptrAt(st, index)
	if err != nil {
		return "", err
	}
	return m.text(seg, ptrWord)
}

func (m *message) dataAt(st structRef, index int) ([]byte, error) {
	seg, ptrWord, err := m.ptrAt(st, index)
	if err != nil {
		return nil, err
	}
	return m.data(seg, ptrWord)
}

func (m *message) text(seg, ptrWord int) (string, error) {
	l, err := m.listAt(seg, ptrWord)
	if err != nil {
		return "", err
	}
	if l.elemSize != 2 || l.elemCount == 0 {
		return "", errors.New("capnp: text is not a byte list")
	}
	raw := m.listBytes(l)
	// Text 末尾带 NUL。
	if len(raw) == 0 {
		return "", nil
	}
	return string(raw[:len(raw)-1]), nil
}

func (m *message) data(seg, ptrWord int) ([]byte, error) {
	l, err := m.listAt(seg, ptrWord)
	if err != nil {
		return nil, err
	}
	if l.elemSize != 2 {
		return nil, errors.New("capnp: data is not a byte list")
	}
	return m.listBytes(l), nil
}

func (m *message) listBytes(l listRef) []byte {
	out := make([]byte, l.elemCount)
	words := m.segments[l.seg]
	for i := 0; i < l.elemCount; i++ {
		out[i] = byte(words[l.start+i/8] >> (uint(i%8) * 8))
	}
	return out
}

func (m *message) u8At(st structRef, byteOffset int) uint8 {
	return uint8(m.bitsAt(st, byteOffset, 8))
}

func (m *message) u16At(st structRef, byteOffset int) uint16 {
	return uint16(m.bitsAt(st, byteOffset, 16))
}

func (m *message) u32At(st structRef, byteOffset int) uint32 {
	return uint32(m.bitsAt(st, byteOffset, 32))
}

func (m *message) u64At(st structRef, byteOffset int) uint64 {
	return m.bitsAt(st, byteOffset, 64)
}

func (m *message) boolAt(st structRef, bitOffset int) bool {
	if st.dataWords*64 <= bitOffset {
		return false
	}
	word := m.segments[st.seg][st.data+bitOffset/64]
	return word&(uint64(1)<<uint(bitOffset%64)) != 0
}

// bitsAt 读取数据段里的标量。
//
// Cap'n Proto 保证标量按自身宽度对齐，因此一个字段最多落在一个 word 内；
// 越界读取返回 0 而不是 panic，便于把损坏的消息当成“字段缺失”处理。
func (m *message) bitsAt(st structRef, byteOffset, bits int) uint64 {
	// 统一用 bit 计算边界：byteOffset*8+bits 超过数据段长度即视为缺失。
	if byteOffset < 0 || byteOffset*8+bits > st.dataWords*64 {
		return 0
	}
	word := m.segments[st.seg][st.data+byteOffset/8]
	shift := uint(byteOffset%8) * 8
	value := word >> shift
	if bits < 64 {
		value &= (uint64(1) << uint(bits)) - 1
	}
	return value
}

// compositeElem 返回复合列表第 i 个元素的结构体视图。
func (m *message) compositeElem(l listRef, i int) (structRef, error) {
	if l.elemSize != 7 || i < 0 || i >= l.elemCount {
		return structRef{}, errors.New("capnp: composite element out of range")
	}
	elementWords := l.dataWords + l.ptrWords
	data := l.start + i*elementWords
	if data+l.dataWords+l.ptrWords > len(m.segments[l.seg]) {
		return structRef{}, errors.New("capnp: composite element out of bounds")
	}
	return structRef{seg: l.seg, data: data, dataWords: l.dataWords, ptrWords: l.ptrWords}, nil
}

// rootStruct 返回消息根结构体。
func (m *message) rootStruct() (structRef, error) {
	if len(m.segments) == 0 || len(m.segments[0]) == 0 {
		return structRef{}, errors.New("capnp: empty message")
	}
	return m.structAt(0, 0)
}
