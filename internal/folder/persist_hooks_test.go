package folder

import (
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestPersistenceFlushContract(t *testing.T) {
	a, err := os.CreateTemp(t.TempDir(), "inode")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.CreateTemp(t.TempDir(), "inode")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var trace []string
	h := persistHooks{probe: PersistenceProbe{Flush: func(kind, phase string, _, _ uint64) { trace = append(trace, phase+":"+kind) }}}
	h.phase("tx", "G1") // nil callback is safe
	if err := h.fsyncOnly(a, "G1"); err != nil {
		t.Fatal(err)
	}
	if err := h.fsyncOnly(b, "G1"); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ops := platformOps()
	ops.fullSync = func(uintptr) error { calls++; return nil }
	identity := func(f *os.File) (uint64, uint64, error) {
		if f == a {
			return 1, 1, nil
		}
		return 2, 2, nil
	}
	if err := h.fullSyncDevicesWithOps([]*os.File{a, a, b, b}, "G1", ops, identity); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("separate devices: got %d flushes", calls)
	}
	if !reflect.DeepEqual(trace, []string{"G1:fsync", "G1:fsync", "G1:FULLFSYNC", "G1:FULLFSYNC"}) {
		t.Fatal(trace)
	}
	boom := errors.New("hardware failure")
	ops.fullSync = func(uintptr) error { return boom }
	if err := h.fullSyncDevicesWithOps([]*os.File{a, b}, "G2", ops, identity); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if h.counters.Fsync != 2 || h.counters.Fullfsync != 3 {
		t.Fatal(h.counters)
	}
	ops.fullSync = nil
	if err := h.fullSyncDevicesWithOps([]*os.File{a}, "G4", ops, identity); err != nil {
		t.Fatal(err)
	}
	if h.counters.Fullfsync != 3 {
		t.Fatal("Linux counted synthetic flush")
	}
}

type shortPersistenceWriter struct{}

func (shortPersistenceWriter) Write(p []byte) (int, error) { return 2, io.ErrShortWrite }

func TestPersistenceProbeNoOpAndFault(t *testing.T) {
	f := &Folder{}
	now := time.Unix(123, 0)
	var observed int
	f.SetPersistenceProbe(PersistenceProbe{ClockNow: func() time.Time { return now }, Write: func(_, _ string, n int, err error) {
		observed = n
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatal(err)
		}
	}})
	if n, err := f.persistHooks.write("log", "path", shortPersistenceWriter{}, []byte("data")); n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("%d %v", n, err)
	}
	if observed != 2 || f.PersistenceStats().Bytes != 2 || f.persistHooks.clockNow() != now {
		t.Fatal("probe mismatch")
	}
	file, err := os.CreateTemp(t.TempDir(), "data")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	f.SetPersistenceProbe(PersistenceProbe{})
	if _, err := f.persistHooks.write("log", file.Name(), file, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := f.persistHooks.durableFile(file, "G4"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != "data" {
		t.Fatalf("%q %v", data, err)
	}
	boom := errors.New("failed fsync")
	f.SetPersistenceProbe(PersistenceProbe{Fault: func(_, _ string, _, _ uint64) error { return boom }})
	before := f.PersistenceStats()
	if err := f.persistHooks.durableFile(file, "G4"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	after := f.PersistenceStats()
	if after.Fsync != before.Fsync+1 || after.Fullfsync != before.Fullfsync {
		t.Fatal("hardware flush after failed fsync")
	}
}
