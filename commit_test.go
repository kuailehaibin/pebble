package pebble

import (
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petermattis/pebble/arenaskl"
	"github.com/petermattis/pebble/record"
)

type testCommitEnv struct {
	logSeqNum     uint64
	visibleSeqNum uint64
	writePos      int64
	writeCount    uint64
	applyBuf      struct {
		sync.Mutex
		buf []uint64
	}
}

func (e *testCommitEnv) env() commitEnv {
	return commitEnv{
		apply: e.apply,
		sync:  e.sync,
		write: e.write,
	}
}

func (e *testCommitEnv) apply(b *Batch, mem *memTable) error {
	e.applyBuf.Lock()
	e.applyBuf.buf = append(e.applyBuf.buf, b.seqNum())
	e.applyBuf.Unlock()
	return nil
}

func (e *testCommitEnv) prepare(b *Batch) (*memTable, error) {
	return nil, nil
}

func (e *testCommitEnv) sync(log *record.LogWriter, pos, n int64) error {
	return nil
}

func (e *testCommitEnv) write(b *Batch) (*memTable, *record.LogWriter, int64, error) {
	n := int64(len(b.data))
	pos := atomic.AddInt64(&e.writePos, n) - n
	atomic.AddUint64(&e.writeCount, 1)
	return nil, nil, pos, nil
}

func TestCommitPipeline(t *testing.T) {
	var e testCommitEnv
	p := newCommitPipeline(e.env(), &e.logSeqNum, &e.visibleSeqNum)

	const n = 10000
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			var b Batch
			_ = b.Set([]byte(fmt.Sprint(i)), nil, nil)
			_ = p.commit(&b, false)
		}(i)
	}
	wg.Wait()

	if s := atomic.LoadUint64(&e.writeCount); n != s {
		t.Fatalf("expected %d written batches, but found %d", n, s)
	}
	if n != len(e.applyBuf.buf) {
		t.Fatalf("expected %d written batches, but found %d",
			n, len(e.applyBuf.buf))
	}
	if s := atomic.LoadUint64(&e.logSeqNum); n != s {
		t.Fatalf("expected %d, but found %d", n, s)
	}
	if s := atomic.LoadUint64(&e.visibleSeqNum); n != s {
		t.Fatalf("expected %d, but found %d", n, s)
	}
}

func BenchmarkCommitPipeline(b *testing.B) {
	for _, parallelism := range []int{1, 2, 4, 8, 16, 32, 64, 128} {
		b.Run(fmt.Sprintf("parallel=%d", parallelism), func(b *testing.B) {
			b.SetParallelism(parallelism)
			var mem *memTable
			wal := record.NewLogWriter(ioutil.Discard)

			nullCommitEnv := commitEnv{
				apply: func(b *Batch, mem *memTable) error {
					return mem.apply(b, b.seqNum())
				},
				sync: func(log *record.LogWriter, pos, n int64) error {
					// return wal.Sync()
					return nil
				},
				write: func(b *Batch) (*memTable, *record.LogWriter, int64, error) {
					for {
						err := arenaskl.ErrArenaFull
						if mem != nil {
							err = mem.prepare(b)
						}
						if err == arenaskl.ErrArenaFull {
							mem = newMemTable(nil)
							continue
						}
						if err != nil {
							return nil, nil, 0, err
						}
						break
					}

					pos, err := wal.WriteRecord(b.data)
					return mem, wal, pos, err
				},
			}
			var logSeqNum, visibleSeqNum uint64
			p := newCommitPipeline(nullCommitEnv, &logSeqNum, &visibleSeqNum)

			const keySize = 8
			b.SetBytes(2 * keySize)
			b.ResetTimer()

			b.RunParallel(func(pb *testing.PB) {
				rng := rand.New(rand.NewSource(time.Now().UnixNano()))
				buf := make([]byte, keySize)

				for pb.Next() {
					batch := newBatch(nil)
					binary.BigEndian.PutUint64(buf, rng.Uint64())
					batch.Set(buf, buf, nil)
					if err := p.commit(batch, true /* sync */); err != nil {
						b.Fatal(err)
					}
					batch.release()
				}
			})
		})
	}
}
