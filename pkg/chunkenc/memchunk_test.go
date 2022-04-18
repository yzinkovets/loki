package chunkenc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/pkg/chunkenc/testdata"
	"github.com/grafana/loki/pkg/iter"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql/log"
	"github.com/grafana/loki/pkg/logql/syntax"
	"github.com/grafana/loki/pkg/logqlmodel/stats"
	"github.com/grafana/loki/pkg/storage/chunk"
)

var testEncoding = []Encoding{
	EncNone,
	EncGZIP,
	EncLZ4_64k,
	EncLZ4_256k,
	EncLZ4_1M,
	EncLZ4_4M,
	EncSnappy,
	EncFlate,
	EncZstd,
}

var (
	testBlockSize  = 256 * 1024
	testTargetSize = 1500 * 1024
	testBlockSizes = []int{64 * 1024, 256 * 1024, 512 * 1024}
	countExtractor = func() log.StreamSampleExtractor {
		ex, err := log.NewLineSampleExtractor(log.CountExtractor, nil, nil, false, false)
		if err != nil {
			panic(err)
		}
		return ex.ForStream(labels.Labels{})
	}()
)

const DefaultHeadBlockFmt = OrderedHeadBlockFmt

func TestBlocksInclusive(t *testing.T) {
	chk := NewMemChunk(EncNone, DefaultHeadBlockFmt, testBlockSize, testTargetSize)
	err := chk.Append(logprotoEntry(1, "1"))
	require.Nil(t, err)
	err = chk.cut()
	require.Nil(t, err)

	blocks := chk.Blocks(time.Unix(0, 1), time.Unix(0, 1))
	require.Equal(t, 1, len(blocks))
	require.Equal(t, 1, blocks[0].Entries())
}

func TestBlock(t *testing.T) {
	for _, enc := range testEncoding {
		t.Run(enc.String(), func(t *testing.T) {
			t.Parallel()

			chk := NewMemChunk(enc, DefaultHeadBlockFmt, testBlockSize, testTargetSize)
			cases := []struct {
				ts  int64
				str string
				cut bool
			}{
				{
					ts:  1,
					str: "hello, world!",
				},
				{
					ts:  2,
					str: "hello, world2!",
				},
				{
					ts:  3,
					str: "hello, world3!",
				},
				{
					ts:  4,
					str: "hello, world4!",
				},
				{
					ts:  5,
					str: "hello, world5!",
				},
				{
					ts:  6,
					str: "hello, world6!",
					cut: true,
				},
				{
					ts:  7,
					str: "hello, world7!",
				},
				{
					ts:  8,
					str: "hello, worl\nd8!",
				},
				{
					ts:  8,
					str: "hello, world 8, 2!",
				},
				{
					ts:  8,
					str: "hello, world 8, 3!",
				},
				{
					ts:  9,
					str: "",
				},
			}

			for _, c := range cases {
				require.NoError(t, chk.Append(logprotoEntry(c.ts, c.str)))
				if c.cut {
					require.NoError(t, chk.cut())
				}
			}

			it, err := chk.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), logproto.FORWARD, noopStreamPipeline)
			require.NoError(t, err)

			idx := 0
			for it.Next() {
				e := it.Entry()
				require.Equal(t, cases[idx].ts, e.Timestamp.UnixNano())
				require.Equal(t, cases[idx].str, e.Line)
				idx++
			}

			require.NoError(t, it.Error())
			require.NoError(t, it.Close())
			require.Equal(t, len(cases), idx)

			sampleIt := chk.SampleIterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), countExtractor)
			idx = 0
			for sampleIt.Next() {
				s := sampleIt.Sample()
				require.Equal(t, cases[idx].ts, s.Timestamp)
				require.Equal(t, 1., s.Value)
				require.NotEmpty(t, s.Hash)
				idx++
			}

			require.NoError(t, sampleIt.Error())
			require.NoError(t, sampleIt.Close())
			require.Equal(t, len(cases), idx)

			t.Run("bounded-iteration", func(t *testing.T) {
				it, err := chk.Iterator(context.Background(), time.Unix(0, 3), time.Unix(0, 7), logproto.FORWARD, noopStreamPipeline)
				require.NoError(t, err)

				idx := 2
				for it.Next() {
					e := it.Entry()
					require.Equal(t, cases[idx].ts, e.Timestamp.UnixNano())
					require.Equal(t, cases[idx].str, e.Line)
					idx++
				}
				require.NoError(t, it.Error())
				require.Equal(t, 6, idx)
			})
		})
	}
}

func TestReadFormatV1(t *testing.T) {
	t.Parallel()

	c := NewMemChunk(EncGZIP, DefaultHeadBlockFmt, testBlockSize, testTargetSize)
	fillChunk(c)
	// overrides default v2 format
	c.format = chunkFormatV1

	b, err := c.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	r, err := NewByteChunk(b, testBlockSize, testTargetSize)
	if err != nil {
		t.Fatal(err)
	}

	it, err := r.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), logproto.FORWARD, noopStreamPipeline)
	if err != nil {
		t.Fatal(err)
	}

	i := int64(0)
	for it.Next() {
		require.Equal(t, i, it.Entry().Timestamp.UnixNano())
		require.Equal(t, testdata.LogString(i), it.Entry().Line)

		i++
	}
}

// Test all encodings by populating a memchunk, serializing it,
// re-loading with NewByteChunk, serializing it again, and re-loading into via NewByteChunk once more.
// This tests the integrity of transfer between the following:
// 1) memory populated chunks <-> []byte loaded chunks
// 2) []byte loaded chunks <-> []byte loaded chunks
func TestRoundtripV2(t *testing.T) {
	for _, f := range HeadBlockFmts {
		for _, enc := range testEncoding {
			for _, version := range []byte{chunkFormatV2, chunkFormatV3} {
				t.Run(enc.String(), func(t *testing.T) {
					t.Parallel()

					c := NewMemChunk(enc, f, testBlockSize, testTargetSize)
					c.format = version
					populated := fillChunk(c)

					assertLines := func(c *MemChunk) {
						require.Equal(t, enc, c.Encoding())
						it, err := c.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), logproto.FORWARD, noopStreamPipeline)
						if err != nil {
							t.Fatal(err)
						}

						i := int64(0)
						var data int64
						for it.Next() {
							require.Equal(t, i, it.Entry().Timestamp.UnixNano())
							require.Equal(t, testdata.LogString(i), it.Entry().Line)

							data += int64(len(it.Entry().Line))
							i++
						}
						require.Equal(t, populated, data)
					}

					assertLines(c)

					// test MemChunk -> NewByteChunk loading
					b, err := c.Bytes()
					if err != nil {
						t.Fatal(err)
					}

					r, err := NewByteChunk(b, testBlockSize, testTargetSize)
					if err != nil {
						t.Fatal(err)
					}
					assertLines(r)

					// test NewByteChunk -> NewByteChunk loading
					rOut, err := r.Bytes()
					require.Nil(t, err)

					loaded, err := NewByteChunk(rOut, testBlockSize, testTargetSize)
					require.Nil(t, err)

					assertLines(loaded)
				})
			}
		}
	}
}

func TestRoundtripV3(t *testing.T) {
	for _, f := range HeadBlockFmts {
		for _, enc := range testEncoding {
			t.Run(fmt.Sprintf("%v-%v", f, enc), func(t *testing.T) {
				t.Parallel()

				c := NewMemChunk(enc, f, testBlockSize, testTargetSize)
				c.format = chunkFormatV3
				_ = fillChunk(c)

				b, err := c.Bytes()
				require.Nil(t, err)
				r, err := NewByteChunk(b, testBlockSize, testTargetSize)
				require.Nil(t, err)

				b2, err := r.Bytes()
				require.Nil(t, err)
				require.Equal(t, b, b2)
			})
		}
	}
}

func TestSerialization(t *testing.T) {
	for _, f := range HeadBlockFmts {
		for _, enc := range testEncoding {
			t.Run(enc.String(), func(t *testing.T) {
				t.Parallel()

				chk := NewMemChunk(enc, f, testBlockSize, testTargetSize)

				numSamples := 50000

				for i := 0; i < numSamples; i++ {
					require.NoError(t, chk.Append(logprotoEntry(int64(i), strconv.Itoa(i))))
				}
				require.NoError(t, chk.Close())

				byt, err := chk.Bytes()
				require.NoError(t, err)

				bc, err := NewByteChunk(byt, testBlockSize, testTargetSize)
				require.NoError(t, err)

				it, err := bc.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), logproto.FORWARD, noopStreamPipeline)
				require.NoError(t, err)
				for i := 0; i < numSamples; i++ {
					require.True(t, it.Next())

					e := it.Entry()
					require.Equal(t, int64(i), e.Timestamp.UnixNano())
					require.Equal(t, strconv.Itoa(i), e.Line)
				}
				require.NoError(t, it.Error())

				sampleIt := bc.SampleIterator(context.Background(), time.Unix(0, 0), time.Unix(0, math.MaxInt64), countExtractor)
				for i := 0; i < numSamples; i++ {
					require.True(t, sampleIt.Next(), i)

					s := sampleIt.Sample()
					require.Equal(t, int64(i), s.Timestamp)
					require.Equal(t, 1., s.Value)
				}
				require.NoError(t, sampleIt.Error())

				byt2, err := chk.Bytes()
				require.NoError(t, err)

				require.True(t, bytes.Equal(byt, byt2))
			})
		}
	}
}

func TestChunkFilling(t *testing.T) {
	for _, f := range HeadBlockFmts {
		for _, enc := range testEncoding {
			t.Run(enc.String(), func(t *testing.T) {
				t.Parallel()

				chk := NewMemChunk(enc, f, testBlockSize, 0)
				chk.blockSize = 1024

				// We should be able to append only 10KB of logs.
				maxBytes := chk.blockSize * blocksPerChunk
				lineSize := 512
				lines := maxBytes / lineSize

				logLine := string(make([]byte, lineSize))
				entry := &logproto.Entry{
					Timestamp: time.Unix(0, 0),
					Line:      logLine,
				}

				i := int64(0)
				for ; chk.SpaceFor(entry) && i < 30; i++ {
					entry.Timestamp = time.Unix(0, i)
					require.NoError(t, chk.Append(entry))
				}

				require.Equal(t, int64(lines), i)

				it, err := chk.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, 100), logproto.FORWARD, noopStreamPipeline)
				require.NoError(t, err)
				i = 0
				for it.Next() {
					entry := it.Entry()
					require.Equal(t, i, entry.Timestamp.UnixNano())
					i++
				}

				require.Equal(t, int64(lines), i)
			})
		}
	}
}

func TestGZIPChunkTargetSize(t *testing.T) {
	t.Parallel()

	chk := NewMemChunk(EncGZIP, DefaultHeadBlockFmt, testBlockSize, testTargetSize)

	lineSize := 512
	entry := &logproto.Entry{
		Timestamp: time.Unix(0, 0),
		Line:      "",
	}

	// Use a random number to generate random log data, otherwise the gzip compression is way too good
	// and the following loop has to run waaayyyyy to many times
	// Using the same seed should guarantee the same random numbers and same test data.
	r := rand.New(rand.NewSource(99))

	i := int64(0)

	for ; chk.SpaceFor(entry) && i < 5000; i++ {
		logLine := make([]byte, lineSize)
		for j := range logLine {
			logLine[j] = byte(r.Int())
		}
		entry = &logproto.Entry{
			Timestamp: time.Unix(0, 0),
			Line:      string(logLine),
		}
		entry.Timestamp = time.Unix(0, i)
		require.NoError(t, chk.Append(entry))
	}

	// 5000 is a limit ot make sure the test doesn't run away, we shouldn't need this many log lines to make 1MB chunk
	require.NotEqual(t, 5000, i)

	require.NoError(t, chk.Close())

	require.Equal(t, 0, chk.head.UncompressedSize())

	// Even though the seed is static above and results should be deterministic,
	// we will allow +/- 10% variance
	minSize := int(float64(testTargetSize) * 0.9)
	maxSize := int(float64(testTargetSize) * 1.1)
	require.Greater(t, chk.CompressedSize(), minSize)
	require.Less(t, chk.CompressedSize(), maxSize)

	// Also verify our utilization is close to 1.0
	ut := chk.Utilization()
	require.Greater(t, ut, 0.99)
	require.Less(t, ut, 1.01)
}

func TestMemChunk_AppendOutOfOrder(t *testing.T) {
	t.Parallel()

	type tester func(t *testing.T, chk *MemChunk)

	tests := map[string]tester{
		"append out of order in the same block": func(t *testing.T, chk *MemChunk) {
			assert.NoError(t, chk.Append(logprotoEntry(5, "test")))
			assert.NoError(t, chk.Append(logprotoEntry(6, "test")))

			if chk.headFmt == OrderedHeadBlockFmt {
				assert.EqualError(t, chk.Append(logprotoEntry(1, "test")), ErrOutOfOrder.Error())
			} else {
				assert.NoError(t, chk.Append(logprotoEntry(1, "test")))
			}
		},
		"append out of order in a new block right after cutting the previous one": func(t *testing.T, chk *MemChunk) {
			assert.NoError(t, chk.Append(logprotoEntry(5, "test")))
			assert.NoError(t, chk.Append(logprotoEntry(6, "test")))
			assert.NoError(t, chk.cut())

			if chk.headFmt == OrderedHeadBlockFmt {
				assert.EqualError(t, chk.Append(logprotoEntry(1, "test")), ErrOutOfOrder.Error())
			} else {
				assert.NoError(t, chk.Append(logprotoEntry(1, "test")))
			}
		},
		"append out of order in a new block after multiple cuts": func(t *testing.T, chk *MemChunk) {
			assert.NoError(t, chk.Append(logprotoEntry(5, "test")))
			assert.NoError(t, chk.cut())

			assert.NoError(t, chk.Append(logprotoEntry(6, "test")))
			assert.NoError(t, chk.cut())

			if chk.headFmt == OrderedHeadBlockFmt {
				assert.EqualError(t, chk.Append(logprotoEntry(1, "test")), ErrOutOfOrder.Error())
			} else {
				assert.NoError(t, chk.Append(logprotoEntry(1, "test")))
			}
		},
	}

	for _, f := range HeadBlockFmts {
		for testName, tester := range tests {
			tester := tester

			t.Run(testName, func(t *testing.T) {
				t.Parallel()

				tester(t, NewMemChunk(EncGZIP, f, testBlockSize, testTargetSize))
			})
		}
	}
}

func TestChunkSize(t *testing.T) {
	type res struct {
		name           string
		size           uint64
		compressedSize uint64
		ratio          float64
	}
	var result []res
	for _, bs := range testBlockSizes {
		for _, f := range HeadBlockFmts {
			for _, enc := range testEncoding {
				name := fmt.Sprintf("%s_%s", enc.String(), humanize.Bytes(uint64(bs)))
				t.Run(name, func(t *testing.T) {
					c := NewMemChunk(enc, f, bs, testTargetSize)
					inserted := fillChunk(c)
					b, err := c.Bytes()
					if err != nil {
						t.Fatal(err)
					}
					result = append(result, res{
						name:           name,
						size:           uint64(inserted),
						compressedSize: uint64(len(b)),
						ratio:          float64(inserted) / float64(len(b)),
					})
				})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ratio > result[j].ratio
	})
	fmt.Printf("%s\t%s\t%s\t%s\n", "name", "uncompressed", "compressed", "ratio")
	for _, r := range result {
		fmt.Printf("%s\t%s\t%s\t%f\n", r.name, humanize.Bytes(r.size), humanize.Bytes(r.compressedSize), r.ratio)
	}
}

func TestChunkStats(t *testing.T) {
	c := NewMemChunk(EncSnappy, DefaultHeadBlockFmt, testBlockSize, 0)
	first := time.Now()
	entry := &logproto.Entry{
		Timestamp: first,
		Line:      `ts=2020-03-16T13:58:33.459Z caller=dedupe.go:112 component=remote level=debug remote_name=3ea44a url=https:/blan.goo.net/api/prom/push msg=QueueManager.updateShardsLoop lowerBound=45.5 desiredShards=56.724401194003136 upperBound=84.5`,
	}
	inserted := 0
	// fill the chunk with known data size.
	for {
		if !c.SpaceFor(entry) {
			break
		}
		if err := c.Append(entry); err != nil {
			t.Fatal(err)
		}
		inserted++
		entry.Timestamp = entry.Timestamp.Add(time.Nanosecond)
	}
	expectedSize := (inserted * len(entry.Line)) + (inserted * 2 * binary.MaxVarintLen64)
	statsCtx, ctx := stats.NewContext(context.Background())

	it, err := c.Iterator(ctx, first.Add(-time.Hour), entry.Timestamp.Add(time.Hour), logproto.BACKWARD, noopStreamPipeline)
	if err != nil {
		t.Fatal(err)
	}
	for it.Next() {
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	// test on a chunk filling up
	s := statsCtx.Result(time.Since(first), 0)
	require.Equal(t, int64(expectedSize), s.Summary.TotalBytesProcessed)
	require.Equal(t, int64(inserted), s.Summary.TotalLinesProcessed)

	require.Equal(t, int64(expectedSize), s.TotalDecompressedBytes())
	require.Equal(t, int64(inserted), s.TotalDecompressedLines())

	b, err := c.Bytes()
	if err != nil {
		t.Fatal(err)
	}

	// test on a new chunk.
	cb, err := NewByteChunk(b, testBlockSize, testTargetSize)
	if err != nil {
		t.Fatal(err)
	}
	statsCtx, ctx = stats.NewContext(context.Background())
	it, err = cb.Iterator(ctx, first.Add(-time.Hour), entry.Timestamp.Add(time.Hour), logproto.BACKWARD, noopStreamPipeline)
	if err != nil {
		t.Fatal(err)
	}
	for it.Next() {
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	s = statsCtx.Result(time.Since(first), 0)
	require.Equal(t, int64(expectedSize), s.Summary.TotalBytesProcessed)
	require.Equal(t, int64(inserted), s.Summary.TotalLinesProcessed)

	require.Equal(t, int64(expectedSize), s.TotalDecompressedBytes())
	require.Equal(t, int64(inserted), s.TotalDecompressedLines())
}

func TestIteratorClose(t *testing.T) {
	for _, f := range HeadBlockFmts {
		for _, enc := range testEncoding {
			t.Run(enc.String(), func(t *testing.T) {
				for _, test := range []func(iter iter.EntryIterator, t *testing.T){
					func(iter iter.EntryIterator, t *testing.T) {
						// close without iterating
						if err := iter.Close(); err != nil {
							t.Fatal(err)
						}
					},
					func(iter iter.EntryIterator, t *testing.T) {
						// close after iterating
						for iter.Next() {
							_ = iter.Entry()
						}
						if err := iter.Close(); err != nil {
							t.Fatal(err)
						}
					},
					func(iter iter.EntryIterator, t *testing.T) {
						// close after a single iteration
						iter.Next()
						_ = iter.Entry()
						if err := iter.Close(); err != nil {
							t.Fatal(err)
						}
					},
				} {
					c := NewMemChunk(enc, f, testBlockSize, testTargetSize)
					inserted := fillChunk(c)
					iter, err := c.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, inserted), logproto.BACKWARD, noopStreamPipeline)
					if err != nil {
						t.Fatal(err)
					}
					test(iter, t)
				}
			})
		}
	}
}

var result []Chunk

func BenchmarkWrite(b *testing.B) {
	chunks := []Chunk{}

	entry := &logproto.Entry{
		Timestamp: time.Unix(0, 0),
		Line:      testdata.LogString(0),
	}
	i := int64(0)

	for _, f := range HeadBlockFmts {
		for _, enc := range testEncoding {
			b.Run(fmt.Sprintf("%v-%v", f, enc), func(b *testing.B) {
				for n := 0; n < b.N; n++ {
					c := NewMemChunk(enc, f, testBlockSize, testTargetSize)
					// adds until full so we trigger cut which serialize using gzip
					for c.SpaceFor(entry) {
						_ = c.Append(entry)
						entry.Timestamp = time.Unix(0, i)
						entry.Line = testdata.LogString(i)
						i++
					}
					chunks = append(chunks, c)
				}
				result = chunks
			})
		}
	}
}

type nomatchPipeline struct{}

func (nomatchPipeline) BaseLabels() log.LabelsResult { return log.EmptyLabelsResult }
func (nomatchPipeline) Process(_ int64, line []byte) ([]byte, log.LabelsResult, bool) {
	return line, nil, false
}
func (nomatchPipeline) ProcessString(_ int64, line string) (string, log.LabelsResult, bool) {
	return line, nil, false
}

func BenchmarkRead(b *testing.B) {
	type res struct {
		name  string
		speed float64
	}
	result := []res{}
	for _, bs := range testBlockSizes {
		for _, enc := range testEncoding {
			name := fmt.Sprintf("%s_%s", enc.String(), humanize.Bytes(uint64(bs)))
			b.Run(name, func(b *testing.B) {
				chunks, size := generateData(enc, 5, bs, testTargetSize)
				b.ResetTimer()
				bytesRead := uint64(0)
				now := time.Now()
				for n := 0; n < b.N; n++ {
					for _, c := range chunks {
						// use forward iterator for benchmark -- backward iterator does extra allocations by keeping entries in memory
						iterator, err := c.Iterator(context.Background(), time.Unix(0, 0), time.Now(), logproto.FORWARD, nomatchPipeline{})
						if err != nil {
							panic(err)
						}
						for iterator.Next() {
							_ = iterator.Entry()
						}
						if err := iterator.Close(); err != nil {
							b.Fatal(err)
						}
					}
					bytesRead += size
				}
				result = append(result, res{
					name:  name,
					speed: float64(bytesRead) / time.Since(now).Seconds(),
				})
			})

			name = fmt.Sprintf("sample_%s_%s", enc.String(), humanize.Bytes(uint64(bs)))

			b.Run(name, func(b *testing.B) {
				chunks, size := generateData(enc, 5, bs, testTargetSize)
				b.ResetTimer()
				bytesRead := uint64(0)
				now := time.Now()
				for n := 0; n < b.N; n++ {
					for _, c := range chunks {
						iterator := c.SampleIterator(context.Background(), time.Unix(0, 0), time.Now(), countExtractor)
						for iterator.Next() {
							_ = iterator.Sample()
						}
						if err := iterator.Close(); err != nil {
							b.Fatal(err)
						}
					}
					bytesRead += size
				}
				result = append(result, res{
					name:  name,
					speed: float64(bytesRead) / time.Since(now).Seconds(),
				})
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].speed > result[j].speed
	})
	for _, r := range result {
		fmt.Printf("%s: %.2f MB/s\n", r.name, r.speed/1024/1024)
	}
}

func BenchmarkBackwardIterator(b *testing.B) {
	for _, bs := range testBlockSizes {
		b.Run(humanize.Bytes(uint64(bs)), func(b *testing.B) {
			b.ReportAllocs()
			c := NewMemChunk(EncSnappy, DefaultHeadBlockFmt, bs, testTargetSize)
			_ = fillChunk(c)
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				iterator, err := c.Iterator(context.Background(), time.Unix(0, 0), time.Now(), logproto.BACKWARD, noopStreamPipeline)
				if err != nil {
					panic(err)
				}
				for iterator.Next() {
					_ = iterator.Entry()
				}
				if err := iterator.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestGenerateDataSize(t *testing.T) {
	for _, enc := range testEncoding {
		t.Run(enc.String(), func(t *testing.T) {
			chunks, size := generateData(enc, 50, testBlockSize, testTargetSize)

			bytesRead := uint64(0)
			for _, c := range chunks {
				// use forward iterator for benchmark -- backward iterator does extra allocations by keeping entries in memory
				iterator, err := c.Iterator(context.TODO(), time.Unix(0, 0), time.Now(), logproto.FORWARD, noopStreamPipeline)
				if err != nil {
					panic(err)
				}
				for iterator.Next() {
					e := iterator.Entry()
					bytesRead += uint64(len(e.Line))
				}
				if err := iterator.Close(); err != nil {
					t.Fatal(err)
				}
			}

			require.Equal(t, size, bytesRead)
		})
	}
}

func BenchmarkHeadBlockIterator(b *testing.B) {
	for _, j := range []int{100000, 50000, 15000, 10000} {
		b.Run(fmt.Sprintf("Size %d", j), func(b *testing.B) {
			h := headBlock{}

			for i := 0; i < j; i++ {
				if err := h.Append(int64(i), "this is the append string"); err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()

			for n := 0; n < b.N; n++ {
				iter := h.Iterator(context.Background(), logproto.BACKWARD, 0, math.MaxInt64, noopStreamPipeline)

				for iter.Next() {
					_ = iter.Entry()
				}
			}
		})
	}
}

func BenchmarkHeadBlockSampleIterator(b *testing.B) {
	for _, j := range []int{20000, 10000, 8000, 5000} {
		b.Run(fmt.Sprintf("Size %d", j), func(b *testing.B) {
			h := headBlock{}

			for i := 0; i < j; i++ {
				if err := h.Append(int64(i), "this is the append string"); err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()

			for n := 0; n < b.N; n++ {
				iter := h.SampleIterator(context.Background(), 0, math.MaxInt64, countExtractor)

				for iter.Next() {
					_ = iter.Sample()
				}
				iter.Close()
			}
		})
	}
}

func TestMemChunk_IteratorBounds(t *testing.T) {
	createChunk := func() *MemChunk {
		t.Helper()
		c := NewMemChunk(EncNone, DefaultHeadBlockFmt, 1e6, 1e6)

		if err := c.Append(&logproto.Entry{
			Timestamp: time.Unix(0, 1),
			Line:      "1",
		}); err != nil {
			t.Fatal(err)
		}
		if err := c.Append(&logproto.Entry{
			Timestamp: time.Unix(0, 2),
			Line:      "2",
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}

	for _, tt := range []struct {
		mint, maxt time.Time
		direction  logproto.Direction
		expect     []bool // array of expected values for next call in sequence
	}{
		{time.Unix(0, 0), time.Unix(0, 1), logproto.FORWARD, []bool{false}},
		{time.Unix(0, 1), time.Unix(0, 1), logproto.FORWARD, []bool{true, false}},
		{time.Unix(0, 1), time.Unix(0, 2), logproto.FORWARD, []bool{true, false}},
		{time.Unix(0, 2), time.Unix(0, 2), logproto.FORWARD, []bool{true, false}},
		{time.Unix(0, 1), time.Unix(0, 3), logproto.FORWARD, []bool{true, true, false}},
		{time.Unix(0, 2), time.Unix(0, 3), logproto.FORWARD, []bool{true, false}},
		{time.Unix(0, 3), time.Unix(0, 3), logproto.FORWARD, []bool{false}},

		{time.Unix(0, 0), time.Unix(0, 1), logproto.BACKWARD, []bool{false}},
		{time.Unix(0, 1), time.Unix(0, 1), logproto.BACKWARD, []bool{true, false}},
		{time.Unix(0, 1), time.Unix(0, 2), logproto.BACKWARD, []bool{true, false}},
		{time.Unix(0, 2), time.Unix(0, 2), logproto.BACKWARD, []bool{true, false}},
		{time.Unix(0, 1), time.Unix(0, 3), logproto.BACKWARD, []bool{true, true, false}},
		{time.Unix(0, 2), time.Unix(0, 3), logproto.BACKWARD, []bool{true, false}},
		{time.Unix(0, 3), time.Unix(0, 3), logproto.BACKWARD, []bool{false}},
	} {
		t.Run(
			fmt.Sprintf("mint:%d,maxt:%d,direction:%s", tt.mint.UnixNano(), tt.maxt.UnixNano(), tt.direction),
			func(t *testing.T) {
				t.Parallel()

				tt := tt
				c := createChunk()

				// testing headchunk
				it, err := c.Iterator(context.Background(), tt.mint, tt.maxt, tt.direction, noopStreamPipeline)
				require.NoError(t, err)
				for i := range tt.expect {
					require.Equal(t, tt.expect[i], it.Next())
				}
				require.NoError(t, it.Close())

				// testing chunk blocks
				require.NoError(t, c.cut())
				it, err = c.Iterator(context.Background(), tt.mint, tt.maxt, tt.direction, noopStreamPipeline)
				require.NoError(t, err)
				for i := range tt.expect {
					require.Equal(t, tt.expect[i], it.Next())
				}
				require.NoError(t, it.Close())
			})
	}
}

func TestMemchunkLongLine(t *testing.T) {
	for _, enc := range testEncoding {
		t.Run(enc.String(), func(t *testing.T) {
			t.Parallel()

			c := NewMemChunk(enc, DefaultHeadBlockFmt, testBlockSize, testTargetSize)
			for i := 1; i <= 10; i++ {
				require.NoError(t, c.Append(&logproto.Entry{Timestamp: time.Unix(0, int64(i)), Line: strings.Repeat("e", 200000)}))
			}
			it, err := c.Iterator(context.Background(), time.Unix(0, 0), time.Unix(0, 100), logproto.FORWARD, noopStreamPipeline)
			require.NoError(t, err)
			for i := 1; i <= 10; i++ {
				require.True(t, it.Next())
			}
			require.False(t, it.Next())
		})
	}
}

// Ensure passing a reusable []byte doesn't affect output
func TestBytesWith(t *testing.T) {
	t.Parallel()

	exp, err := NewMemChunk(EncNone, DefaultHeadBlockFmt, testBlockSize, testTargetSize).BytesWith(nil)
	require.Nil(t, err)
	out, err := NewMemChunk(EncNone, DefaultHeadBlockFmt, testBlockSize, testTargetSize).BytesWith([]byte{1, 2, 3})
	require.Nil(t, err)

	require.Equal(t, exp, out)
}

func TestCheckpointEncoding(t *testing.T) {
	t.Parallel()

	blockSize, targetSize := 256*1024, 1500*1024
	for _, f := range HeadBlockFmts {
		t.Run(f.String(), func(t *testing.T) {
			c := NewMemChunk(EncSnappy, f, blockSize, targetSize)

			// add a few entries
			for i := 0; i < 5; i++ {
				entry := &logproto.Entry{
					Timestamp: time.Unix(int64(i), 0),
					Line:      fmt.Sprintf("hi there - %d", i),
				}
				require.Equal(t, true, c.SpaceFor(entry))
				require.Nil(t, c.Append(entry))
			}

			// cut it
			require.Nil(t, c.cut())

			// add a few more to head
			for i := 5; i < 10; i++ {
				entry := &logproto.Entry{
					Timestamp: time.Unix(int64(i), 0),
					Line:      fmt.Sprintf("hi there - %d", i),
				}
				require.Equal(t, true, c.SpaceFor(entry))
				require.Nil(t, c.Append(entry))
			}

			// ensure new blocks are not cut
			require.Equal(t, 1, len(c.blocks))

			var chk, head bytes.Buffer
			err := c.SerializeForCheckpointTo(&chk, &head)
			require.Nil(t, err)

			cpy, err := MemchunkFromCheckpoint(chk.Bytes(), head.Bytes(), f, blockSize, targetSize)
			require.Nil(t, err)

			require.Equal(t, c, cpy)
		})
	}
}

var (
	streams = []logproto.Stream{}
	series  = []logproto.Series{}
)

func BenchmarkBufferedIteratorLabels(b *testing.B) {
	for _, f := range HeadBlockFmts {
		b.Run(f.String(), func(b *testing.B) {
			c := NewMemChunk(EncSnappy, f, testBlockSize, testTargetSize)
			_ = fillChunk(c)

			labelsSet := []labels.Labels{
				{
					{Name: "cluster", Value: "us-central1"},
					{Name: "stream", Value: "stdout"},
					{Name: "filename", Value: "/var/log/pods/loki-prod_query-frontend-6894f97b98-89q2n_eac98024-f60f-44af-a46f-d099bc99d1e7/query-frontend/0.log"},
					{Name: "namespace", Value: "loki-dev"},
					{Name: "job", Value: "loki-prod/query-frontend"},
					{Name: "container", Value: "query-frontend"},
					{Name: "pod", Value: "query-frontend-6894f97b98-89q2n"},
				},
				{
					{Name: "cluster", Value: "us-central2"},
					{Name: "stream", Value: "stderr"},
					{Name: "filename", Value: "/var/log/pods/loki-prod_querier-6894f97b98-89q2n_eac98024-f60f-44af-a46f-d099bc99d1e7/query-frontend/0.log"},
					{Name: "namespace", Value: "loki-dev"},
					{Name: "job", Value: "loki-prod/querier"},
					{Name: "container", Value: "querier"},
					{Name: "pod", Value: "querier-6894f97b98-89q2n"},
				},
			}
			for _, test := range []string{
				`{app="foo"}`,
				`{app="foo"} != "foo"`,
				`{app="foo"} != "foo" | logfmt `,
				`{app="foo"} != "foo" | logfmt | duration > 10ms`,
				`{app="foo"} != "foo" | logfmt | duration > 10ms and component="tsdb"`,
			} {
				b.Run(test, func(b *testing.B) {
					b.ReportAllocs()
					expr, err := syntax.ParseLogSelector(test, true)
					if err != nil {
						b.Fatal(err)
					}
					p, err := expr.Pipeline()
					if err != nil {
						b.Fatal(err)
					}
					var iters []iter.EntryIterator
					for _, lbs := range labelsSet {
						it, err := c.Iterator(context.Background(), time.Unix(0, 0), time.Now(), logproto.FORWARD, p.ForStream(lbs))
						if err != nil {
							b.Fatal(err)
						}
						iters = append(iters, it)
					}
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						for _, it := range iters {
							for it.Next() {
								streams = append(streams, logproto.Stream{Labels: it.Labels(), Entries: []logproto.Entry{it.Entry()}})
							}
						}
					}
					streams = streams[:0]
				})
			}

			for _, test := range []string{
				`rate({app="foo"}[1m])`,
				`sum by (cluster) (rate({app="foo"}[10s]))`,
				`sum by (cluster) (rate({app="foo"} != "foo" [10s]))`,
				`sum by (cluster) (rate({app="foo"} != "foo" | logfmt[10s]))`,
				`sum by (caller) (rate({app="foo"} != "foo" | logfmt[10s]))`,
				`sum by (cluster) (rate({app="foo"} != "foo" | logfmt | duration > 10ms[10s]))`,
				`sum by (cluster) (rate({app="foo"} != "foo" | logfmt | duration > 10ms and component="tsdb"[1m]))`,
			} {
				b.Run(test, func(b *testing.B) {
					b.ReportAllocs()
					expr, err := syntax.ParseSampleExpr(test)
					if err != nil {
						b.Fatal(err)
					}
					ex, err := expr.Extractor()
					if err != nil {
						b.Fatal(err)
					}
					var iters []iter.SampleIterator
					for _, lbs := range labelsSet {
						iters = append(iters, c.SampleIterator(context.Background(), time.Unix(0, 0), time.Now(), ex.ForStream(lbs)))
					}
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						for _, it := range iters {
							for it.Next() {
								series = append(series, logproto.Series{Labels: it.Labels(), Samples: []logproto.Sample{it.Sample()}})
							}
						}
					}
					series = series[:0]
				})
			}
		})
	}
}

func Test_HeadIteratorReverse(t *testing.T) {
	for _, f := range HeadBlockFmts {
		t.Run(f.String(), func(t *testing.T) {
			c := NewMemChunk(EncSnappy, f, testBlockSize, testTargetSize)
			genEntry := func(i int64) *logproto.Entry {
				return &logproto.Entry{
					Timestamp: time.Unix(0, i),
					Line:      fmt.Sprintf(`msg="%d"`, i),
				}
			}
			var i int64
			for e := genEntry(i); c.SpaceFor(e); e, i = genEntry(i+1), i+1 {
				require.NoError(t, c.Append(e))
			}

			assertOrder := func(t *testing.T, total int64) {
				expr, err := syntax.ParseLogSelector(`{app="foo"} | logfmt`, true)
				require.NoError(t, err)
				p, err := expr.Pipeline()
				require.NoError(t, err)
				it, err := c.Iterator(context.TODO(), time.Unix(0, 0), time.Unix(0, i), logproto.BACKWARD, p.ForStream(labels.Labels{{Name: "app", Value: "foo"}}))
				require.NoError(t, err)
				for it.Next() {
					total--
					require.Equal(t, total, it.Entry().Timestamp.UnixNano())
				}
			}

			assertOrder(t, i)
			// let's try again without the headblock.
			require.NoError(t, c.cut())
			assertOrder(t, i)
		})
	}
}

func TestMemChunk_Rebound(t *testing.T) {
	chkFrom := time.Unix(0, 0)
	chkThrough := chkFrom.Add(time.Hour)
	originalChunk := buildTestMemChunk(t, chkFrom, chkThrough)

	for _, tc := range []struct {
		name               string
		sliceFrom, sliceTo time.Time
		err                error
	}{
		{
			name:      "slice whole chunk",
			sliceFrom: chkFrom,
			sliceTo:   chkThrough,
		},
		{
			name:      "slice first half",
			sliceFrom: chkFrom,
			sliceTo:   chkFrom.Add(30 * time.Minute),
		},
		{
			name:      "slice second half",
			sliceFrom: chkFrom.Add(30 * time.Minute),
			sliceTo:   chkThrough,
		},
		{
			name:      "slice in the middle",
			sliceFrom: chkFrom.Add(15 * time.Minute),
			sliceTo:   chkFrom.Add(45 * time.Minute),
		},
		{
			name:      "slice interval not aligned with sample intervals",
			sliceFrom: chkFrom.Add(time.Second),
			sliceTo:   chkThrough.Add(-time.Second),
		},
		{
			name:      "slice out of bounds without overlap",
			err:       chunk.ErrSliceNoDataInRange,
			sliceFrom: chkThrough.Add(time.Minute),
			sliceTo:   chkThrough.Add(time.Hour),
		},
		{
			name:      "slice out of bounds with overlap",
			sliceFrom: chkFrom.Add(10 * time.Minute),
			sliceTo:   chkThrough.Add(10 * time.Minute),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newChunk, err := originalChunk.Rebound(tc.sliceFrom, tc.sliceTo)
			if tc.err != nil {
				require.Equal(t, tc.err, err)
				return
			}
			require.NoError(t, err)

			// iterate originalChunk from slice start to slice end + nanosecond. Adding a nanosecond here to be inclusive of sample at end time.
			originalChunkItr, err := originalChunk.Iterator(context.Background(), tc.sliceFrom, tc.sliceTo.Add(time.Nanosecond), logproto.FORWARD, log.NewNoopPipeline().ForStream(labels.Labels{}))
			require.NoError(t, err)

			// iterate newChunk for whole chunk interval which should include all the samples in the chunk and hence align it with expected values.
			newChunkItr, err := newChunk.Iterator(context.Background(), chkFrom, chkThrough, logproto.FORWARD, log.NewNoopPipeline().ForStream(labels.Labels{}))
			require.NoError(t, err)

			for {
				originalChunksHasMoreSamples := originalChunkItr.Next()
				newChunkHasMoreSamples := newChunkItr.Next()

				// either both should have samples or none of them
				require.Equal(t, originalChunksHasMoreSamples, newChunkHasMoreSamples)
				if !originalChunksHasMoreSamples {
					break
				}

				require.Equal(t, originalChunkItr.Entry(), newChunkItr.Entry())
			}
		})
	}
}

func buildTestMemChunk(t *testing.T, from, through time.Time) *MemChunk {
	chk := NewMemChunk(EncGZIP, DefaultHeadBlockFmt, defaultBlockSize, 0)
	for ; from.Before(through); from = from.Add(time.Second) {
		err := chk.Append(&logproto.Entry{
			Line:      from.String(),
			Timestamp: from,
		})
		require.NoError(t, err)
	}

	return chk
}
