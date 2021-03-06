// Copyright (C) 2018 Storj Labs, Inc.
// See LICENSE for copying information.

package eestream

import (
	"context"
	"io"
	"io/ioutil"
	"sync"

	"storj.io/storj/internal/pkg/readcloser"
	"storj.io/storj/pkg/ranger"
	"storj.io/storj/pkg/utils"
)

type decodedReader struct {
	ctx             context.Context
	cancel          context.CancelFunc
	readers         map[int]io.ReadCloser
	scheme          ErasureScheme
	stripeReader    *StripeReader
	outbuf          []byte
	err             error
	currentStripe   int64
	expectedStripes int64
	close           sync.Once
	closeErr        error
}

// DecodeReaders takes a map of readers and an ErasureScheme returning a
// combined Reader.
//
// rs is a map of erasure piece numbers to erasure piece streams.
// expectedSize is the number of bytes expected to be returned by the Reader.
// mbm is the maximum memory (in bytes) to be allocated for read buffers. If
// set to 0, the minimum possible memory will be used.
func DecodeReaders(ctx context.Context, rs map[int]io.ReadCloser,
	es ErasureScheme, expectedSize int64, mbm int) io.ReadCloser {
	if expectedSize < 0 {
		return readcloser.FatalReadCloser(Error.New("negative expected size"))
	}
	if expectedSize%int64(es.DecodedBlockSize()) != 0 {
		return readcloser.FatalReadCloser(
			Error.New("expected size (%d) not a factor decoded block size (%d)",
				expectedSize, es.DecodedBlockSize()))
	}
	if err := checkMBM(mbm); err != nil {
		return readcloser.FatalReadCloser(err)
	}
	dr := &decodedReader{
		readers:         rs,
		scheme:          es,
		stripeReader:    NewStripeReader(rs, es, mbm),
		outbuf:          make([]byte, 0, es.DecodedBlockSize()),
		expectedStripes: expectedSize / int64(es.DecodedBlockSize()),
	}
	dr.ctx, dr.cancel = context.WithCancel(ctx)
	// Kick off a goroutine to watch for context cancelation.
	go func() {
		<-dr.ctx.Done()
		_ = dr.Close()
	}()
	return dr
}

func (dr *decodedReader) Read(p []byte) (n int, err error) {
	if len(dr.outbuf) <= 0 {
		// if the output buffer is empty, let's fill it again
		// if we've already had an error, fail
		if dr.err != nil {
			return 0, dr.err
		}
		// return EOF is the expected stripes were read
		if dr.currentStripe >= dr.expectedStripes {
			dr.err = io.EOF
			return 0, dr.err
		}
		// read the input buffers of the next stripe - may also decode it
		dr.outbuf, dr.err = dr.stripeReader.ReadStripe(dr.currentStripe, dr.outbuf)
		if dr.err != nil {
			return 0, dr.err
		}
		dr.currentStripe++
	}

	// copy what data we have to the output
	n = copy(p, dr.outbuf)
	// slide the remaining bytes to the beginning
	copy(dr.outbuf, dr.outbuf[n:])
	// shrink the remaining buffer
	dr.outbuf = dr.outbuf[:len(dr.outbuf)-n]
	return n, nil
}

func (dr *decodedReader) Close() error {
	// cancel the context to terminate reader goroutines
	dr.cancel()
	// avoid double close of readers
	dr.close.Do(func() {
		errs := make([]error, len(dr.readers)+1)
		// close the readers
		for i, r := range dr.readers {
			errs[i] = r.Close()
		}
		// close the stripe reader
		errs[len(dr.readers)] = dr.stripeReader.Close()
		dr.closeErr = utils.CombineErrors(errs...)
	})
	return dr.closeErr
}

type decodedRanger struct {
	es     ErasureScheme
	rrs    map[int]ranger.Ranger
	inSize int64
	mbm    int // max buffer memory
}

// Decode takes a map of Rangers and an ErasureScheme and returns a combined
// Ranger.
//
// rrs is a map of erasure piece numbers to erasure piece rangers.
// mbm is the maximum memory (in bytes) to be allocated for read buffers. If
// set to 0, the minimum possible memory will be used.
func Decode(rrs map[int]ranger.Ranger, es ErasureScheme, mbm int) (ranger.Ranger, error) {
	if err := checkMBM(mbm); err != nil {
		return nil, err
	}
	if len(rrs) < es.RequiredCount() {
		return nil, Error.New("not enough readers to reconstruct data!")
	}
	size := int64(-1)
	for _, rr := range rrs {
		if size == -1 {
			size = rr.Size()
		} else {
			if size != rr.Size() {
				return nil, Error.New(
					"decode failure: range reader sizes don't all match")
			}
		}
	}
	if size == -1 {
		return ranger.ByteRanger(nil), nil
	}
	if size%int64(es.EncodedBlockSize()) != 0 {
		return nil, Error.New("invalid erasure decoder and range reader combo. "+
			"range reader size (%d) must be a multiple of erasure encoder block size (%d)",
			size, es.EncodedBlockSize())
	}
	return &decodedRanger{
		es:     es,
		rrs:    rrs,
		inSize: size,
		mbm:    mbm,
	}, nil
}

func (dr *decodedRanger) Size() int64 {
	blocks := dr.inSize / int64(dr.es.EncodedBlockSize())
	return blocks * int64(dr.es.DecodedBlockSize())
}

func (dr *decodedRanger) Range(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	// offset and length might not be block-aligned. figure out which
	// blocks contain this request
	firstBlock, blockCount := calcEncompassingBlocks(
		offset, length, dr.es.DecodedBlockSize())
	// go ask for ranges for all those block boundaries
	// do it parallel to save from network latency
	readers := make(map[int]io.ReadCloser, len(dr.rrs))
	type indexReadCloser struct {
		i   int
		r   io.ReadCloser
		err error
	}
	result := make(chan indexReadCloser, len(dr.rrs))
	for i, rr := range dr.rrs {
		go func(i int, rr ranger.Ranger) {
			r, err := rr.Range(ctx,
				firstBlock*int64(dr.es.EncodedBlockSize()),
				blockCount*int64(dr.es.EncodedBlockSize()))
			result <- indexReadCloser{i: i, r: r, err: err}
		}(i, rr)
	}
	// wait for all goroutines to finish and save result in readers map
	for range dr.rrs {
		res := <-result
		if res.err != nil {
			readers[res.i] = readcloser.FatalReadCloser(res.err)
		} else {
			readers[res.i] = res.r
		}
	}
	// decode from all those ranges
	r := DecodeReaders(ctx, readers, dr.es, blockCount*int64(dr.es.DecodedBlockSize()), dr.mbm)
	// offset might start a few bytes in, potentially discard the initial bytes
	_, err := io.CopyN(ioutil.Discard, r,
		offset-firstBlock*int64(dr.es.DecodedBlockSize()))
	if err != nil {
		return nil, Error.Wrap(err)
	}
	// length might not have included all of the blocks, limit what we return
	return readcloser.LimitReadCloser(r, length), nil
}
