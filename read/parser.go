package read

import (
	"bufio"
	"debug/dwarf"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"runtime"
	"sort"
)

type FieldKind int
type TypeKind int

const (
	FieldKindEol    FieldKind = 0
	FieldKindPtr              = 1
	FieldKindIface            = 2
	FieldKindEface            = 3
	FieldKindString           = 4
	FieldKindSlice            = 5

	FieldKindBool       FieldKind = 6
	FieldKindUInt8                = 7
	FieldKindSInt8                = 8
	FieldKindUInt16               = 9
	FieldKindSInt16               = 10
	FieldKindUInt32     FieldKind = 11
	FieldKindSInt32               = 12
	FieldKindUInt64     FieldKind = 13
	FieldKindSInt64               = 14
	FieldKindFloat32              = 15
	FieldKindFloat64              = 16
	FieldKindComplex64            = 17
	FieldKindComplex128           = 18

	FieldKindBytes8      = 19
	FieldKindBytes16     = 20
	FieldKindBytesElided = 21

	TypeKindObject       TypeKind = 0
	TypeKindArray                 = 1
	TypeKindChan                  = 2
	TypeKindConservative          = 127

	tagEOF         = 0
	tagObject      = 1
	tagOtherRoot   = 2
	tagType        = 3
	tagGoRoutine   = 4
	tagStackFrame  = 5
	tagParams      = 6
	tagFinalizer   = 7
	tagItab        = 8
	tagOSThread    = 9
	tagMemStats    = 10
	tagQFinal      = 11
	tagData        = 12
	tagBss         = 13
	tagDefer       = 14
	tagPanic       = 15
	tagMemProf     = 16
	tagAllocSample = 17

	// DWARF constants
	dw_op_call_frame_cfa = 156
	dw_op_consts         = 17
	dw_op_plus           = 34
	dw_op_plus_uconst    = 35
	dw_op_addr           = 3
	dw_ate_boolean       = 2
	dw_ate_complex_float = 3 // complex64/complex128
	dw_ate_float         = 4 // float32/float64
	dw_ate_signed        = 5 // int8/int16/int32/int64/int
	dw_ate_unsigned      = 7 // uint8/uint16/uint32/uint64/uint/uintptr

	// Size of buckets for FindObj.  Bigger buckets use less memory
	// but make FindObj take longer.  512 byte buckets use about 1.5%
	// of the total heap size and require us to look at at most
	// 64 objects.
	bucketSize = 512
)

type Dump struct {
	Order        binary.ByteOrder
	PtrSize      uint64 // in bytes
	HChanSize    uint64 // channel header size in bytes
	HeapStart    uint64
	HeapEnd      uint64
	TheChar      byte
	Experiment   string
	Ncpu         uint64
	Types        []*Type
	objects      []object
	Frames       []*StackFrame
	Goroutines   []*GoRoutine
	Otherroots   []*OtherRoot
	Finalizers   []*Finalizer  // pending finalizers, object still live
	QFinal       []*QFinalizer // finalizers which are ready to run
	Osthreads    []*OSThread
	Memstats     *runtime.MemStats
	Data         *Data
	Bss          *Data
	Defers       []*Defer
	Panics       []*Panic
	MemProf      []*MemProfEntry
	AllocSamples []*AllocSample

	// handle to dump file
	r io.ReaderAt

	buf []byte // temporary space for Contents calls

	edges []Edge // temporary space for Edges calls

	// list of full types, indexed by ID
	FTList []*FullType

	// map from type address to type
	TypeMap map[uint64]*Type

	// map from itab address to the type address that itab address represents.
	ItabMap map[uint64]uint64

	// Data structure for fast lookup of objects.  Divides the heap
	// into chunks of bucketSize bytes.  For each bucket, we keep
	// track of the lowest address object that has any of its
	// bytes in that bucket.
	bucketSize uint64
	idx        []ObjId
}

type Type struct {
	Name     string // not necessarily unique
	Size     uint64
	efaceptr bool    // Efaces with this type have a data field which is a pointer
	Fields   []Field // ordered in increasing offset order

	Addr uint64
}

type FullType struct {
	Id     int
	Size   uint64
	GCSig  string
	Name   string
	Fields []Field
}

// An edge is a directed connection between two objects.  The source
// object is implicit.  An edge includes information about where it
// leaves the source object and where it lands in the destination obj.
type Edge struct {
	To         ObjId  // index of target object in array
	FromOffset uint64 // offset in source object where ptr was found
	ToOffset   uint64 // offset in destination object where ptr lands

	// name of field in the source object, if known
	FieldName string
}

// object represents an object in the heap.
// There will be a lot of these.  They need to be small.
type object struct {
	Ft     *FullType
	offset int64 // position of object contents in dump file
	Addr   uint64
}

type ObjId int

const (
	ObjNil ObjId = -1
)

// NumObjects returns the number of objects in the heap.  Valid
// ObjIds for other calls are from 0 to NumObjects()-1.
func (d *Dump) NumObjects() int {
	return len(d.objects)
}
func (d *Dump) Contents(i ObjId) []byte {
	x := d.objects[i]
	b := d.buf
	if uint64(cap(b)) < x.Ft.Size {
		b = make([]byte, x.Ft.Size)
		d.buf = b
	}
	b = b[:x.Ft.Size]
	_, err := d.r.ReadAt(b, x.offset)
	if err != nil {
		// TODO: propagate to caller
		log.Fatal(err)
	}
	return b
}
func (d *Dump) Addr(x ObjId) uint64 {
	return d.objects[x].Addr
}
func (d *Dump) Size(x ObjId) uint64 {
	return d.objects[x].Ft.Size
}
func (d *Dump) Ft(x ObjId) *FullType {
	return d.objects[x].Ft
}

// FindObj returns the object id containing the address addr, or -1 if no object contains addr.
func (d *Dump) FindObj(addr uint64) ObjId {
	if addr < d.HeapStart || addr >= d.HeapEnd { // quick exit.  Includes nil.
		return ObjNil
	}
	// linear search among all the objects that map to the same bucketSize-byte bucket.
	for i := d.idx[(addr-d.HeapStart)/bucketSize]; i < ObjId(len(d.objects)); i++ {
		x := &d.objects[i]
		if addr < x.Addr {
			return ObjNil
		}
		if addr < x.Addr+x.Ft.Size {
			return ObjId(i)
		}
	}
	return ObjNil
}

func (d *Dump) Edges(i ObjId) []Edge {
	x := &d.objects[i]
	e := d.edges[:0]
	b := d.Contents(i)
	for _, f := range x.Ft.Fields {
		//fmt.Printf("field %d %s %d\n", f.Kind, f.Name, f.Offset)
		switch f.Kind {
		case FieldKindPtr:
			p := readPtr(d, b[f.Offset:])
			y := d.FindObj(p)
			if y != ObjNil {
				e = append(e, Edge{y, f.Offset, p - d.objects[y].Addr, f.Name})
			}
		case FieldKindEface:
			taddr := readPtr(d, b[f.Offset:])
			if taddr != 0 {
				t := d.TypeMap[taddr]
				if t == nil {
					log.Fatalf("Edges: can't find eface type %x", taddr)
				}
				if t.efaceptr {
					p := readPtr(d, b[f.Offset+d.PtrSize:])
					y := d.FindObj(p)
					if y != ObjNil {
						e = append(e, Edge{y, f.Offset + d.PtrSize, p - d.objects[y].Addr, f.Name})
					}
				}
			}
		case FieldKindIface:
			itabaddr := readPtr(d, b[f.Offset:])
			if itabaddr != 0 {
				taddr := d.ItabMap[itabaddr]
				if taddr == 0 {
					log.Fatalf("Edges: can't find itab %x", itabaddr)
				}
				t := d.TypeMap[taddr]
				if t == nil {
					log.Fatalf("Edges: can't find iface type %x", taddr)
				}
				if t.efaceptr {
					p := readPtr(d, b[f.Offset+d.PtrSize:])
					y := d.FindObj(p)
					if y != ObjNil {
						e = append(e, Edge{y, f.Offset + d.PtrSize, p - d.objects[y].Addr, f.Name})
					}
				}
			}
		default:
			continue
		}
	}
	d.edges = e
	return e
}

type OtherRoot struct {
	Description string
	Edges       []Edge

	toaddr uint64
}

// Object obj has a finalizer.
type Finalizer struct {
	obj  uint64
	fn   uint64 // function to be run (a FuncVal*)
	code uint64 // code ptr (fn->fn)
	fint uint64 // type of function argument
	ot   uint64 // type of object
}

// Finalizer that's ready to run
type QFinalizer struct {
	obj   uint64
	fn    uint64 // function to be run (a FuncVal*)
	code  uint64 // code ptr (fn->fn)
	fint  uint64 // type of function argument
	ot    uint64 // type of object
	Edges []Edge
}

type Defer struct {
	addr uint64
	gp   uint64
	argp uint64
	pc   uint64
	fn   uint64
	code uint64
	link uint64
}

type Panic struct {
	addr uint64
	gp   uint64
	typ  uint64
	data uint64
	defr uint64
	link uint64
}

type MemProfFrame struct {
	Func string
	File string
	Line uint64
}

type MemProfEntry struct {
	addr   uint64
	size   uint64
	stack  []MemProfFrame
	allocs uint64
	frees  uint64
}

type AllocSample struct {
	Addr uint64        // address of object
	Prof *MemProfEntry // record of allocation site
}

type Data struct {
	Addr   uint64
	Data   []byte
	Fields []Field
	Edges  []Edge
}

type OSThread struct {
	addr   uint64
	id     uint64
	procid uint64
}

// A Field is a location in an object where there
// might be a pointer.
type Field struct {
	Kind     FieldKind
	Offset   uint64
	Name     string
	BaseType string // base type for Ptr, Slice, Iface ("" if not known)
}

type GoRoutine struct {
	Bos  *StackFrame // frame at the top of the stack (i.e. currently running)
	Ctxt ObjId

	Addr         uint64
	bosaddr      uint64
	Goid         uint64
	Gopc         uint64
	Status       uint64
	IsSystem     bool
	IsBackground bool
	WaitSince    uint64
	WaitReason   string
	ctxtaddr     uint64
	maddr        uint64
	deferaddr    uint64
	panicaddr    uint64
}

type StackFrame struct {
	Name      string
	Parent    *StackFrame
	Goroutine *GoRoutine
	Depth     uint64
	Data      []byte
	Edges     []Edge

	Addr      uint64
	childaddr uint64
	entry     uint64
	pc        uint64
	Fields    []Field
}

// both an io.Reader and an io.ByteReader
type Reader interface {
	Read(p []byte) (n int, err error)
	ReadByte() (c byte, err error)
}

func readUint64(r Reader) uint64 {
	x, err := binary.ReadUvarint(r)
	if err != nil {
		log.Fatal(err)
	}
	return x
}

func readNBytes(r Reader, n uint64) []byte {
	s := make([]byte, n)
	_, err := io.ReadFull(r, s)
	if err != nil {
		log.Fatal(err)
	}
	return s
}

func readBytes(r Reader) []byte {
	n := readUint64(r)
	return readNBytes(r, n)
}

func readString(r Reader) string {
	return string(readBytes(r))
}

func readBool(r Reader) bool {
	b, err := r.ReadByte()
	if err != nil {
		log.Fatal(err)
	}
	return b != 0
}

func readFields(r Reader) []Field {
	var x []Field
	for {
		kind := FieldKind(readUint64(r))
		if kind == FieldKindEol {
			// TODO: sort by offset, or check that it is sorted
			return x
		}
		x = append(x, Field{Kind: kind, Offset: readUint64(r)})
	}
}

// A Reader that can tell you its current offset in the file.
type myReader struct {
	r   *bufio.Reader
	cnt int64
}

func (r *myReader) Read(p []byte) (n int, err error) {
	n, err = r.r.Read(p)
	r.cnt += int64(n)
	return
}
func (r *myReader) ReadByte() (c byte, err error) {
	c, err = r.r.ReadByte()
	if err != nil {
		return
	}
	r.cnt++
	return
}
func (r *myReader) ReadLine() (line []byte, isPrefix bool, err error) {
	line, isPrefix, err = r.r.ReadLine()
	r.cnt += int64(len(line)) + 1
	return
}
func (r *myReader) Skip(n int64) error {
	k, err := io.CopyN(ioutil.Discard, r.r, n)
	r.cnt += k
	return err
}
func (r *myReader) Count() int64 {
	return r.cnt
}

type tkey struct {
	size    uint64
	gcsig   string
}

func (d *Dump) makeFullType(size uint64, gcmap string) *FullType {
	name := fmt.Sprintf("%d_%s", size, gcmap)
	ft := &FullType{len(d.FTList), size, gcmap, name, nil}
	d.FTList = append(d.FTList, ft)
	return ft
}

// Reads heap dump into memory.
func rawRead(filename string) *Dump {
	file, err := os.Open(filename)
	if err != nil {
		log.Fatal(err)
	}
	r := &myReader{r: bufio.NewReader(file)}

	// check for header
	hdr, prefix, err := r.ReadLine()
	if err != nil {
		log.Fatal(err)
	}
	if prefix || string(hdr) != "go1.4 heap dump" {
		log.Fatal("not a go1.4 heap dump file")
	}

	var d Dump
	d.r = file
	d.ItabMap = map[uint64]uint64{}
	d.TypeMap = map[uint64]*Type{}
	ftmap := map[tkey]*FullType{} // full type dedup
	memprof := map[uint64]*MemProfEntry{}
	var sig []byte // buffer for reading a garbage collection signature
	for {
		kind := readUint64(r)
		switch kind {
		case tagObject:
			obj := object{}
			obj.Addr = readUint64(r)
			size := readUint64(r)
			obj.offset = r.Count()
			r.Skip(int64(size))

			// build a "signature" for the object.  This is its type
			// as far as the garbage collector is concerned.
			sig = sig[:0]
			var offset uint64
		gcloop:
			for {
				// P = pointer
				// S = scalar
				// I = iface
				// E = eface
				switch FieldKind(readUint64(r)) {
				case FieldKindPtr:
					for off := readUint64(r); offset < off; offset += d.PtrSize {
						sig = append(sig, 'S')
					}
					sig = append(sig, 'P')
					offset += d.PtrSize
				case FieldKindIface:
					for off := readUint64(r); offset < off; offset += d.PtrSize {
						sig = append(sig, 'S')
					}
					sig = append(sig, 'I', 'I')
					offset += 2*d.PtrSize
				case FieldKindEface:
					for off := readUint64(r); offset < off; offset += d.PtrSize {
						sig = append(sig, 'S')
					}
					sig = append(sig, 'E', 'E')
					offset += 2*d.PtrSize
				case FieldKindEol:
					break gcloop
				}
			}
			gcsig := string(sig)
			k := tkey{size,gcsig}
			ft := ftmap[k]
			if ft == nil {
				ft = d.makeFullType(size, gcsig)
				ftmap[k] = ft
			}
			obj.Ft = ft
			d.objects = append(d.objects, obj)
		case tagEOF:
			return &d
		case tagOtherRoot:
			t := &OtherRoot{}
			t.Description = readString(r)
			t.toaddr = readUint64(r)
			d.Otherroots = append(d.Otherroots, t)
		case tagType:
			typ := &Type{}
			typ.Addr = readUint64(r)
			typ.Size = readUint64(r)
			typ.Name = readString(r)
			typ.efaceptr = readBool(r)
			// Note: there may be duplicate type records in a dump.
			// The duplicates get thrown away here.
			if _, ok := d.TypeMap[typ.Addr]; !ok {
				d.TypeMap[typ.Addr] = typ
				d.Types = append(d.Types, typ)
			}
			//fmt.Printf("type %x\n", typ.Addr)
		case tagGoRoutine:
			g := &GoRoutine{}
			g.Addr = readUint64(r)
			g.bosaddr = readUint64(r)
			g.Goid = readUint64(r)
			g.Gopc = readUint64(r)
			g.Status = readUint64(r)
			g.IsSystem = readBool(r)
			g.IsBackground = readBool(r)
			g.WaitSince = readUint64(r)
			g.WaitReason = readString(r)
			g.ctxtaddr = readUint64(r)
			g.maddr = readUint64(r)
			g.deferaddr = readUint64(r)
			g.panicaddr = readUint64(r)
			d.Goroutines = append(d.Goroutines, g)
		case tagStackFrame:
			t := &StackFrame{}
			t.Addr = readUint64(r)
			t.Depth = readUint64(r)
			t.childaddr = readUint64(r)
			t.Data = readBytes(r)
			t.entry = readUint64(r)
			t.pc = readUint64(r)
			readUint64(r) // continpc
			t.Name = readString(r)
			t.Fields = readFields(r)
			d.Frames = append(d.Frames, t)
		case tagParams:
			if readUint64(r) == 0 {
				d.Order = binary.LittleEndian
			} else {
				d.Order = binary.BigEndian
			}
			d.PtrSize = readUint64(r)
			d.HeapStart = readUint64(r)
			d.HeapEnd = readUint64(r)
			d.TheChar = byte(readUint64(r))
			d.Experiment = readString(r)
			d.Ncpu = readUint64(r)
		case tagFinalizer:
			t := &Finalizer{}
			t.obj = readUint64(r)
			t.fn = readUint64(r)
			t.code = readUint64(r)
			t.fint = readUint64(r)
			t.ot = readUint64(r)
			d.Finalizers = append(d.Finalizers, t)
		case tagQFinal:
			t := &QFinalizer{}
			t.obj = readUint64(r)
			t.fn = readUint64(r)
			t.code = readUint64(r)
			t.fint = readUint64(r)
			t.ot = readUint64(r)
			d.QFinal = append(d.QFinal, t)
		case tagData:
			t := &Data{}
			t.Addr = readUint64(r)
			t.Data = readBytes(r)
			t.Fields = readFields(r)
			d.Data = t
		case tagBss:
			t := &Data{}
			t.Addr = readUint64(r)
			t.Data = readBytes(r)
			t.Fields = readFields(r)
			d.Bss = t
		case tagItab:
			addr := readUint64(r)
			typaddr := readUint64(r)
			d.ItabMap[addr] = typaddr
			fmt.Printf("itab %x %x\n", addr, typaddr)
		case tagOSThread:
			t := &OSThread{}
			t.addr = readUint64(r)
			t.id = readUint64(r)
			t.procid = readUint64(r)
			d.Osthreads = append(d.Osthreads, t)
		case tagMemStats:
			t := &runtime.MemStats{}
			t.Alloc = readUint64(r)
			t.TotalAlloc = readUint64(r)
			t.Sys = readUint64(r)
			t.Lookups = readUint64(r)
			t.Mallocs = readUint64(r)
			t.Frees = readUint64(r)
			t.HeapAlloc = readUint64(r)
			t.HeapSys = readUint64(r)
			t.HeapIdle = readUint64(r)
			t.HeapInuse = readUint64(r)
			t.HeapReleased = readUint64(r)
			t.HeapObjects = readUint64(r)
			t.StackInuse = readUint64(r)
			t.StackSys = readUint64(r)
			t.MSpanInuse = readUint64(r)
			t.MSpanSys = readUint64(r)
			t.MCacheInuse = readUint64(r)
			t.MCacheSys = readUint64(r)
			t.BuckHashSys = readUint64(r)
			t.GCSys = readUint64(r)
			t.OtherSys = readUint64(r)
			t.NextGC = readUint64(r)
			t.LastGC = readUint64(r)
			t.PauseTotalNs = readUint64(r)
			for i := 0; i < 256; i++ {
				t.PauseNs[i] = readUint64(r)
			}
			t.NumGC = uint32(readUint64(r))
			d.Memstats = t
		case tagDefer:
			t := &Defer{}
			t.addr = readUint64(r)
			t.gp = readUint64(r)
			t.argp = readUint64(r)
			t.pc = readUint64(r)
			t.fn = readUint64(r)
			t.code = readUint64(r)
			t.link = readUint64(r)
			d.Defers = append(d.Defers, t)
		case tagPanic:
			t := &Panic{}
			t.addr = readUint64(r)
			t.gp = readUint64(r)
			t.typ = readUint64(r)
			t.data = readUint64(r)
			t.defr = readUint64(r)
			t.link = readUint64(r)
			d.Panics = append(d.Panics, t)
		case tagMemProf:
			t := &MemProfEntry{}
			key := readUint64(r)
			t.size = readUint64(r)
			nstk := readUint64(r)
			for i := uint64(0); i < nstk; i++ {
				fn := readString(r)
				file := readString(r)
				line := readUint64(r)
				// TODO: intern fn, file.  They will repeat a lot.
				t.stack = append(t.stack, MemProfFrame{fn, file, line})
			}
			t.allocs = readUint64(r)
			t.frees = readUint64(r)
			d.MemProf = append(d.MemProf, t)
			memprof[key] = t
		case tagAllocSample:
			t := &AllocSample{}
			t.Addr = readUint64(r)
			t.Prof = memprof[readUint64(r)]
			d.AllocSamples = append(d.AllocSamples, t)
		default:
			log.Fatal("unknown record kind ", kind)
		}
	}
	// TODO: any easy way to truncate the objects array?  We could
	// reclaim the fraction that append() added but we didn't need.
}

func getDwarf(execname string) *dwarf.Data {
	e, err := elf.Open(execname)
	if err == nil {
		defer e.Close()
		d, err := e.DWARF()
		if err == nil {
			return d
		}
	}
	m, err := macho.Open(execname)
	if err == nil {
		defer m.Close()
		d, err := m.DWARF()
		if err == nil {
			return d
		}
	}
	p, err := pe.Open(execname)
	if err == nil {
		defer p.Close()
		d, err := p.DWARF()
		if err == nil {
			return d
		}
	}
	log.Fatal("can't get dwarf info from executable", err)
	return nil
}

func readUleb(b []byte) ([]byte, uint64) {
	r := uint64(0)
	s := uint(0)
	for {
		x := b[0]
		b = b[1:]
		r |= uint64(x&127) << s
		if x&128 == 0 {
			break
		}
		s += 7

	}
	return b, r
}
func readSleb(b []byte) ([]byte, int64) {
	c, v := readUleb(b)
	// sign extend
	k := (len(b) - len(c)) * 7
	return c, int64(v) << uint(64-k) >> uint(64-k)
}

func joinNames(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return fmt.Sprintf("%s.%s", a, b)
}

type dwarfType interface {
	// Name returns the name of this type
	Name() string
	// Size returns the size of this type in bytes
	Size() uint64
	// Fields returns a list of fields within the object, in increasing offset order.
	Fields() []Field
	// dwarfFields returns a list of fields within the type.
	// The list is "flattened", so only base & ptr types remain. (TODO: and func, for now)
	// We call this dynamically instead of building it for each type
	// when the type is constructed, so we avoid constructing this list for
	// crazy types that are never instantiated, e.g. [1000000000]byte.
	dwarfFields() []dwarfTypeMember
}
type dwarfTypeImpl struct {
	name   string
	size   uint64
	fields []Field
	dFields []dwarfTypeMember
}
type dwarfBaseType struct {
	dwarfTypeImpl
	encoding int64
}
type dwarfTypedef struct {
	dwarfTypeImpl
	type_ dwarfType
}
type dwarfStructType struct {
	dwarfTypeImpl
	members []dwarfTypeMember
}
type dwarfTypeMember struct {
	offset uint64
	name   string
	type_  dwarfType
}
type dwarfPtrType struct {
	dwarfTypeImpl
	elem dwarfType
}
type dwarfArrayType struct {
	dwarfTypeImpl
	elem dwarfType
}
type dwarfFuncType struct {
	dwarfTypeImpl
}

func (t *dwarfTypeImpl) Name() string {
	return t.name
}
func (t *dwarfTypeImpl) Size() uint64 {
	return t.size
}
func (t *dwarfBaseType) Fields() []Field {
	if t.fields != nil {
		return t.fields
	}
	switch {
	case t.encoding == dw_ate_boolean:
		t.fields = append(t.fields, Field{FieldKindBool, 0, "", ""})
	case t.encoding == dw_ate_signed && t.size == 1:
		t.fields = append(t.fields, Field{FieldKindSInt8, 0, "", ""})
	case t.encoding == dw_ate_unsigned && t.size == 1:
		t.fields = append(t.fields, Field{FieldKindUInt8, 0, "", ""})
	case t.encoding == dw_ate_signed && t.size == 2:
		t.fields = append(t.fields, Field{FieldKindSInt16, 0, "", ""})
	case t.encoding == dw_ate_unsigned && t.size == 2:
		t.fields = append(t.fields, Field{FieldKindUInt16, 0, "", ""})
	case t.encoding == dw_ate_signed && t.size == 4:
		t.fields = append(t.fields, Field{FieldKindSInt32, 0, "", ""})
	case t.encoding == dw_ate_unsigned && t.size == 4:
		t.fields = append(t.fields, Field{FieldKindUInt32, 0, "", ""})
	case t.encoding == dw_ate_signed && t.size == 8:
		t.fields = append(t.fields, Field{FieldKindSInt64, 0, "", ""})
	case t.encoding == dw_ate_unsigned && t.size == 8:
		t.fields = append(t.fields, Field{FieldKindUInt64, 0, "", ""})
	case t.encoding == dw_ate_float && t.size == 4:
		t.fields = append(t.fields, Field{FieldKindFloat32, 0, "", ""})
	case t.encoding == dw_ate_float && t.size == 8:
		t.fields = append(t.fields, Field{FieldKindFloat64, 0, "", ""})
	case t.encoding == dw_ate_complex_float && t.size == 8:
		t.fields = append(t.fields, Field{FieldKindComplex64, 0, "", ""})
	case t.encoding == dw_ate_complex_float && t.size == 16:
		t.fields = append(t.fields, Field{FieldKindComplex128, 0, "", ""})
	default:
		log.Fatalf("unknown encoding type encoding=%d size=%d", t.encoding, t.size)
	}
	return t.fields
}
func (t *dwarfBaseType) dwarfFields() []dwarfTypeMember {
	if t.dFields != nil {
		return t.dFields
	}
	t.dFields = append(t.dFields, dwarfTypeMember{0,"",t}) // TODO: infinite recursion?
	return t.dFields
}

func (t *dwarfTypedef) Fields() []Field {
	return t.type_.Fields()
}
func (t *dwarfTypedef) dwarfFields() []dwarfTypeMember {
	return t.type_.dwarfFields()
}
func (t *dwarfTypedef) Size() uint64 {
	return t.type_.Size()
}

var unkBase = "unkBase"

func (t *dwarfPtrType) Fields() []Field {
	if t.fields == nil {
		if t.Name()[0] == '*' {
			t.fields = append(t.fields, Field{FieldKindPtr, 0, "", t.Name()[1:]})
		} else {
			t.fields = append(t.fields, Field{FieldKindPtr, 0, "", unkBase})
		}
	}
	return t.fields
}
func (t *dwarfPtrType) dwarfFields() []dwarfTypeMember {
	if t.dFields == nil {
		t.dFields = append(t.dFields, dwarfTypeMember{0, "", t})
	}
	return t.dFields
}

// We treat a func as a *uintptr.  (It is actually a pointer to a closure, which is
// in turn a pointer to code.)
// TODO: how do we deduce types of closure parameters???  We could look at the code
// pointer and figure it out somehow.
// TODO: parameterize size by d.PtrSize.
var dwarfCodePtr dwarfType = &dwarfBaseType{dwarfTypeImpl{"<codeptr>",8,nil,nil}, dw_ate_unsigned}
var dwarfFunc dwarfType = &dwarfPtrType{dwarfTypeImpl{"*<closure>", 8, nil, nil}, dwarfCodePtr}

func (t *dwarfFuncType) Fields() []Field {
	if t.fields == nil {
		t.fields = append(t.fields, Field{FieldKindPtr, 0, "", unkBase})
	}
	return t.fields
}

func (t *dwarfFuncType) dwarfFields() []dwarfTypeMember {
	if t.dFields == nil {
		t.dFields = append(t.dFields, dwarfTypeMember{0, "", dwarfFunc})
	}
	return t.dFields
}

func (t *dwarfStructType) Fields() []Field {
	if t.fields != nil {
		return t.fields
	}
	// Iterate over members, flatten fields.
	// Don't look inside strings, interfaces, slices.
	switch {
	case t.name == "string":
		
		t.fields = append(t.fields, Field{FieldKindPtr, 0, "", ""}, Field{FieldKindUInt64, 0, "", ""}) // TODO: uint32 for 32-bit?
	case t.name == "runtime.iface":
		t.fields = append(t.fields, Field{FieldKindPtr, 0, "", unkBase}, Field{FieldKindPtr, 0, "", unkBase})
	case t.name == "runtime.eface":
		t.fields = append(t.fields, Field{FieldKindEface, 0, "", ""}, Field{FieldKindEface, 0, "", ""})
	default:
		/*
		// Detect slices.  TODO: This could be fooled by the right user
		// code, so find a better way.
		if len(t.members) == 3 &&
			t.members[0].name == "array" &&
			t.members[1].name == "len" &&
			t.members[2].name == "cap" &&
			t.members[0].offset == 0 &&
			t.members[1].offset == t.members[0].type_.Size() &&
			t.members[2].offset == 2*t.members[0].type_.Size() {
			_, aok := t.members[0].type_.(*dwarfPtrType)
			l, lok := t.members[1].type_.(*dwarfBaseType)
			c, cok := t.members[2].type_.(*dwarfBaseType)
			if aok && lok && cok && l.encoding == dw_ate_unsigned && c.encoding == dw_ate_unsigned {
				t.fields = append(t.fields, Field{FieldKindSlice, 0, "", t.members[0].type_.Name()[1:]})
				break
			}
		}
		*/

		for _, m := range t.members {
			for _, f := range m.type_.Fields() {
				t.fields = append(t.fields, Field{f.Kind, m.offset + f.Offset, joinNames(m.name, f.Name), f.BaseType})
			}
		}
	}
	return t.fields
}

func (t *dwarfStructType) dwarfFields() []dwarfTypeMember {
	if t.dFields != nil {
		return t.dFields
	}
	// Iterate over members, flatten fields.
	for _, m := range t.members {
		for _, f := range m.type_.dwarfFields() {
			t.dFields = append(t.dFields, dwarfTypeMember{m.offset + f.offset, joinNames(m.name, f.name), f.type_})
		}
	}
	return t.dFields
}

func (t *dwarfArrayType) Fields() []Field {
	if t.fields != nil {
		return t.fields
	}
	s := t.elem.Size()
	if s == 0 {
		return t.fields
	}
	n := t.Size() / s
	fields := t.elem.Fields()
	for i := uint64(0); i < n; i++ {
		for _, f := range fields {
			t.fields = append(t.fields, Field{f.Kind, i*s + f.Offset, joinNames(fmt.Sprintf("%d", i), f.Name), f.BaseType})
		}
	}
	return t.fields
}

func (t *dwarfArrayType) dwarfFields() []dwarfTypeMember {
	if t.dFields != nil {
		return t.dFields
	}
	s := t.elem.Size()
	if s == 0 {
		return t.dFields
	}
	n := t.Size() / s
	fields := t.elem.dwarfFields()
	for i := uint64(0); i < n; i++ {
		name := fmt.Sprintf("[%d]", i)
		for _, f := range fields {
			t.dFields = append(t.dFields, dwarfTypeMember{i*s + f.offset, joinNames(name, f.name), f.type_})
		}
	}
	return t.dFields
}

// Some type names in the dwarf info don't match the corresponding
// type names in the binary.  We'll use the rewrites here to map
// between the two.
// TODO: just map names for now.  Rename this?  Do this conversion in the dwarf dumper?
type adjTypeName struct {
	matcher   *regexp.Regexp
	formatter string
}

var adjTypeNames = []adjTypeName{
	{regexp.MustCompile(`hash<(.*),(.*)>`), "map.hdr[%s]%s"},
	{regexp.MustCompile(`bucket<(.*),(.*)>`), "map.bucket[%s]%s"},
}

// load a map of all of the dwarf types
func dwarfTypeMap(d *Dump, w *dwarf.Data) map[dwarf.Offset]dwarfType {
	t := make(map[dwarf.Offset]dwarfType)

	// pass 1: make a dwarfType for all of the types in the file
	r := w.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			log.Fatal(err)
		}
		if e == nil {
			break
		}
		switch e.Tag {
		case dwarf.TagBaseType:
			x := new(dwarfBaseType)
			x.name = e.Val(dwarf.AttrName).(string)
			x.size = uint64(e.Val(dwarf.AttrByteSize).(int64))
			x.encoding = e.Val(dwarf.AttrEncoding).(int64)
			t[e.Offset] = x
		case dwarf.TagPointerType:
			x := new(dwarfPtrType)
			x.name = e.Val(dwarf.AttrName).(string)
			x.size = d.PtrSize
			t[e.Offset] = x
		case dwarf.TagStructType:
			x := new(dwarfStructType)
			x.name = e.Val(dwarf.AttrName).(string)
			x.size = uint64(e.Val(dwarf.AttrByteSize).(int64))
			log.Printf("making struct %s", x.name)
			for _, a := range adjTypeNames {
				if k := a.matcher.FindStringSubmatch(x.name); k != nil {
					var i []interface{}
					for _, j := range k[1:] {
						i = append(i, j)
					}
					x.name = fmt.Sprintf(a.formatter, i...)
				}
			}
			t[e.Offset] = x
		case dwarf.TagArrayType:
			x := new(dwarfArrayType)
			x.name = e.Val(dwarf.AttrName).(string)
			x.size = uint64(e.Val(dwarf.AttrByteSize).(int64))
			t[e.Offset] = x
		case dwarf.TagTypedef:
			x := new(dwarfTypedef)
			x.name = e.Val(dwarf.AttrName).(string)
			t[e.Offset] = x
		case dwarf.TagSubroutineType:
			x := new(dwarfFuncType)
			x.name = e.Val(dwarf.AttrName).(string)
			x.size = d.PtrSize
			t[e.Offset] = x
		}
	}

	// pass 2: fill in / link up the types
	r = w.Reader()
	var currentStruct *dwarfStructType
	for {
		e, err := r.Next()
		if err != nil {
			log.Fatal(err)
		}
		if e == nil {
			break
		}
		switch e.Tag {
		case dwarf.TagTypedef:
			t[e.Offset].(*dwarfTypedef).type_ = t[e.Val(dwarf.AttrType).(dwarf.Offset)]
			if t[e.Offset].(*dwarfTypedef).type_ == nil {
				log.Fatalf("can't find referent for %s %d\n", t[e.Offset].(*dwarfTypedef).name, e.Val(dwarf.AttrType).(dwarf.Offset))
			}
		case dwarf.TagPointerType:
			i := e.Val(dwarf.AttrType)
			if i != nil {
				t[e.Offset].(*dwarfPtrType).elem = t[i.(dwarf.Offset)]
			} else {
				// The only nil cases are unsafe.Pointer and reflect.iword
				if t[e.Offset].Name() != "unsafe.Pointer" &&
					t[e.Offset].Name() != "crypto/x509._Ctype_CFTypeRef" {
					log.Fatalf("pointer without base pointer %s", t[e.Offset].Name())
				}
			}
		case dwarf.TagArrayType:
			t[e.Offset].(*dwarfArrayType).elem = t[e.Val(dwarf.AttrType).(dwarf.Offset)]
		case dwarf.TagStructType:
			currentStruct = t[e.Offset].(*dwarfStructType)
		case dwarf.TagMember:
			name := e.Val(dwarf.AttrName).(string)
			type_ := t[e.Val(dwarf.AttrType).(dwarf.Offset)]
			loc := e.Val(dwarf.AttrDataMemberLoc).([]uint8)
			var offset uint64
			if len(loc) == 0 {
				offset = 0
			} else if loc[0] == dw_op_plus_uconst {
				loc, offset = readUleb(loc[1:])
			} else if len(loc) >= 2 && loc[0] == dw_op_consts && loc[len(loc)-1] == dw_op_plus {
				loc, offset = readUleb(loc[1 : len(loc)-1])
				if len(loc) != 0 {
					break
				}
			} else {
				log.Fatalf("bad dwarf location spec %#v", loc)
			}
			currentStruct.members = append(currentStruct.members, dwarfTypeMember{offset, name, type_})
		}
	}
	
	// TODO: remove
	if false {
	for _, typ := range t {
		fmt.Println(typ.Name())
		n := 0
		for _, f := range typ.dwarfFields() {
			if f.type_ != nil {
				fmt.Printf("  %d %s %s\n", f.offset, f.name, f.type_.Name())
			} else {
				fmt.Printf("  %d %s ==NIL==\n", f.offset, f.name)
			}
			n++
			if n > 100 { break }
		}
	}
	}
	return t
}

type localKey struct {
	funcname string
	offset   uint64 // distance down from frame pointer
}

// Makes a map from <function name, distance before top of frame> to name of field.
func localsMap(d *Dump, w *dwarf.Data, t map[dwarf.Offset]dwarfType) map[localKey]string {
	m := make(map[localKey]string, 0)
	r := w.Reader()
	var funcname string
	for {
		e, err := r.Next()
		if err != nil {
			log.Fatal(err)
		}
		if e == nil {
			break
		}
		switch e.Tag {
		case dwarf.TagSubprogram:
			funcname = e.Val(dwarf.AttrName).(string)
		case dwarf.TagVariable:
			name := e.Val(dwarf.AttrName).(string)
			typ := t[e.Val(dwarf.AttrType).(dwarf.Offset)]
			loc := e.Val(dwarf.AttrLocation).([]uint8)
			if len(loc) == 0 || loc[0] != dw_op_call_frame_cfa {
				break
			}
			var offset int64
			if len(loc) == 1 {
				offset = 0
			} else if len(loc) >= 3 && loc[1] == dw_op_consts && loc[len(loc)-1] == dw_op_plus {
				loc, offset = readSleb(loc[2 : len(loc)-1])
				if len(loc) != 0 {
					break
				}
			}
			for _, f := range typ.Fields() {
				m[localKey{funcname, uint64(-offset) - f.Offset}] = joinNames(name, f.Name)
			}
		}
	}
	return m
}

// Makes a map from <function name, offset in arg area> to name of field.
func argsMap(d *Dump, w *dwarf.Data, t map[dwarf.Offset]dwarfType) map[localKey]string {
	m := make(map[localKey]string, 0)
	r := w.Reader()
	var funcname string
	for {
		e, err := r.Next()
		if err != nil {
			log.Fatal(err)
		}
		if e == nil {
			break
		}
		switch e.Tag {
		case dwarf.TagSubprogram:
			funcname = e.Val(dwarf.AttrName).(string)
		case dwarf.TagFormalParameter:
			if e.Val(dwarf.AttrName) == nil {
				continue
			}
			name := e.Val(dwarf.AttrName).(string)
			typ := t[e.Val(dwarf.AttrType).(dwarf.Offset)]
			loc := e.Val(dwarf.AttrLocation).([]uint8)
			if len(loc) == 0 || loc[0] != dw_op_call_frame_cfa {
				break
			}
			var offset int64
			if len(loc) == 1 {
				offset = 0
			} else if len(loc) >= 3 && loc[1] == dw_op_consts && loc[len(loc)-1] == dw_op_plus {
				loc, offset = readSleb(loc[2 : len(loc)-1])
				if len(loc) != 0 {
					break
				}
			}
			for _, f := range typ.Fields() {
				m[localKey{funcname, uint64(offset)}] = joinNames(name, f.Name)
			}
		}
	}
	return m
}

// map from global address to Field at that address
func globalsMap(d *Dump, w *dwarf.Data, t map[dwarf.Offset]dwarfType) *heap {
	h := new(heap)
	r := w.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			log.Fatal(err)
		}
		if e == nil {
			break
		}
		if e.Tag != dwarf.TagVariable {
			continue
		}
		name := e.Val(dwarf.AttrName).(string)
		typ := t[e.Val(dwarf.AttrType).(dwarf.Offset)]
		locexpr := e.Val(dwarf.AttrLocation).([]uint8)
		if len(locexpr) == 0 || locexpr[0] != dw_op_addr {
			continue
		}
		loc := readPtr(d, locexpr[1:])
		if typ == nil {
			// lots of non-Go global symbols hit here (rodata, reflect.cvtFloat·f, ...)
			h.Insert(loc, Field{FieldKindPtr, 0, "~" + name, ""})
			continue
		}
		for _, f := range typ.Fields() {
			h.Insert(loc+f.Offset, Field{f.Kind, 0, joinNames(name, f.Name), f.BaseType})
		}
	}
	return h
}

func globalRoots(d *Dump, w *dwarf.Data, t map[dwarf.Offset]dwarfType) []dwarfTypeMember {
	var roots []dwarfTypeMember
	r := w.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			log.Fatal(err)
		}
		if e == nil {
			break
		}
		if e.Tag != dwarf.TagVariable {
			continue
		}
		name := e.Val(dwarf.AttrName).(string)
		typ := t[e.Val(dwarf.AttrType).(dwarf.Offset)]
		locexpr := e.Val(dwarf.AttrLocation).([]uint8)
		if len(locexpr) == 0 || locexpr[0] != dw_op_addr {
			continue
		}
		loc := readPtr(d, locexpr[1:])
		if typ == nil {
			// lots of non-Go global symbols hit here (rodata, type..gc,
			// static function closures, ...)
			fmt.Printf("nontyped global %s %d\n", name, loc)
			continue
		}
		roots = append(roots, dwarfTypeMember{loc, name, typ})
	}
	return roots
}

type frameLayout struct {
	// offset is distance down from FP
	locals []dwarfTypeMember
	// offset is distance up from first arg slot
	args []dwarfTypeMember
}

// frameLayouts returns a map from function names to frameLayouts describing that function's stack frame.
func frameLayouts(d *Dump, w *dwarf.Data, t map[dwarf.Offset]dwarfType) map[string]frameLayout {
	m := map[string]frameLayout{}
	var locals []dwarfTypeMember
	var args []dwarfTypeMember
	r := w.Reader()
	var funcname string
	for {
		e, err := r.Next()
		if err != nil {
			log.Fatal(err)
		}
		if e == nil {
			break
		}
		switch e.Tag {
		case dwarf.TagSubprogram:
			if funcname != "" {
				m[funcname] = frameLayout{locals, args}
				locals = nil
				args = nil
			}
			funcname = e.Val(dwarf.AttrName).(string)
		case dwarf.TagVariable:
			name := e.Val(dwarf.AttrName).(string)
			typ := t[e.Val(dwarf.AttrType).(dwarf.Offset)]
			loc := e.Val(dwarf.AttrLocation).([]uint8)
			if len(loc) == 0 || loc[0] != dw_op_call_frame_cfa {
				continue
			}
			var offset int64
			if len(loc) == 1 {
				offset = 0
			} else if len(loc) >= 3 && loc[1] == dw_op_consts && loc[len(loc)-1] == dw_op_plus {
				loc, offset = readSleb(loc[2 : len(loc)-1])
				if len(loc) != 0 {
					continue
				}
			}
			locals = append(locals, dwarfTypeMember{uint64(-offset), name, typ})
		case dwarf.TagFormalParameter:
			if e.Val(dwarf.AttrName) == nil {
				continue
			}
			name := e.Val(dwarf.AttrName).(string)
			typ := t[e.Val(dwarf.AttrType).(dwarf.Offset)]
			loc := e.Val(dwarf.AttrLocation).([]uint8)
			if len(loc) == 0 || loc[0] != dw_op_call_frame_cfa {
				continue
			}
			var offset int64
			if len(loc) == 1 {
				offset = 0
			} else if len(loc) >= 3 && loc[1] == dw_op_consts && loc[len(loc)-1] == dw_op_plus {
				loc, offset = readSleb(loc[2 : len(loc)-1])
				if len(loc) != 0 {
					continue
				}
			}
			args = append(args, dwarfTypeMember{uint64(offset), name, typ})
		}
	}
	if funcname != "" {
		m[funcname] = frameLayout{locals, args}
	}
	return m
}

// stack frames may be zero-sized, so we add call depth
// to the key to ensure uniqueness.
type frameKey struct {
	sp    uint64
	depth uint64
}

// appendEdge might add an edge to edges.  Returns new edges.
//   Requires data[off:] be a pointer
//   Adds an edge if that pointer points to a valid object.
func (d *Dump) appendEdge(edges []Edge, data []byte, off uint64, f Field) []Edge {
	p := readPtr(d, data[off:])
	q := d.FindObj(p)
	if q != ObjNil {
		edges = append(edges, Edge{q, off, p - d.objects[q].Addr, f.Name})
	}
	return edges
}

func (d *Dump) appendFields(edges []Edge, data []byte, fields []Field) []Edge {
	//fmt.Println("appending fields")
	for _, f := range fields {
		//fmt.Printf("field %d %d %s %s\n", f.Kind, f.Offset, f.Name, f.BaseType)
		off := f.Offset
		if off >= uint64(len(data)) {
			// TODO: what the heck is this?
			continue
		}
		switch f.Kind {
		case FieldKindPtr:
			edges = d.appendEdge(edges, data, off, f)
		case FieldKindString:
			edges = d.appendEdge(edges, data, off, f)
		case FieldKindSlice:
			edges = d.appendEdge(edges, data, off, f)
		case FieldKindEface:
			edges = d.appendEdge(edges, data, off, f)
			taddr := readPtr(d, data[off:])
			if taddr == 0 {
				continue // nil eface
			}
			t := d.TypeMap[taddr]
			if t == nil {
				log.Fatalf("can't find eface type %x", taddr)
			}
			if t.efaceptr {
				edges = d.appendEdge(edges, data, off+d.PtrSize, f)
			}
		case FieldKindIface:
			itab := readPtr(d, data[off:])
			if itab == 0 {
				continue // nil iface
			}
			taddr, ok := d.ItabMap[itab]
			if !ok {
				log.Fatalf("can't find itab %x", itab)
			}
			if taddr == 0 {
				// this type has a non-pointer data field
				continue
			}
			t := d.TypeMap[taddr]
			if t == nil {
				log.Fatalf("can't find type for itab %x", taddr)
			}
			if t.efaceptr {
				edges = d.appendEdge(edges, data, off+d.PtrSize, f)
			}
		}
	}
	return edges
}

func typePropagate(d *Dump, execname string) {
	w := getDwarf(execname)
	t := dwarfTypeMap(d, w)

	// map from heap address to type at that address
	htypes := map[uint64]dwarfType{}

	// set of addresses from which we still need to propagate
	var addrq []uint64

	// set types of objects which are pointed to by globals
	for _, r := range globalRoots(d, w, t) {
		var off uint64
		var b []byte
		switch {
		case r.offset>=d.Data.Addr && r.offset < d.Data.Addr+uint64(len(d.Data.Data)):
			b = d.Data.Data
			off = r.offset - d.Data.Addr
		case r.offset>=d.Bss.Addr && r.offset < d.Bss.Addr+uint64(len(d.Bss.Data)):
			b = d.Bss.Data
			off = r.offset - d.Bss.Addr
		default:
			log.Printf("global address %s %x not in data [%x %x] or bss [%x %x]", r.name, r.offset, d.Data.Addr, d.Data.Addr+uint64(len(d.Data.Data)), d.Bss.Addr, d.Bss.Addr+uint64(len(d.Bss.Data)))
			continue
		}
		for _, f := range r.type_.dwarfFields() {
			// we've squashed typedef, struct, array at this point.
			switch t := f.type_.(type) {
			case *dwarfPtrType:
				p := readPtr(d, b[off + f.offset:])
				if t.elem != nil {
					// t.elem is nil for unsafe.Pointer
					if setType(d, htypes, p, t.elem) {
						addrq = append(addrq, p)
					}
				}
			case *dwarfBaseType:
				// nothing to do
			default:
				log.Fatalf("unknown type for field %#v", f)
			}
		}
	}

	// set types of objects which are pointed to by stacks
	layouts := frameLayouts(d, w, t)
	log.Printf("locals & args\n")
	live := map[uint64]bool{}
	for _, g := range d.Goroutines {
		var c *StackFrame
		for r := g.Bos; r != nil; r = r.Parent {
			log.Printf("func %s %x", r.Name, len(r.Data))
			for k := range live {
				delete(live, k)
			}
			for _, f := range r.Fields {
				switch f.Kind {
				case FieldKindPtr:
					log.Printf("liveptr %x\n", f.Offset)
					live[f.Offset] = true
				case FieldKindIface, FieldKindEface:
					log.Printf("liveiface %x %x\n", f.Offset, f.Offset+d.PtrSize)
					live[f.Offset] = true
					live[f.Offset+d.PtrSize] = true
				}
			}
			for i := 0; i < len(r.Data); i+=8 {
				log.Printf("%x: %x %x %x %x %x %x %x %x", i, r.Data[i+0],r.Data[i+1],r.Data[i+2],r.Data[i+3],r.Data[i+4],r.Data[i+5],r.Data[i+6],r.Data[i+7])
			}

			// find live pointers, propagate types along them
			for _, local := range layouts[r.Name].locals {
				log.Printf("  local %s @ %x", local.name, uint64(len(r.Data))-local.offset)
				for _, f := range local.type_.dwarfFields() {
					log.Printf("    field %s @ %x", f.name, uint64(len(r.Data))-local.offset+f.offset)
					switch t := f.type_.(type) {
					case *dwarfPtrType:
						if t.elem == nil {
							// untyped pointer
							continue
						}
						i := uint64(len(r.Data)) - local.offset + f.offset
						if !live[i] {
							continue
						}
						p := readPtr(d, r.Data[i:])
						if setType(d, htypes, p, t.elem) {
							addrq = append(addrq, p)
						}
					case *dwarfBaseType:
						// nothing to do
					default:
						log.Fatalf("unknown type for field %#v", f)
					}
				}
			}
			if c != nil {
				// find live pointers in outargs section
				for _, arg := range layouts[c.Name].args {
					log.Printf("  arg %s @ %x", arg.name, arg.offset)
					for _, f := range arg.type_.dwarfFields() {
						switch t := f.type_.(type) {
						case *dwarfPtrType:
							if t.elem == nil {
								// untyped pointer
								continue
							}
							i := arg.offset + f.offset
							if !live[i] {
								continue
							}
							p := readPtr(d, r.Data[i:])
							if setType(d, htypes, p, t.elem) {
								addrq = append(addrq, p)
							}
						case *dwarfBaseType:
							// nothing to do
						default:
							log.Fatalf("unknown type for field %#v", f)
						}
					}
				}
			}
			c = r
		}
	}
	
	// propagate types
	log.Println("propagating")
	for len(addrq) > 0 {
		addr := addrq[len(addrq)-1]
		addrq = addrq[:len(addrq)-1]
		typ := htypes[addr]
		
		obj := d.FindObj(addr)
		if obj == ObjNil {
			// TODO: what is going on here?
			log.Printf("pointer not to valid heap object %x %s", addr, typ.Name())
			continue
		}
		base := d.Addr(obj)
		data := d.Contents(obj)
		if typ.Size() > uint64(len(data)) {
			log.Fatalf("type=%s/%d is too big for object %d", typ.Name(), typ.Size(), len(data))
		}
		for _, f := range typ.dwarfFields() {
			if f.offset >= uint64(len(data)) {
				log.Fatalf("type too big for object %s %v %d", typ.Name(), f, typ.Size())
			}
			switch t := f.type_.(type) {
			case *dwarfPtrType:
				if t.elem == nil {
					break
				}
				p := readPtr(d, data[addr + f.offset - base:])
				if setType(d, htypes, p, t.elem) {
					addrq = append(addrq, p)
				}
			}
		}
	}

	// update types of known objects
	dwarfToFull := map[dwarfType]*FullType{}
	for i := 0; i < d.NumObjects(); i++ {
		x := ObjId(i)
		addr := d.Addr(x)
		if t, ok := htypes[addr]; ok {
			ft, ok := dwarfToFull[t]
			if !ok {
				ft = &FullType{len(d.FTList), t.Size(), "", t.Name(), nil}
				d.FTList = append(d.FTList, ft)
				dwarfToFull[t] = ft
			}
			d.objects[x].Ft = ft
		}
	}
}

func setType(d *Dump, htypes map[uint64]dwarfType, addr uint64, typ dwarfType) bool {
	if addr < d.HeapStart || addr >= d.HeapEnd {
		return false
	}
	if true {
		obj := d.FindObj(addr)
		if obj != ObjNil {
			if typ.Size() > d.Size(obj) {
				log.Printf("%x: objsize:%d typsize:%d typ:%s", addr, d.Size(obj), typ.Size(), typ.Name())
				panic("foo")
			}
		}
	}
	if oldtyp, ok := htypes[addr]; ok {
		if typ != oldtyp {
			log.Printf("type mismatch in heap %x %s %s", addr, oldtyp.Name(), typ.Name())
		}
		// TODO: containment should be allowed.  Pick bigger type.
		return false
	}
	htypes[addr] = typ
	fmt.Printf("%x: %s\n", addr, typ.Name())
	return true
}

// Names the fields it can for better debugging output
func nameWithDwarf(d *Dump, execname string) {
	w := getDwarf(execname)
	t := dwarfTypeMap(d, w)

	// name fields in all types
	m := make(map[string]dwarfType)
	for _, x := range t {
		m[x.Name()] = x
	}
	for _, t := range d.Types {
		dt := m[t.Name]
		if dt == nil {
			// A type in the dump has no entry in the Dwarf info.
			// This can happen for unexported types, e.g. reflect.ptrGC.
			//log.Printf("type %s has no dwarf info", t.Name)
			continue
		}
		// Check that the Dwarf type is consistent with the type we got from
		// the heap dump.  The heap dump type is the root truth, but it is
		// missing non-pointer-bearing fields and has no field names.  If the
		// Dwarf type is consistent with the heap dump type, then we'll use
		// the fields from the Dwarf type instead.
		consistent := true

		// load Dwarf fields into layout
		df := dt.Fields()
		log.Print(df)
		layout := make(map[uint64]Field)
		for _, f := range df {
			layout[f.Offset] = f
		}
		log.Print(layout)
		log.Print(t.Fields)
		// A field in the heap dump must match the corresponding Dwarf field
		// in both kind and offset.
		for _, f := range t.Fields {
			if layout[f.Offset].Kind != f.Kind {
				log.Printf("dwarf field kind doesn't match dump kind %s.%d dwarf=%d dump=%d", t.Name, f.Offset, layout[f.Offset].Kind, f.Kind)
				consistent = false
			}
			delete(layout, f.Offset)
		}
		// all remaining fields must not be pointer-containing
		for _, f := range layout {
			switch f.Kind {
			case FieldKindPtr, FieldKindIface, FieldKindEface:
				log.Printf("dwarf type has additional ptr field %s %d %d", f.Name, f.Offset, f.Kind)
				consistent = false
			}
		}
		if consistent {
			// Dwarf info looks good, overwrite the fields from the dump
			// with fields from the Dwarf info.
			t.Fields = df
		} else {
			log.Print("inconsistent type for ", t.Name)
		}
	}

	// link up frames in sequence
	// TODO: already do this later in link
	frames := make(map[frameKey]*StackFrame, len(d.Frames))
	for _, x := range d.Frames {
		frames[frameKey{x.Addr, x.Depth}] = x
	}
	for _, f := range d.Frames {
		if f.Depth == 0 {
			continue
		}
		g := frames[frameKey{f.childaddr, f.Depth - 1}]
		g.Parent = f
	}
	for _, g := range d.Goroutines {
		g.Bos = frames[frameKey{g.bosaddr, 0}]
	}

	// name all frame fields
	locals := localsMap(d, w, t)
	args := argsMap(d, w, t)
	for _, g := range d.Goroutines {
		var c *StackFrame
		for r := g.Bos; r != nil; r = r.Parent {
			for i, f := range r.Fields {
				name := locals[localKey{r.Name, uint64(len(r.Data)) - f.Offset}]
				if name == "" && c != nil {
					name = args[localKey{c.Name, f.Offset}]
					if name != "" {
						name = "outarg." + name
					}
				}
				if name == "" {
					name = fmt.Sprintf("~%d", f.Offset)
				}
				r.Fields[i].Name = name
			}
			c = r
		}
	}

	// naming for globals
	globals := globalsMap(d, w, t)
	for _, x := range []*Data{d.Data, d.Bss} {
		for i, f := range x.Fields {
			addr := x.Addr + f.Offset
			a, v := globals.Lookup(addr)
			if v == nil {
				continue
			}
			ff := v.(Field)
			if a != addr {
				ff.Name = fmt.Sprintf("%s:%d", ff.Name, addr-a)
			}
			ff.Offset = f.Offset
			x.Fields[i] = ff
		}
	}
}

func link1(d *Dump) {
	// sort objects in increasing address order
	sort.Sort(byAddr(d.objects))

	// initialize index array
	d.idx = make([]ObjId, (d.HeapEnd-d.HeapStart+bucketSize-1)/bucketSize)
	for i := len(d.idx) - 1; i >= 0; i-- {
		d.idx[i] = ObjId(len(d.objects))
	}
	for i := len(d.objects) - 1; i >= 0; i-- {
		// Note: we iterate in reverse order so that the object with
		// the lowest address that intersects a bucket will win.
		lo := (d.objects[i].Addr - d.HeapStart) / bucketSize
		hi := (d.objects[i].Addr + d.objects[i].Ft.Size - 1 - d.HeapStart) / bucketSize
		for j := lo; j <= hi; j++ {
			d.idx[j] = ObjId(i)
		}
	}

	// initialize some maps used for linking
	frames := make(map[frameKey]*StackFrame, len(d.Frames))
	for _, x := range d.Frames {
		frames[frameKey{x.Addr, x.Depth}] = x
	}

	// link up frames in sequence
	for _, f := range d.Frames {
		if f.Depth == 0 {
			continue
		}
		g := frames[frameKey{f.childaddr, f.Depth - 1}]
		g.Parent = f
	}

	// link goroutines to frames & vice versa
	for _, g := range d.Goroutines {
		g.Bos = frames[frameKey{g.bosaddr, 0}]
		if g.Bos == nil {
			log.Fatal("bos missing")
		}
		for f := g.Bos; f != nil; f = f.Parent {
			f.Goroutine = g
		}
		x := d.FindObj(g.ctxtaddr)
		if x != ObjNil {
			g.Ctxt = x
		}
	}
}

func link2(d *Dump) {
	// link stack frames to objects
	for _, f := range d.Frames {
		f.Edges = d.appendFields(f.Edges, f.Data, f.Fields)
	}

	// link data roots
	for _, x := range []*Data{d.Data, d.Bss} {
		x.Edges = d.appendFields(x.Edges, x.Data, x.Fields)
	}

	// link other roots
	for _, r := range d.Otherroots {
		x := d.FindObj(r.toaddr)
		if x != ObjNil {
			r.Edges = append(r.Edges, Edge{x, 0, r.toaddr - d.objects[x].Addr, ""})
		}
	}

	// Add links for finalizers
	// TODO: how do we represent these?
	/*
		for _, f := range d.Finalizers {
			x := d.FindObj(f.obj)
			for _, addr := range []uint64{f.fn, f.fint, f.ot} {
				y := d.FindObj(addr)
				if x != nil && y != nil {
					x.Edges = append(x.Edges, Edge{y, 0, addr - y.Addr, "finalizer", 0})
				}
			}
		}
	*/
	for _, f := range d.QFinal {
		for _, addr := range []uint64{f.obj, f.fn, f.fint, f.ot} {
			x := d.FindObj(addr)
			if x != ObjNil {
				f.Edges = append(f.Edges, Edge{x, 0, addr - d.objects[x].Addr, ""})
			}
		}
	}
}

func nameFallback(d *Dump) {
	// No dwarf info, just name generically
	for _, t := range d.Types {
		for i := range t.Fields {
			t.Fields[i].Name = fmt.Sprintf("field%d", i)
		}
	}
	// name all frame fields
	for _, r := range d.Frames {
		for i := range r.Fields {
			r.Fields[i].Name = fmt.Sprintf("var%d", i)
		}
	}
	// name all globals
	for i := range d.Data.Fields {
		d.Data.Fields[i].Name = fmt.Sprintf("data%d", i)
	}
	for i := range d.Bss.Fields {
		d.Bss.Fields[i].Name = fmt.Sprintf("bss%d", i)
	}
}

// needs to be kept in sync with src/pkg/runtime/chan.h in
// the main Go distribution.
var chanFields = map[uint64]map[uint64]string{
	4: map[uint64]string{
		0:  "len",
		4:  "cap",
		20: "next send index",
		24: "next receive index",
	},
	8: map[uint64]string{
		0:  "len",
		8:  "cap",
		32: "next send index",
		40: "next receive index",
	},
}

func nameFullTypes(d *Dump) {
	for _, ft := range d.FTList {
		for i := 0; i < len(ft.GCSig); i++ {
			switch ft.GCSig[i] {
			case 'S':
				// TODO: byte arrays instead?
				if d.PtrSize == 8 {
					ft.Fields = append(ft.Fields, Field{FieldKindUInt64, uint64(i)*d.PtrSize, fmt.Sprintf("f%d", i), ""})
				} else {
					ft.Fields = append(ft.Fields, Field{FieldKindUInt32, uint64(i)*d.PtrSize, fmt.Sprintf("f%d", i), ""})
				}
			case 'P':
				ft.Fields = append(ft.Fields, Field{FieldKindPtr, uint64(i)*d.PtrSize, fmt.Sprintf("f%d", i), ""})
			case 'I':
				ft.Fields = append(ft.Fields, Field{FieldKindIface, uint64(i)*d.PtrSize, fmt.Sprintf("f%d", i), ""})
				i++
			case 'E':
				ft.Fields = append(ft.Fields, Field{FieldKindEface, uint64(i)*d.PtrSize, fmt.Sprintf("f%d", i), ""})
				i++
			}
		}
		// after gc signature, there may be more data bytes
		for i := uint64(len(ft.GCSig))*d.PtrSize; i < ft.Size; i += d.PtrSize {
			// TODO: byte arrays instead?
			if d.PtrSize == 8 {
				ft.Fields = append(ft.Fields, Field{FieldKindUInt64, i, fmt.Sprintf("%d", i/d.PtrSize), ""})
			} else {
				ft.Fields = append(ft.Fields, Field{FieldKindUInt32, i, fmt.Sprintf("%d", i/d.PtrSize), ""})
			}
			if i >= 1<<16 {
				// ignore >64KB of data
				ft.Fields = append(ft.Fields, Field{FieldKindBytesElided, i, fmt.Sprintf("offset %x", i), ""})
				break
			}
		}
	}
}

type byAddr []object

func (a byAddr) Len() int           { return len(a) }
func (a byAddr) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byAddr) Less(i, j int) bool { return a[i].Addr < a[j].Addr }

func Read(dumpname, execname string) *Dump {
	d := rawRead(dumpname)
	link1(d)
	if execname != "" {
		typePropagate(d, execname)
		//nameWithDwarf(d, execname)
	} else {
		nameFallback(d)
	}
	nameFullTypes(d)
	link2(d)
	return d
}

func readPtr(d *Dump, b []byte) uint64 {
	switch d.PtrSize {
	case 4:
		return uint64(d.Order.Uint32(b))
	case 8:
		return d.Order.Uint64(b)
	default:
		log.Fatal("unsupported PtrSize=%d", d.PtrSize)
		return 0
	}
}
