package faketcp

import (
	"encoding/binary"
	"fmt"
	"log"
	"sync"

	"github.com/klauspost/reedsolomon"
)

type Fec struct {
	encoder     *FecEncoder
	decoder     *FecDecoder
	dataCh      chan []byte
	txBytes     uint64 //recovered tx bytes
	rxBytes     uint64 //recovered rx bytes
	DropPackets uint64
}

func NewFec(dataShards, parityShards, offset int) *Fec {
	return &Fec{
		encoder: NewFECEncoder(dataShards, parityShards, offset),
		decoder: NewFECDecoder(3*(dataShards+parityShards), dataShards, parityShards),
		dataCh:  make(chan []byte, (dataShards + parityShards)), // fec recovered data channel, 这个chan 不需要很大缓存，因为当有recovered数据产生时，会立即优先被业务层读取。
	}
}

func (fec *Fec) EncodeData(b []byte, send func([]byte) error) error {
	fecBufSize := FecHeaderSizePlus2 + len(b)
	fecBuf := make([]byte, fecBufSize)
	copy(fecBuf[FecHeaderSizePlus2:], b)
	ps := fec.Encode(fecBuf[:fecBufSize])

	if err := send(fecBuf); err != nil {
		return err
	}

	for _, p := range ps {
		if err := send(p); err != nil {
			return err
		}
		fec.txBytes += uint64(len(p))
	}
	return nil
}

func (f *Fec) GetDataCh() chan []byte {
	return f.dataCh
}

func (f *Fec) GetDropPackets() uint64 {
	return f.DropPackets
}

// 业务层统一调用这个接口。
// 先从dataCh中读取数据， 如果没有数据， 再从getData中读取数据， 然后解码。
func (fec *Fec) DecodeData(getData func() ([]byte, error)) ([]byte, error) {
	for {
		select {
		case data := <-fec.dataCh:
			fec.rxBytes += uint64(len(data))
			return data, nil
		default:
			data, err := getData()
			if err != nil {
				return nil, err
			}
			if len(data) == 0 {
				continue
			}

			data = fec.decode(data)
			if data != nil {
				return data, nil
			}
			//data == nil, means TypeFec data, go to next loop,
		}
	}
}

// 把TypeData数据和从recovered中提取数据，放到dataCh中(业务层读的时候, 优先从dataCh中读取)
func (fec *Fec) decode(data []byte) []byte {
	fecPacket := fec.decoder.DecodeFecPacket(data)
	// log.Printf("input1 fec pkt: %+v", fecPacket)
	if fecPacket.GetType() == TypeData {
		data = data[FecHeaderSizePlus2:]
	} else {
		data = nil
		if fecPacket.GetType() != TypeFEC {
			panic(fmt.Sprintf("fecPacket.GetType():%d, TypeFEC:%d", fecPacket.GetType(), TypeFEC))
		}
	}

	recovered := fec.decoder.Decode(fecPacket)
	for _, r := range recovered {
		if len(r) <= 2 {
			continue
		}
		sz := binary.LittleEndian.Uint16(r)
		if sz < 2 || int(sz) > len(r) {
			panic(fmt.Sprintf("fec data invalid size:%d, len(r):%d", sz, len(r))) //panic: fec data invalid size:2654, len(r):1422
			//continue
		}
		r := r[2:sz]

		select {
		case fec.dataCh <- r:
		default:
			fec.DropPackets++
			log.Printf("fecDataCh is full, drop data len: %d, fec dropPacket:%d", len(r), fec.DropPackets)
			panic(fmt.Sprintf("fecDataCh is full, drop data len: %d, fec dropPacket:%d", len(r), fec.DropPackets))
		}
	}

	return data
}

func (fec *Fec) Encode(data []byte) [][]byte {
	return fec.encoder.Encode(data)
}

/*
     FecHeaderSize      | 2B data size
      4           1   1    2 (Byte)
+---+---+---+---+---+---+---+---+---------------+
|     seqid     |  flag |  plen |  user payloay |
+---+---+---+---+---+---+---+---+---------------+
|	fec header			|---fec de/endcode buf--|

*/

const (
	FecHeaderSize      = 6
	FecHeaderSizePlus2 = FecHeaderSize + 2 // plus 2B data size
	TypeData           = 0xf1
	TypeFEC            = 0xf2
)

const (
	// mtuLimit = 1528
	mtuLimit = 1 << 11 // 2048
	maxLen   = 1 << 16
)

var (
	// shared among sending/receiving/FEC
	xmitBuf sync.Pool
)

func init() {
	xmitBuf.New = func() interface{} {
		return make([]byte, mtuLimit)
	}
}

type (
	// fecPacket is a decoded FEC packet
	fecPacket struct {
		seqid uint32
		flag  uint16
		data  []byte
	}

	// FecDecoder for decoding incoming packets
	FecDecoder struct {
		rxlimit      int // queue size limit
		dataShards   int
		parityShards int
		shardSize    int
		rx           []fecPacket // ordered receive queue

		// caches
		decodeCache [][]byte
		flagCache   []bool

		// zeros
		zeros []byte

		// RS decoder
		codec reedsolomon.Encoder
	}
)

func (fp *fecPacket) GetType() uint16 {
	return fp.flag
}

func NewFECDecoder(rxlimit, dataShards, parityShards int) *FecDecoder {
	if dataShards <= 0 || parityShards <= 0 {
		return nil
	}
	if rxlimit < dataShards+parityShards {
		return nil
	}

	dec := new(FecDecoder)
	dec.rxlimit = rxlimit
	dec.dataShards = dataShards
	dec.parityShards = parityShards
	dec.shardSize = dataShards + parityShards
	codec, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil
	}
	dec.codec = codec
	dec.decodeCache = make([][]byte, dec.shardSize)
	dec.flagCache = make([]bool, dec.shardSize)
	dec.zeros = make([]byte, mtuLimit)
	return dec
}

func (dec *FecDecoder) DecodeFecPacket(data []byte) fecPacket {
	var pkt fecPacket
	pkt.seqid = binary.LittleEndian.Uint32(data)
	pkt.flag = binary.LittleEndian.Uint16(data[4:])
	// allocate memory & copy
	buf := xmitBuf.Get().([]byte)[:len(data)-6]
	copy(buf, data[6:])
	pkt.data = buf
	return pkt
}
func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

// decode a fec packet
func (dec *FecDecoder) Decode(pkt fecPacket) (recovered [][]byte) {
	// insertion
	n := len(dec.rx) - 1
	insertIdx := 0
	for i := n; i >= 0; i-- {
		if pkt.seqid == dec.rx[i].seqid { // de-duplicate
			xmitBuf.Put(pkt.data)
			return nil
		} else if _itimediff(pkt.seqid, dec.rx[i].seqid) > 0 { // insertion
			insertIdx = i + 1
			break
		}
	}

	// insert into ordered rx queue
	if insertIdx == n+1 {
		dec.rx = append(dec.rx, pkt)
	} else {
		dec.rx = append(dec.rx, fecPacket{})
		copy(dec.rx[insertIdx+1:], dec.rx[insertIdx:]) // shift right
		dec.rx[insertIdx] = pkt
	}

	// shard range for current packet
	shardBegin := pkt.seqid - pkt.seqid%uint32(dec.shardSize)
	shardEnd := shardBegin + uint32(dec.shardSize) - 1

	// max search range in ordered queue for current shard
	searchBegin := insertIdx - int(pkt.seqid%uint32(dec.shardSize))
	if searchBegin < 0 {
		searchBegin = 0
	}
	searchEnd := searchBegin + dec.shardSize - 1
	if searchEnd >= len(dec.rx) {
		searchEnd = len(dec.rx) - 1
	}

	// re-construct datashards
	if searchEnd-searchBegin+1 >= dec.dataShards {
		var numshard, numDataShard, first, maxlen int

		// zero cache
		shards := dec.decodeCache
		shardsflag := dec.flagCache
		for k := range dec.decodeCache {
			shards[k] = nil
			shardsflag[k] = false
		}

		// shard assembly
		for i := searchBegin; i <= searchEnd; i++ {
			seqid := dec.rx[i].seqid
			if _itimediff(seqid, shardEnd) > 0 {
				break
			} else if _itimediff(seqid, shardBegin) >= 0 {
				shards[seqid%uint32(dec.shardSize)] = dec.rx[i].data
				shardsflag[seqid%uint32(dec.shardSize)] = true
				numshard++
				if dec.rx[i].flag == TypeData {
					numDataShard++
				}
				if numshard == 1 {
					first = i
				}
				if len(dec.rx[i].data) > maxlen {
					maxlen = len(dec.rx[i].data)
				}
			}
		}

		if numDataShard == dec.dataShards {
			// case 1:  no lost data shards
			dec.rx = dec.freeRange(first, numshard, dec.rx)
		} else if numshard >= dec.dataShards {
			// case 2: data shard lost, but  recoverable from parity shard
			for k := range shards {
				if shards[k] != nil {
					dlen := len(shards[k])
					shards[k] = shards[k][:maxlen]
					copy(shards[k][dlen:], dec.zeros)
				}
			}
			if err := dec.codec.ReconstructData(shards); err == nil {
				for k := range shards[:dec.dataShards] {
					if !shardsflag[k] {
						recovered = append(recovered, shards[k])
					}
				}
			}
			dec.rx = dec.freeRange(first, numshard, dec.rx)
		}
	}

	// keep rxlimit
	if len(dec.rx) > dec.rxlimit {
		if dec.rx[0].flag == TypeData { // record unrecoverable data
			// log.Println("record unrecoverable data")
		}
		dec.rx = dec.freeRange(0, 1, dec.rx)
	}
	return
}

// free a range of fecPacket, and zero for GC recycling
func (dec *FecDecoder) freeRange(first, n int, q []fecPacket) []fecPacket {
	for i := first; i < first+n; i++ { // free
		xmitBuf.Put(q[i].data)
	}
	copy(q[first:], q[first+n:])
	for i := 0; i < n; i++ { // dereference data
		q[len(q)-1-i].data = nil
	}
	return q[:len(q)-n]
}

type (
	// FecEncoder for encoding outgoing packets
	FecEncoder struct {
		dataShards   int
		parityShards int
		shardSize    int
		paws         uint32 // Protect Against Wrapped Sequence numbers
		next         uint32 // next seqid

		shardCount int // count the number of datashards collected
		maxSize    int // record maximum data length in datashard

		headerOffset  int // FEC header offset
		payloadOffset int // FEC payload offset

		// caches
		shardCache  [][]byte
		encodeCache [][]byte

		// zeros
		zeros []byte

		// RS encoder
		codec reedsolomon.Encoder
	}
)

func NewFECEncoder(dataShards, parityShards, offset int) *FecEncoder {
	if dataShards <= 0 || parityShards <= 0 {
		return nil
	}
	enc := new(FecEncoder)
	enc.dataShards = dataShards
	enc.parityShards = parityShards
	enc.shardSize = dataShards + parityShards
	enc.paws = (0xffffffff/uint32(enc.shardSize) - 1) * uint32(enc.shardSize)
	enc.headerOffset = offset
	enc.payloadOffset = enc.headerOffset + FecHeaderSize

	codec, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil
	}
	enc.codec = codec

	// caches
	enc.encodeCache = make([][]byte, enc.shardSize)
	enc.shardCache = make([][]byte, enc.shardSize)
	for k := range enc.shardCache {
		enc.shardCache[k] = make([]byte, mtuLimit)
	}
	enc.zeros = make([]byte, mtuLimit)
	return enc
}

// encode the packet, output parity shards if we have enough datashards
// the content of returned parityshards will change in next encode
func (enc *FecEncoder) Encode(b []byte) (ps [][]byte) {
	enc.markData(b[enc.headerOffset:])
	binary.LittleEndian.PutUint16(b[enc.payloadOffset:], uint16(len(b[enc.payloadOffset:])))

	// copy data to fec datashards
	sz := len(b)
	enc.shardCache[enc.shardCount] = enc.shardCache[enc.shardCount][:sz]
	copy(enc.shardCache[enc.shardCount], b)
	enc.shardCount++

	// record max datashard length
	if sz > enc.maxSize {
		enc.maxSize = sz
	}

	//  calculate Reed-Solomon Erasure Code
	if enc.shardCount == enc.dataShards {
		// bzero each datashard's tail
		for i := 0; i < enc.dataShards; i++ {
			shard := enc.shardCache[i]
			slen := len(shard)
			copy(shard[slen:enc.maxSize], enc.zeros)
		}

		// construct equal-sized slice with stripped header
		cache := enc.encodeCache
		for k := range cache {
			cache[k] = enc.shardCache[k][enc.payloadOffset:enc.maxSize]
		}

		// rs encode
		if err := enc.codec.Encode(cache); err == nil {
			ps = enc.shardCache[enc.dataShards:]
			for k := range ps {
				enc.markFEC(ps[k][enc.headerOffset:])
				ps[k] = ps[k][:enc.maxSize]
			}
		}

		// reset counters to zero
		enc.shardCount = 0
		enc.maxSize = 0
	}

	return
}

func (enc *FecEncoder) markData(data []byte) {
	binary.LittleEndian.PutUint32(data, enc.next)
	binary.LittleEndian.PutUint16(data[4:], TypeData)
	enc.next++
}

func (enc *FecEncoder) markFEC(data []byte) {
	binary.LittleEndian.PutUint32(data, enc.next)
	binary.LittleEndian.PutUint16(data[4:], TypeFEC)
	enc.next = (enc.next + 1) % enc.paws
}
