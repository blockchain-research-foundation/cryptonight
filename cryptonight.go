// HEAD_PLACEHOLDER
// +build ignore

// Package cryptonight implements CryptoNight hash function and some of its
// variant. Original CryptoNight algorithm is defined in CNS008 at
// https://cryptonote.org/cns/cns008.txt
package cryptonight // import "ekyu.moe/cryptonight"

import (
	"encoding/binary"
	"hash"
	"math"
	"runtime"
	"unsafe"

	"github.com/aead/skein"
	"github.com/dchest/blake256"

	"ekyu.moe/cryptonight/groestl"
	"ekyu.moe/cryptonight/internal/aes"
	"ekyu.moe/cryptonight/internal/sha3"
	"ekyu.moe/cryptonight/jh"
)

// This field is for macro definitions.
// We define it in a literal string so that it can trick gofmt(1).
//
// It should be empty after they are expanded by cpp(1).
const _ = `
#undef build
#undef ignore

#define U64_U8(a, begin, end) \
    ( (*[( (end) - (begin) ) * 8 ]uint8)(unsafe.Pointer(&a[ (begin) ])) )

#define U64_U32(a, begin, end) \
    ( (*[( (end) - (begin) ) * 2 ]uint32)(unsafe.Pointer(&a[ (begin) ])) )

#define U64_U16_LEN8(a, begin) \
    ( (*[8]uint16)(unsafe.Pointer(&a[ (begin) ])) )

#define U64_U32_LEN4(a, begin) \
    ( (*[4]uint32)(unsafe.Pointer(&a[ (begin) ])) )

#define TO_ADDR(a) (( (a[0]) & 0x1ffff0) >> 3)

#define VARIANT2_SHUFFLE() \
	/* each chunk has 16 bytes, or 8 group of 2-bytes */ \
	chunk0 = U64_U16_LEN8(cache.scratchpad, addr^0x02); \
	chunk1 = U64_U16_LEN8(cache.scratchpad, addr^0x04); \
	chunk2 = U64_U16_LEN8(cache.scratchpad, addr^0x06); \
	\
	/* Shuffle modification \
	   ( 0 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23) -> \
	   (18 22 19 23 16 17 20 21 2 5 3 4 6 7 0 1 9 13  8 12 10 11 14 15) \
	   See https://github.com/SChernykh/xmr-stak-cpu/blob/master/README.md for details */ \
	chunk0[0], chunk0[1], chunk0[2], chunk0[3],         \
	    chunk0[4], chunk0[5], chunk0[6], chunk0[7],     \
	    chunk1[0], chunk1[1], chunk1[2], chunk1[3],     \
	    chunk1[4], chunk1[5], chunk1[6], chunk1[7],     \
	    chunk2[0], chunk2[1], chunk2[2], chunk2[3],     \
	    chunk2[4], chunk2[5], chunk2[6], chunk2[7] =    \
	    chunk2[2], chunk2[6], chunk2[3], chunk2[7],     \
	    chunk2[0], chunk2[1], chunk2[4], chunk2[5],     \
	    chunk0[2], chunk0[5], chunk0[3], chunk0[4],     \
	    chunk0[6], chunk0[7], chunk0[0], chunk0[1],     \
	    chunk1[1], chunk1[5], chunk1[0], chunk1[4],     \
	    chunk1[2], chunk1[3], chunk1[6], chunk1[7];
`

// To trick goimports(1).
var _ = unsafe.Pointer(nil)

// Cache can reuse the memory chunks for potential multiple Sum calls. A Cache
// instance occupies at least 2,097,352 bytes in memory.
//
// cache.Sum is not concurrent safe. A Cache only allows at most one Sum running.
// If you intend to call cache.Sum it concurrently, you should either create
// multiple Cache instances (recommended for mining apps), or use a sync.Pool to
// manage multiple Cache instances (recommended for mining pools).
//
//
// Example for multiple instances (for mining app):
//      n := runtime.GOMAXPROCS()
//      c := make([]*cryptonight.Cached, n)
//      for i := 0; i < n; i++ {
//          c[i] = new(cryptonight.Cached)
//      }
//
//      // ...
//      for _, v := range c {
//          go func() {
//              for {
//                  sum := v.Sum(data, 1)
//                  // do something with sum...
//              }
//          }()
//      }
//      // ...
//
//
// Example for sync.Pool (for mining pool):
//      cachePool := sync.Pool{
//          New: func() interface{} {
//              return new(cryptonight.Cache)
//          },
//      }
//
//      // ...
//      blob := <-share // received from some miner
//      if len(blob) < 43 { // input for variant 1 must be longer than 43 bytes.
//      	// reject share...
//      	return
//      }
//      cache := cachePool.Get().(*cryptonight.Cache)
//      sum := cache.Sum(blob, 1)
//      // cache is not used after calling Sum, should be returned to the memory
//      // pool as soon as possible, and it's better not to use a defer here.
//      cachePool.Put(cache)
//      // calculate difficulty.
//      diff := cryptonight.Difficulty(sum)
//      // accept share...
//
// The zero value for Cache is ready to use.
type Cache struct {
	finalState [25]uint64                  // state of keccak1600
	scratchpad [2 * 1024 * 1024 / 8]uint64 // 2 MiB scratchpad for memhard loop
}

// Sum calculate a CryptoNight hash digest. The return value is exactly 32 bytes
// long.
//
// When variant is 1, data is required to have at least 43 bytes.
// This is assumed and not checked by Sum. If this condition doesn't meet, Sum
// will panic straightforward.
func (cache *Cache) Sum(data []byte, variant int) []byte {
	// as per CNS008 sec.3 Scratchpad Initialization
	sha3.Keccak1600State(&cache.finalState, data)

	// for variant 1
	var tweak, t uint64
	if variant == 1 {
		// that's why data must have more than 43 bytes
		tweak = cache.finalState[24] ^ binary.LittleEndian.Uint64(data[35:43])
	}

	// for variant 2
	var (
		divisionResult, sqrtResult uint64
		dividend, divisor          uint64
		chunk0, chunk1, chunk2     *[8]uint16 // references
	)

	// scratchpad init
	key := cache.finalState[:4]
	rkeys := new([40]uint32) // 10 rounds, instead of 14 as in standard AES-256
	aes.CnExpandKey(key, rkeys)
	blocks := make([]uint64, 16)
	copy(blocks, cache.finalState[8:24])

	for i := 0; i < 2*1024*1024/8; i += 16 {
		for j := 0; j < 16; j += 2 {
			aes.CnRounds(blocks[j:], blocks[j:], rkeys)
		}
		copy(cache.scratchpad[i:], blocks)
	}

	// as per CNS008 sec.4 Memory-Hard Loop
	a, b, c := new([2]uint64), new([2]uint64), new([2]uint64)
	product := new([2]uint64) // product in byteMul step
	addr := uint64(0)         // address index

	a[0] = cache.finalState[0] ^ cache.finalState[4]
	a[1] = cache.finalState[1] ^ cache.finalState[5]
	b[0] = cache.finalState[2] ^ cache.finalState[6]
	b[1] = cache.finalState[3] ^ cache.finalState[7]

	for i := 0; i < 524288; i++ {
		addr = TO_ADDR(a)
		aes.CnSingleRound(c[:], cache.scratchpad[addr:], U64_U32(a, 0, 2))

		if variant == 2 {
			VARIANT2_SHUFFLE()
		}

		cache.scratchpad[addr] = b[0] ^ c[0]
		cache.scratchpad[addr+1] = b[1] ^ c[1]
		b[0], b[1] = c[0], c[1]

		if variant == 1 {
			t = cache.scratchpad[addr+1] >> 24
			t = ((^t)&1)<<4 | (((^t)&1)<<4&t)<<1 | (t&32)>>1
			cache.scratchpad[addr+1] ^= t << 24
		}

		addr = TO_ADDR(b)
		c[0] = cache.scratchpad[addr]
		c[1] = cache.scratchpad[addr+1]

		if variant == 2 {
			c[1] ^= divisionResult ^ sqrtResult
			dividend = b[1]
			divisor = b[0]&0xffffffff | 0x80000001
			divisionResult = (dividend/divisor)&0xffffffff | ((dividend % divisor) << 32)
			sqrtResult = uint64(math.Sqrt(float64((b[0] + divisionResult) >> 16)))
		}

		byteMul(product, b[0], c[0])

		if variant == 2 {
			VARIANT2_SHUFFLE()
		}

		// byteAdd
		a[0] += product[0]
		a[1] += product[1]

		cache.scratchpad[addr] = a[0]
		cache.scratchpad[addr+1] = a[1]
		a[0] ^= c[0]
		a[1] ^= c[1]

		if variant == 1 {
			cache.scratchpad[addr+1] ^= tweak
		}
	}

	// as per CNS008 sec.5 Result Calculation
	key = cache.finalState[4:8]
	aes.CnExpandKey(key, rkeys)
	blocks = cache.finalState[8:24]

	for i := 0; i < 2*1024*1024/8; i += 16 {
		for j := 0; j < 16; j += 2 {
			cache.scratchpad[i+j] ^= blocks[j]
			cache.scratchpad[i+j+1] ^= blocks[j+1]
			aes.CnRounds(cache.scratchpad[i+j:], cache.scratchpad[i+j:], rkeys)
		}
		blocks = cache.scratchpad[i : i+16]
	}

	copy(cache.finalState[8:24], blocks)

	// This KeepAlive is a must, as we hacked too much for memory.
	runtime.KeepAlive(cache.finalState)
	sha3.Keccak1600Permute(&cache.finalState)

	var h hash.Hash
	switch cache.finalState[0] & 0x03 {
	case 0x00:
		h = blake256.New()
	case 0x01:
		h = groestl.New256()
	case 0x02:
		h = jh.New256()
	default:
		h = skein.New256(nil)
	}
	h.Write(U64_U8(cache.finalState, 0, 25)[:])

	return h.Sum(nil)
}

// Sum calculate a CryptoNight hash digest. The return value is exactly 32 bytes
// long.
//
// When variant is 1, data is required to have at least 43 bytes.
// This is assumed and not checked by Sum. If this condition doesn't meet, Sum
// will panic straightforward.
//
// Sum is not recommended for a large scale of calls since CryptoNight itself is
// a memory hard algorithm. In such scenario, consider using Cache instead.
func Sum(data []byte, variant int) []byte {
	return new(Cache).Sum(data, variant)
}
