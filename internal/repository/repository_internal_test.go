package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/go-cmp/cmp"
	"github.com/klauspost/compress/zstd"
	"github.com/restic/restic/internal/backend"
	"github.com/restic/restic/internal/crypto"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/restic"
	rtest "github.com/restic/restic/internal/test"
)

type mapcache map[backend.Handle]bool

func (c mapcache) Has(h backend.Handle) bool { return c[h] }

func TestSortCachedPacksFirst(t *testing.T) {
	var (
		blobs, sorted [100]restic.PackedBlob

		cache = make(mapcache)
		r     = rand.New(rand.NewSource(1261))
	)

	for i := 0; i < len(blobs); i++ {
		var id restic.ID
		r.Read(id[:])
		blobs[i] = restic.PackedBlob{PackID: id}

		if i%3 == 0 {
			h := backend.Handle{Name: id.String(), Type: backend.PackFile}
			cache[h] = true
		}
	}

	copy(sorted[:], blobs[:])
	sort.SliceStable(sorted[:], func(i, j int) bool {
		hi := backend.Handle{Type: backend.PackFile, Name: sorted[i].PackID.String()}
		hj := backend.Handle{Type: backend.PackFile, Name: sorted[j].PackID.String()}
		return cache.Has(hi) && !cache.Has(hj)
	})

	sortCachedPacksFirst(cache, blobs[:])
	rtest.Equals(t, sorted, blobs)
}

func BenchmarkSortCachedPacksFirst(b *testing.B) {
	const nblobs = 512 // Corresponds to a file of ca. 2GB.

	var (
		blobs [nblobs]restic.PackedBlob
		cache = make(mapcache)
		r     = rand.New(rand.NewSource(1261))
	)

	for i := 0; i < nblobs; i++ {
		var id restic.ID
		r.Read(id[:])
		blobs[i] = restic.PackedBlob{PackID: id}

		if i%3 == 0 {
			h := backend.Handle{Name: id.String(), Type: backend.PackFile}
			cache[h] = true
		}
	}

	var cpy [nblobs]restic.PackedBlob
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		copy(cpy[:], blobs[:])
		sortCachedPacksFirst(cache, cpy[:])
	}
}

// buildPackfileWithoutHeader returns a manually built pack file without a header.
func buildPackfileWithoutHeader(blobSizes []int, key *crypto.Key, compress bool) (blobs []restic.Blob, packfile []byte) {
	opts := []zstd.EOption{
		// Set the compression level configured.
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		// Disable CRC, we have enough checks in place, makes the
		// compressed data four bytes shorter.
		zstd.WithEncoderCRC(false),
		// Set a window of 512kbyte, so we have good lookbehind for usual
		// blob sizes.
		zstd.WithWindowSize(512 * 1024),
	}
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		panic(err)
	}

	var offset uint
	for i, size := range blobSizes {
		plaintext := rtest.Random(800+i, size)
		id := restic.Hash(plaintext)
		uncompressedLength := uint(0)
		if compress {
			uncompressedLength = uint(len(plaintext))
			plaintext = enc.EncodeAll(plaintext, nil)
		}

		// we use a deterministic nonce here so the whole process is
		// deterministic, last byte is the blob index
		var nonce = []byte{
			0x15, 0x98, 0xc0, 0xf7, 0xb9, 0x65, 0x97, 0x74,
			0x12, 0xdc, 0xd3, 0x62, 0xa9, 0x6e, 0x20, byte(i),
		}

		before := len(packfile)
		packfile = append(packfile, nonce...)
		packfile = key.Seal(packfile, nonce, plaintext, nil)
		after := len(packfile)

		ciphertextLength := after - before

		blobs = append(blobs, restic.Blob{
			BlobHandle: restic.BlobHandle{
				Type: restic.DataBlob,
				ID:   id,
			},
			Length:             uint(ciphertextLength),
			UncompressedLength: uncompressedLength,
			Offset:             offset,
		})

		offset = uint(len(packfile))
	}

	return blobs, packfile
}

func TestStreamPack(t *testing.T) {
	TestAllVersions(t, testStreamPack)
}

func testStreamPack(t *testing.T, version uint) {
	// always use the same key for deterministic output
	const jsonKey = `{"mac":{"k":"eQenuI8adktfzZMuC8rwdA==","r":"k8cfAly2qQSky48CQK7SBA=="},"encrypt":"MKO9gZnRiQFl8mDUurSDa9NMjiu9MUifUrODTHS05wo="}`

	var key crypto.Key
	err := json.Unmarshal([]byte(jsonKey), &key)
	if err != nil {
		t.Fatal(err)
	}

	blobSizes := []int{
		5522811,
		10,
		5231,
		18812,
		123123,
		13522811,
		12301,
		892242,
		28616,
		13351,
		252287,
		188883,
		3522811,
		18883,
	}

	var compress bool
	switch version {
	case 1:
		compress = false
	case 2:
		compress = true
	default:
		t.Fatal("test does not support repository version", version)
	}

	packfileBlobs, packfile := buildPackfileWithoutHeader(blobSizes, &key, compress)

	loadCalls := 0
	shortFirstLoad := false

	loadBytes := func(length int, offset int64) []byte {
		data := packfile

		if offset > int64(len(data)) {
			offset = 0
			length = 0
		}
		data = data[offset:]

		if length > len(data) {
			length = len(data)
		}
		if shortFirstLoad {
			length /= 2
			shortFirstLoad = false
		}

		return data[:length]
	}

	load := func(ctx context.Context, h backend.Handle, length int, offset int64, fn func(rd io.Reader) error) error {
		data := loadBytes(length, offset)
		if shortFirstLoad {
			data = data[:len(data)/2]
			shortFirstLoad = false
		}

		loadCalls++

		err := fn(bytes.NewReader(data))
		if err == nil {
			return nil
		}
		var permanent *backoff.PermanentError
		if errors.As(err, &permanent) {
			return err
		}

		// retry loading once
		return fn(bytes.NewReader(loadBytes(length, offset)))
	}

	// first, test regular usage
	t.Run("regular", func(t *testing.T) {
		tests := []struct {
			blobs          []restic.Blob
			calls          int
			shortFirstLoad bool
		}{
			{packfileBlobs[1:2], 1, false},
			{packfileBlobs[2:5], 1, false},
			{packfileBlobs[2:8], 1, false},
			{[]restic.Blob{
				packfileBlobs[0],
				packfileBlobs[4],
				packfileBlobs[2],
			}, 1, false},
			{[]restic.Blob{
				packfileBlobs[0],
				packfileBlobs[len(packfileBlobs)-1],
			}, 2, false},
			{packfileBlobs[:], 1, true},
		}

		for _, test := range tests {
			t.Run("", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				gotBlobs := make(map[restic.ID]int)

				handleBlob := func(blob restic.BlobHandle, buf []byte, err error) error {
					gotBlobs[blob.ID]++

					id := restic.Hash(buf)
					if !id.Equal(blob.ID) {
						t.Fatalf("wrong id %v for blob %s returned", id, blob.ID)
					}

					return err
				}

				wantBlobs := make(map[restic.ID]int)
				for _, blob := range test.blobs {
					wantBlobs[blob.ID] = 1
				}

				loadCalls = 0
				shortFirstLoad = test.shortFirstLoad
				err = streamPack(ctx, load, &key, restic.ID{}, test.blobs, handleBlob)
				if err != nil {
					t.Fatal(err)
				}

				if !cmp.Equal(wantBlobs, gotBlobs) {
					t.Fatal(cmp.Diff(wantBlobs, gotBlobs))
				}
				rtest.Equals(t, test.calls, loadCalls)
			})
		}
	})
	shortFirstLoad = false

	// next, test invalid uses, which should return an error
	t.Run("invalid", func(t *testing.T) {
		tests := []struct {
			blobs []restic.Blob
			err   string
		}{
			{
				// pass one blob several times
				blobs: []restic.Blob{
					packfileBlobs[3],
					packfileBlobs[8],
					packfileBlobs[3],
					packfileBlobs[4],
				},
				err: "overlapping blobs in pack",
			},

			{
				// pass something that's not a valid blob in the current pack file
				blobs: []restic.Blob{
					{
						Offset: 123,
						Length: 20000,
					},
				},
				err: "ciphertext verification failed",
			},

			{
				// pass a blob that's too small
				blobs: []restic.Blob{
					{
						Offset: 123,
						Length: 10,
					},
				},
				err: "invalid blob length",
			},
		}

		for _, test := range tests {
			t.Run("", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				handleBlob := func(blob restic.BlobHandle, buf []byte, err error) error {
					return err
				}

				err = streamPack(ctx, load, &key, restic.ID{}, test.blobs, handleBlob)
				if err == nil {
					t.Fatalf("wanted error %v, got nil", test.err)
				}

				if !strings.Contains(err.Error(), test.err) {
					t.Fatalf("wrong error returned, it should contain %q but was %q", test.err, err)
				}
			})
		}
	})
}
