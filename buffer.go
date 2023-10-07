package main

import (
	"unsafe"
)

/* TODO(anton2920): I need to write tests for this... */
type CircularBuffer struct {
	Buf  []byte
	Head int
	Tail int
	M    Mutex
}

const (
	/* See <sys/mman.h>. */
	PROT_NONE  = 0x00
	PROT_READ  = 0x01
	PROT_WRITE = 0x02

	MAP_SHARED  = 0x0001
	MAP_PRIVATE = 0x0002

	MAP_FIXED = 0x0010
	MAP_ANON  = 0x1000
)

func NewCircularBuffer(size int) (CircularBuffer, error) {
	var cb CircularBuffer

	var buffer, rb unsafe.Pointer
	var fd, ret int32

	if size%int(PageSize) != 0 {
		return CircularBuffer{}, NewError("size must be divisible by 4096", -size)
	}

	/* NOTE(anton2920): this is just (*byte)(1). */
	var SHM_ANON = unsafe.String((*byte)(unsafe.Pointer(uintptr(1))), 8)

	if fd = ShmOpen(SHM_ANON, O_RDWR, 0); fd < 0 {
		return CircularBuffer{}, NewError("Failed to open shared memory region: ", int(fd))
	}

	if ret = Ftruncate(fd, int64(size)); ret < 0 {
		return CircularBuffer{}, NewError("Failed to adjust size of shared memory region: ", int(ret))
	}

	if buffer = Mmap(nil, 2*uint64(size), PROT_NONE, MAP_PRIVATE|MAP_ANON, -1, 0); buffer == nil {
		return CircularBuffer{}, NewError("Failed to query address for future mappings: ", int(uintptr(buffer)))
	}

	if rb = Mmap(buffer, uint64(size), PROT_READ|PROT_WRITE, MAP_SHARED|MAP_FIXED, fd, 0); rb == nil {
		return CircularBuffer{}, NewError("Failed to map first view of buffer: ", int(uintptr(rb)))
	}
	if rb = Mmap(unsafe.Add(buffer, size), uint64(size), PROT_READ|PROT_WRITE, MAP_SHARED|MAP_FIXED, fd, 0); rb == nil {
		return CircularBuffer{}, NewError("Failed to map second view of buffer: ", int(uintptr(rb)))
	}

	cb.Buf = unsafe.Slice((*byte)(buffer), 2*size)

	/* NOTE(anton2920): sanity checks. */
	cb.Buf[0] = '\x00'
	cb.Buf[size-1] = '\x00'
	cb.Buf[size] = '\x00'
	cb.Buf[2*size-1] = '\x00'

	return cb, nil
}

/* Here socket is producer and application is consumer. */
func (cb *CircularBuffer) ReadFrom(fd int32) int {
	cb.M.Lock()
	n := int(Read(fd, cb.remainingSlice()))
	if n > 0 {
		cb.produce(n)
	}
	cb.M.Unlock()
	return n
}

func (cb *CircularBuffer) consume(n int) {
	cb.Head += n
	if cb.Head > len(cb.Buf)/2 {
		cb.Head -= len(cb.Buf) / 2
		cb.Tail -= len(cb.Buf) / 2
	}
}

func (cb *CircularBuffer) Consume(n int) {
	cb.M.Lock()
	cb.consume(n)
	cb.M.Unlock()
}

func (cb *CircularBuffer) unconsumedLen() int {
	return max(cb.Tail-cb.Head, 0)
}

func (cb *CircularBuffer) UnconsumedLen() int {
	cb.M.Lock()
	defer cb.M.Unlock()
	return cb.unconsumedLen()
}

func (cb *CircularBuffer) unconsumedSlice() []byte {
	return unsafe.Slice(&cb.Buf[cb.Head], cb.unconsumedLen())
}

func (cb *CircularBuffer) UnconsumedSlice() []byte {
	cb.M.Lock()
	defer cb.M.Unlock()
	return cb.unconsumedSlice()
}

func (cb *CircularBuffer) UnconsumedString() string {
	cb.M.Lock()
	defer cb.M.Unlock()
	return unsafe.String(&cb.Buf[cb.Head], cb.unconsumedLen())
}

/* Here application is producer and socket is consumer. */
func (cb *CircularBuffer) WriteTo(fd int32) int {
	cb.M.Lock()
	n := int(Write(fd, cb.unconsumedSlice()))
	if n > 0 {
		cb.consume(n)
	}
	cb.M.Unlock()
	return n
}

func (cb *CircularBuffer) produce(n int) {
	cb.Tail += n
}

func (cb *CircularBuffer) Produce(n int) {
	cb.M.Lock()
	cb.produce(n)
	cb.M.Unlock()
}

func (cb *CircularBuffer) RemainingSpace() int {
	cb.M.Lock()
	defer cb.M.Unlock()
	return (len(cb.Buf) / 2) - (cb.Tail - cb.Head)
}

/* remainingSlice returns slice of remaining free space in buffer. */
func (cb *CircularBuffer) remainingSlice() []byte {
	return cb.Buf[cb.Tail : cb.Head+len(cb.Buf)/2]
}

func (cb *CircularBuffer) RemainingSlice() []byte {
	cb.M.Lock()
	defer cb.M.Unlock()
	return cb.remainingSlice()
}
