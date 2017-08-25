package capnp

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// A SegmentID is a numeric identifier for a Segment.
type SegmentID uint32

// A Segment is an allocation arena for Cap'n Proto objects.
// It is part of a Message, which can contain other segments that
// reference each other.
type Segment struct {
	msg  *Message
	id   SegmentID
	data []byte
}

// Message returns the message that contains s.
func (s *Segment) Message() *Message {
	return s.msg
}

// ID returns the segment's ID.
func (s *Segment) ID() SegmentID {
	return s.id
}

// Data returns the raw byte slice for the segment.
func (s *Segment) Data() []byte {
	return s.data
}

func (s *Segment) inBounds(addr Address) bool {
	return addr < Address(len(s.data))
}

func (s *Segment) regionInBounds(base Address, sz Size) bool {
	end, ok := base.addSize(sz)
	if !ok {
		return false
	}
	return end <= Address(len(s.data))
}

// slice returns the segment of data from base to base+sz.
func (s *Segment) slice(base Address, sz Size) []byte {
	// Bounds check should have happened before calling slice.
	return s.data[base : base+Address(sz)]
}

func (s *Segment) readUint8(addr Address) uint8 {
	return s.slice(addr, 1)[0]
}

func (s *Segment) readUint16(addr Address) uint16 {
	return binary.LittleEndian.Uint16(s.slice(addr, 2))
}

func (s *Segment) readUint32(addr Address) uint32 {
	return binary.LittleEndian.Uint32(s.slice(addr, 4))
}

func (s *Segment) readUint64(addr Address) uint64 {
	return binary.LittleEndian.Uint64(s.slice(addr, 8))
}

func (s *Segment) readRawPointer(addr Address) rawPointer {
	return rawPointer(s.readUint64(addr))
}

func (s *Segment) writeUint8(addr Address, val uint8) {
	s.slice(addr, 1)[0] = val
}

func (s *Segment) writeUint16(addr Address, val uint16) {
	binary.LittleEndian.PutUint16(s.slice(addr, 2), val)
}

func (s *Segment) writeUint32(addr Address, val uint32) {
	binary.LittleEndian.PutUint32(s.slice(addr, 4), val)
}

func (s *Segment) writeUint64(addr Address, val uint64) {
	binary.LittleEndian.PutUint64(s.slice(addr, 8), val)
}

func (s *Segment) writeRawPointer(addr Address, val rawPointer) {
	s.writeUint64(addr, uint64(val))
}

// root returns a 1-element pointer list that references the first word
// in the segment.  This only makes sense to call on the first segment
// in a message.
func (s *Segment) root() PointerList {
	sz := ObjectSize{PointerCount: 1}
	if !s.regionInBounds(0, sz.totalSize()) {
		return PointerList{}
	}
	return PointerList{List{
		seg:        s,
		length:     1,
		size:       sz,
		depthLimit: s.msg.depthLimit(),
	}}
}

func (s *Segment) lookupSegment(id SegmentID) (*Segment, error) {
	if s.id == id {
		return s, nil
	}
	return s.msg.Segment(id)
}

func (s *Segment) readPtr(off Address, depthLimit uint) (ptr Ptr, err error) {
	val := s.readRawPointer(off)
	s, off, val, err = s.resolveFarPointer(off, val)
	if err != nil {
		return Ptr{}, err
	}
	if val == 0 {
		return Ptr{}, nil
	}
	if depthLimit == 0 {
		return Ptr{}, errDepthLimit
	}
	// Be wary of overflow. Offset is 30 bits signed. List size is 29 bits
	// unsigned. For both of these we need to check in terms of words if
	// using 32 bit maths as bits or bytes will overflow.
	switch val.pointerType() {
	case structPointer:
		sp, err := s.readStructPtr(off, val)
		if err != nil {
			return Ptr{}, err
		}
		if !s.msg.canRead(sp.readSize()) {
			return Ptr{}, errReadLimit
		}
		sp.depthLimit = depthLimit - 1
		return sp.ToPtr(), nil
	case listPointer:
		lp, err := s.readListPtr(off, val)
		if err != nil {
			return Ptr{}, err
		}
		if !s.msg.canRead(lp.readSize()) {
			return Ptr{}, errReadLimit
		}
		lp.depthLimit = depthLimit - 1
		return lp.ToPtr(), nil
	case otherPointer:
		if val.otherPointerType() != 0 {
			return Ptr{}, errOtherPointer
		}
		return Interface{
			seg: s,
			cap: val.capabilityIndex(),
		}.ToPtr(), nil
	default:
		// Only other types are far pointers.
		return Ptr{}, errBadLandingPad
	}
}

func (s *Segment) readStructPtr(off Address, val rawPointer) (Struct, error) {
	addr, ok := val.offset().resolve(off)
	if !ok {
		return Struct{}, errPointerAddress
	}
	sz := val.structSize()
	if !s.regionInBounds(addr, sz.totalSize()) {
		return Struct{}, errPointerAddress
	}
	return Struct{
		seg:  s,
		off:  addr,
		size: sz,
	}, nil
}

func (s *Segment) readListPtr(off Address, val rawPointer) (List, error) {
	addr, ok := val.offset().resolve(off)
	if !ok {
		return List{}, errPointerAddress
	}
	lsize, ok := val.totalListSize()
	if !ok {
		return List{}, errOverflow
	}
	if !s.regionInBounds(addr, lsize) {
		return List{}, errPointerAddress
	}
	lt := val.listType()
	if lt == compositeList {
		hdr := s.readRawPointer(addr)
		var ok bool
		addr, ok = addr.addSize(wordSize)
		if !ok {
			return List{}, errOverflow
		}
		if hdr.pointerType() != structPointer {
			return List{}, errBadTag
		}
		sz := hdr.structSize()
		n := int32(hdr.offset())
		// TODO(light): check that this has the same end address
		if tsize, ok := sz.totalSize().times(n); !ok {
			return List{}, errOverflow
		} else if !s.regionInBounds(addr, tsize) {
			return List{}, errPointerAddress
		}
		return List{
			seg:    s,
			size:   sz,
			off:    addr,
			length: n,
			flags:  isCompositeList,
		}, nil
	}
	if lt == bit1List {
		return List{
			seg:    s,
			off:    addr,
			length: val.numListElements(),
			flags:  isBitList,
		}, nil
	}
	return List{
		seg:    s,
		size:   val.elementSize(),
		off:    addr,
		length: val.numListElements(),
	}, nil
}

func (s *Segment) resolveFarPointer(off Address, val rawPointer) (*Segment, Address, rawPointer, error) {
	switch val.pointerType() {
	case doubleFarPointer:
		// A double far pointer points to a double pointer, where the
		// first points to the actual data, and the second is the tag
		// that would normally be placed right before the data (offset
		// == 0).

		faroff, segid := val.farAddress(), val.farSegment()
		s, err := s.lookupSegment(segid)
		if err != nil {
			return nil, 0, 0, err
		}
		if !s.regionInBounds(faroff, wordSize*2) {
			return nil, 0, 0, errPointerAddress
		}
		far := s.readRawPointer(faroff)
		tagStart, ok := faroff.addSize(wordSize)
		if !ok {
			return nil, 0, 0, errOverflow
		}
		tag := s.readRawPointer(tagStart)
		if far.pointerType() != farPointer || tag.offset() != 0 {
			return nil, 0, 0, errPointerAddress
		}
		segid = far.farSegment()
		if s, err = s.lookupSegment(segid); err != nil {
			return nil, 0, 0, errBadLandingPad
		}
		return s, 0, landingPadNearPointer(far, tag), nil
	case farPointer:
		faroff, segid := val.farAddress(), val.farSegment()
		s, err := s.lookupSegment(segid)
		if err != nil {
			return nil, 0, 0, err
		}
		if !s.regionInBounds(faroff, wordSize) {
			return nil, 0, 0, errPointerAddress
		}
		val = s.readRawPointer(faroff)
		return s, faroff, val, nil
	default:
		return s, off, val, nil
	}
}

func (s *Segment) writePtr(off Address, src Ptr, forceCopy bool) error {
	if !src.IsValid() {
		s.writeRawPointer(off, 0)
		return nil
	}
	// Copy src, if needed.  This is type-dependent.
	switch src.flags.ptrType() {
	case structPtrType:
		st := src.Struct()
		if forceCopy || src.seg.msg != s.msg || st.flags&isListMember != 0 {
			newSeg, newAddr, err := alloc(s, st.size.totalSize())
			if err != nil {
				return err
			}
			dst := Struct{
				seg:        newSeg,
				off:        newAddr,
				size:       st.size,
				depthLimit: maxDepth,
				// clear flags
			}
			if err := copyStruct(dst, st); err != nil {
				return err
			}
			src = dst.ToPtr()
		}
	case listPtrType:
		if forceCopy || src.seg.msg != s.msg {
			l := src.List()
			sz := l.allocSize()
			newSeg, newAddr, err := alloc(s, sz)
			if err != nil {
				return err
			}
			dst := List{
				seg:        newSeg,
				off:        newAddr,
				length:     l.length,
				size:       l.size,
				flags:      l.flags,
				depthLimit: maxDepth,
			}
			if dst.flags&isCompositeList != 0 {
				// Copy tag word
				newSeg.writeRawPointer(newAddr, l.seg.readRawPointer(l.off-Address(wordSize)))
				var ok bool
				dst.off, ok = dst.off.addSize(wordSize)
				if !ok {
					return errOverflow
				}
				sz -= wordSize
			}
			if dst.flags&isBitList != 0 || dst.size.PointerCount == 0 {
				end, _ := l.off.addSize(sz) // list has already validated
				copy(newSeg.data[dst.off:], l.seg.data[l.off:end])
			} else {
				for i := 0; i < l.Len(); i++ {
					err := copyStruct(dst.Struct(i), l.Struct(i))
					if err != nil {
						return err
					}
				}
			}
			src = dst.ToPtr()
		}
	case interfacePtrType:
		i := src.Interface()
		if src.seg.msg != s.msg {
			c := s.msg.AddCap(i.Client())
			i = NewInterface(s, c)
		}
		s.writeRawPointer(off, i.value(off))
		return nil
	default:
		panic("unreachable")
	}

	// Create far pointer if object is in a different segment.
	if src.seg != s {
		if !hasCapacity(src.seg.data, wordSize) {
			// Double far pointer needed.
			const landingSize = wordSize * 2
			t, dstAddr, err := alloc(s, landingSize)
			if err != nil {
				return err
			}

			srcAddr := src.address()
			t.writeRawPointer(dstAddr, rawFarPointer(src.seg.id, srcAddr))
			// alloc guarantees that two words are available.
			t.writeRawPointer(dstAddr+Address(wordSize), src.value(srcAddr-Address(wordSize)))
			s.writeRawPointer(off, rawDoubleFarPointer(t.id, dstAddr))
			return nil
		}
		// Have room in the target for a tag
		_, srcAddr, _ := alloc(src.seg, wordSize)
		src.seg.writeRawPointer(srcAddr, src.value(srcAddr))
		s.writeRawPointer(off, rawFarPointer(src.seg.id, srcAddr))
		return nil
	}

	// Local pointer.
	s.writeRawPointer(off, src.value(off))
	return nil
}

// Equal returns true iff p1 and p2 are equal.
//
// Equality is defined to be:
//
//	- Two structs are equal iff all of their fields are equal.  If one
//	  struct has more fields than the other, the extra fields must all be
//		zero.
//	- Two lists are equal iff they have the same length and their
//	  corresponding elements are equal.  If one list is a list of
//	  primitives and the other is a list of structs, then the list of
//	  primitives is treated as if it was a list of structs with the
//	  element value as the sole field.
//	- Two interfaces are equal iff they point to a capability created by
//	  the same call to NewClient or they are referring to the same
//	  capability table index in the same message.  The latter is
//	  significant when the message's capability table has not been
//	  populated.
//	- Two null pointers are equal.
//	- All other combinations of things are not equal.
func Equal(p1, p2 Ptr) (bool, error) {
	if !p1.IsValid() && !p2.IsValid() {
		return true, nil
	}
	if !p1.IsValid() || !p2.IsValid() {
		return false, nil
	}
	pt := p1.flags.ptrType()
	if pt != p2.flags.ptrType() {
		return false, nil
	}
	switch pt {
	case structPtrType:
		s1, s2 := p1.Struct(), p2.Struct()
		data1 := s1.seg.slice(s1.off, s1.size.DataSize)
		data2 := s2.seg.slice(s2.off, s2.size.DataSize)
		switch {
		case len(data1) < len(data2):
			if !bytes.Equal(data1, data2[:len(data1)]) {
				return false, nil
			}
			if !isZeroFilled(data2[len(data1):]) {
				return false, nil
			}
		case len(data1) > len(data2):
			if !bytes.Equal(data1[:len(data2)], data2) {
				return false, nil
			}
			if !isZeroFilled(data1[len(data2):]) {
				return false, nil
			}
		default:
			if !bytes.Equal(data1, data2) {
				return false, nil
			}
		}
		n := int(s1.size.PointerCount)
		if n2 := int(s2.size.PointerCount); n2 < n {
			n = n2
		}
		for i := 0; i < n; i++ {
			sp1, err := s1.Ptr(uint16(i))
			if err != nil {
				return false, err
			}
			sp2, err := s2.Ptr(uint16(i))
			if err != nil {
				return false, err
			}
			if ok, err := Equal(sp1, sp2); !ok || err != nil {
				return false, err
			}
		}
		for i := n; i < int(s1.size.PointerCount); i++ {
			if s1.HasPtr(uint16(i)) {
				return false, nil
			}
		}
		for i := n; i < int(s2.size.PointerCount); i++ {
			if s2.HasPtr(uint16(i)) {
				return false, nil
			}
		}
		return true, nil
	case listPtrType:
		l1, l2 := p1.List(), p2.List()
		if l1.Len() != l2.Len() {
			return false, nil
		}
		if l1.flags&compositeList == 0 && l2.flags&compositeList == 0 && l1.size != l2.size {
			return false, nil
		}
		if l1.size.PointerCount == 0 && l2.size.PointerCount == 0 && l1.size.DataSize == l2.size.DataSize {
			// Optimization: pure data lists can be compared bytewise.
			sz, _ := l1.size.totalSize().times(l1.length) // both list bounds have been validated
			return bytes.Equal(l1.seg.slice(l1.off, sz), l2.seg.slice(l2.off, sz)), nil
		}
		for i := 0; i < l1.Len(); i++ {
			e1, e2 := l1.Struct(i), l2.Struct(i)
			if ok, err := Equal(e1.ToPtr(), e2.ToPtr()); !ok || err != nil {
				return false, err
			}
		}
		return true, nil
	case interfacePtrType:
		i1, i2 := p1.Interface(), p2.Interface()
		if i1.Message() == i2.Message() {
			if i1.Capability() == i2.Capability() {
				return true, nil
			}
			ntab := len(i1.Message().CapTable)
			if int64(i1.Capability()) >= int64(ntab) || int64(i2.Capability()) >= int64(ntab) {
				return false, nil
			}
		}
		return i1.Client().IsSame(i2.Client()), nil
	default:
		panic("unreachable")
	}
}

func isZeroFilled(b []byte) bool {
	for _, bb := range b {
		if bb != 0 {
			return false
		}
	}
	return true
}

var (
	errPointerAddress = errors.New("capnp: invalid pointer address")
	errBadLandingPad  = errors.New("capnp: invalid far pointer landing pad")
	errBadTag         = errors.New("capnp: invalid tag word")
	errOtherPointer   = errors.New("capnp: unknown pointer type")
	errObjectSize     = errors.New("capnp: invalid object size")
	errElementSize    = errors.New("capnp: mismatched list element size")
	errReadLimit      = errors.New("capnp: read traversal limit reached")
	errDepthLimit     = errors.New("capnp: depth limit reached")
)

var (
	errOverflow    = errors.New("capnp: address or size overflow")
	errOutOfBounds = errors.New("capnp: address out of bounds")
	errCopyDepth   = errors.New("capnp: copy depth too large")
	errOverlap     = errors.New("capnp: overlapping data on copy")
	errListSize    = errors.New("capnp: invalid list size")
)
