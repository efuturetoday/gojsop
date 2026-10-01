package jsengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
)

const pageSize = 65536

// Snapshot is the state of a VM between two calls: its linear memory, its
// limits and its host functions. A VM restored from it (Snapshot.NewVM)
// starts exactly where the VM stood when the snapshot was taken, with a fresh
// Math.random state. A snapshot is immutable and safe for concurrent
// restores; it is bound to the Engine that took it.
//
// glue.c keeps all state in linear memory, and the only mutable wasm global
// (the stack pointer) is at its base between calls, so the memory is the whole
// VM. Only the pages that differ from a fresh instance are kept (most of the
// 1 MiB stack is zero), so a small policy costs a few hundred KB.
//
// js-execution.R16
type Snapshot struct {
	eng    *Engine
	limits Limits
	host   map[string]HostFunc
	pages  uint32 // memory size in pages; a restore grows to it
	runs   []run  // byte ranges that differ from a fresh instance
	bytes  int
}

// run is a range of whole pages that differ from a fresh instance.
type run struct {
	off  uint32
	data []byte
}

// Bytes is the Go heap the snapshot holds.
func (s *Snapshot) Bytes() int { return s.bytes }

// Limits returns the limits of the VM the snapshot was taken from.
func (s *Snapshot) Limits() Limits { return s.limits }

// baseImage is the linear memory of a fresh instance before its start
// function: data segments only. A restore starts from it.
func (e *Engine) baseImage(ctx context.Context) ([]byte, error) {
	e.baseOnce.Do(func() {
		vm, err := e.instantiate(ctx, Limits{}, false)
		if err != nil {
			e.baseErr = err
			return
		}
		defer vm.Close()
		mem := vm.m.Memory()
		b, _ := mem.Read(0, mem.Size())
		e.base = bytes.Clone(b)
	})
	return e.base, e.baseErr
}

// Snapshot copies the state of vm. Take it only between calls; vm stays
// usable and independent of the snapshot.
func (vm *VM) Snapshot(ctx context.Context) (*Snapshot, error) {
	if !vm.Usable() {
		return nil, ErrClosed
	}
	base, err := vm.eng.baseImage(ctx)
	if err != nil {
		return nil, err
	}
	mem := vm.m.Memory()
	b, ok := mem.Read(0, mem.Size())
	if !ok {
		return nil, errors.New("snapshot: read memory")
	}
	s := &Snapshot{eng: vm.eng, limits: vm.limits, host: vm.host, pages: mem.Size() / pageSize}
	start := -1 // start of the current run of differing pages
	flush := func(end int) {
		if start >= 0 {
			s.runs = append(s.runs, run{off: uint32(start), data: bytes.Clone(b[start:end])})
			s.bytes += end - start
			start = -1
		}
	}
	for off := 0; off < len(b); off += pageSize {
		end := min(off+pageSize, len(b))
		if samePage(b[off:end], base, off) {
			flush(off)
		} else if start < 0 {
			start = off
		}
	}
	flush(len(b))
	return s, nil
}

// samePage reports whether page p at offset off equals the same range of base
// (zero beyond the end of base).
func samePage(p, base []byte, off int) bool {
	var bp []byte
	if off < len(base) {
		bp = base[off:min(off+len(p), len(base))]
	}
	if !bytes.Equal(p[:len(bp)], bp) {
		return false
	}
	for _, c := range p[len(bp):] {
		if c != 0 {
			return false
		}
	}
	return true
}

// NewVM restores a fresh VM from the snapshot: it instantiates the module
// without its start function, grows the memory to the snapshot's size, writes
// the pages that differ, re-reads the stack top and reseeds Math.random. The
// VM has the limits and host functions of the snapshot.
func (s *Snapshot) NewVM(ctx context.Context) (*VM, error) {
	if _, err := s.eng.baseImage(ctx); err != nil {
		return nil, err
	}
	vm, err := s.eng.instantiate(ctx, s.limits, false)
	if err != nil {
		return nil, err
	}
	mem := vm.m.Memory()
	if have := mem.Size() / pageSize; have < s.pages {
		if _, ok := mem.Grow(s.pages - have); !ok {
			vm.Close()
			return nil, ErrOOM
		}
	}
	for _, r := range s.runs {
		if !mem.Write(r.off, r.data) {
			vm.Close()
			return nil, errors.New("restore snapshot: write out of range")
		}
	}
	vm.host, vm.bound = s.host, s.host != nil
	if _, err := vm.invoke(ctx, vm.stackTop); err != nil {
		vm.Close()
		return nil, fmt.Errorf("restore snapshot: %w", err)
	}
	if _, err := vm.invoke(ctx, vm.reseed, rand.Uint64()); err != nil {
		vm.Close()
		return nil, fmt.Errorf("restore snapshot: %w", err)
	}
	return vm, nil
}
