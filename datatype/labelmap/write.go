package labelmap

import (
	"fmt"
	"io"
	"sync"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/common/downres"
	"github.com/janelia-flyem/dvid/datatype/common/labels"
	"github.com/janelia-flyem/dvid/datatype/imageblk"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

type putOperation struct {
	data       []byte // the full label volume sent to PUT
	scale      uint8
	subvol     *dvid.Subvolume
	indexZYX   dvid.IndexZYX
	version    dvid.VersionID
	mutate     bool   // if false, we just ingest without needing to GET previous value
	mutID      uint64 // should be unique within a server's uptime.
	downresMut *downres.Mutation
	blockCh    chan blockChange
}

// PutLabels persists voxels from a subvolume into the storage engine.  This involves transforming
// a supplied volume of uint64 with known geometry into many labels.Block that tiles the subvolume.
// Messages are sent to subscribers for ingest events.
func (d *Data) PutLabels(v dvid.VersionID, subvol *dvid.Subvolume, data []byte, roiname dvid.InstanceName, mutate bool) error {
	if subvol.DataShape().ShapeDimensions() != 3 {
		return fmt.Errorf("cannot store labels for data %q in non 3D format", d.DataName())
	}

	// Make sure data is block-aligned
	if !dvid.BlockAligned(subvol, d.BlockSize()) {
		return fmt.Errorf("cannot store labels for data %q in non-block aligned geometry %s -> %s", d.DataName(), subvol.StartPoint(), subvol.EndPoint())
	}

	// Make sure the received data buffer is of appropriate size.
	labelBytes := subvol.Size().Prod() * 8
	if labelBytes != int64(len(data)) {
		return fmt.Errorf("expected %d bytes for data %q label PUT but only received %d bytes", labelBytes, d.DataName(), len(data))
	}

	r, err := imageblk.GetROI(v, roiname, subvol)
	if err != nil {
		return err
	}

	// Only do voxel-based mutations one at a time.  This lets us remove handling for block-level concurrency.
	d.voxelMu.Lock()
	defer d.voxelMu.Unlock()

	// Keep track of changing extents, labels and mark repo as dirty if changed.
	var extentChanged bool
	defer func() {
		if extentChanged {
			err := datastore.SaveDataByVersion(v, d)
			if err != nil {
				dvid.Infof("Error in trying to save repo on change: %v\n", err)
			}
		}
	}()

	// Track point extents
	ctx := datastore.NewVersionedCtx(d, v)
	extents := d.Extents()
	if extents.AdjustPoints(subvol.StartPoint(), subvol.EndPoint()) {
		extentChanged = true
		if err := d.PostExtents(ctx, extents.MinPoint, extents.MaxPoint); err != nil {
			return err
		}
	}

	// extract buffer interface if it exists
	var putbuffer storage.RequestBuffer
	store, err := datastore.GetOrderedKeyValueDB(d)
	if err != nil {
		return fmt.Errorf("Data type imageblk had error initializing store: %v\n", err)
	}
	if req, ok := store.(storage.KeyValueRequester); ok {
		putbuffer = req.NewBuffer(ctx)
	}

	// Iterate through index space for this data.
	mutID := d.NewMutationID()
	downresMut := downres.NewMutation(d, v, mutID)

	wg := new(sync.WaitGroup)

	blockCh := make(chan blockChange, 100)
	svmap, err := getMapping(d, v)
	if err != nil {
		return fmt.Errorf("PutLabels couldn't get mapping for data %q, version %d: %v", d.DataName(), v, err)
	}
	go d.aggregateBlockChanges(v, svmap, blockCh)

	blocks := 0
	for it, err := subvol.NewIndexZYXIterator(d.BlockSize()); err == nil && it.Valid(); it.NextSpan() {
		i0, i1, err := it.IndexSpan()
		if err != nil {
			close(blockCh)
			return err
		}
		ptBeg := i0.Duplicate().(dvid.ChunkIndexer)
		ptEnd := i1.Duplicate().(dvid.ChunkIndexer)

		begX := ptBeg.Value(0)
		endX := ptEnd.Value(0)

		if extents.AdjustIndices(ptBeg, ptEnd) {
			extentChanged = true
		}

		wg.Add(int(endX-begX) + 1)
		c := dvid.ChunkPoint3d{begX, ptBeg.Value(1), ptBeg.Value(2)}
		for x := begX; x <= endX; x++ {
			c[0] = x
			curIndex := dvid.IndexZYX(c)

			// Don't PUT if this index is outside a specified ROI
			if r != nil && r.Iter != nil && !r.Iter.InsideFast(curIndex) {
				wg.Done()
				continue
			}

			putOp := &putOperation{
				data:       data,
				subvol:     subvol,
				indexZYX:   curIndex,
				version:    v,
				mutate:     mutate,
				mutID:      mutID,
				downresMut: downresMut,
				blockCh:    blockCh,
			}
			server.CheckChunkThrottling()
			go d.putChunk(putOp, wg, putbuffer)
			blocks++
		}
	}
	wg.Wait()
	close(blockCh)

	// if a bufferable op, flush
	if putbuffer != nil {
		putbuffer.Flush()
	}

	return downresMut.Execute()
}

// Puts a chunk of data as part of a mapped operation.
// Only some multiple of the # of CPU cores can be used for chunk handling before
// it waits for chunk processing to abate via the buffered server.HandlerToken channel.
func (d *Data) putChunk(op *putOperation, wg *sync.WaitGroup, putbuffer storage.RequestBuffer) {
	defer func() {
		// After processing a chunk, return the token.
		server.HandlerToken <- 1

		// Notify the requestor that this chunk is done.
		wg.Done()
	}()

	bcoord := op.indexZYX.ToIZYXString()
	ctx := datastore.NewVersionedCtx(d, op.version)

	// If we are mutating, get the previous label Block
	var scale uint8
	var oldBlock *labels.PositionedBlock
	if op.mutate {
		var err error
		if oldBlock, err = d.getLabelPositionedBlock(ctx, scale, bcoord); err != nil {
			dvid.Errorf("Unable to load previous block in %q, key %v: %v\n", d.DataName(), bcoord, err)
			return
		}
	}

	// Get the current label Block from the received label array
	blockSize, ok := d.BlockSize().(dvid.Point3d)
	if !ok {
		dvid.Errorf("can't putChunk() on data %q with non-3d block size: %s", d.DataName(), d.BlockSize())
		return
	}
	curBlock, err := labels.SubvolumeToBlock(op.subvol, op.data, op.indexZYX, blockSize)
	if err != nil {
		dvid.Errorf("error creating compressed block from label array at %s", op.subvol)
		return
	}
	go d.updateBlockMaxLabel(op.version, curBlock)

	blockData, _ := curBlock.MarshalBinary()
	serialization, err := dvid.SerializeData(blockData, d.Compression(), d.Checksum())
	if err != nil {
		dvid.Errorf("Unable to serialize block in %q: %v\n", d.DataName(), err)
		return
	}

	store, err := datastore.GetOrderedKeyValueDB(d)
	if err != nil {
		dvid.Errorf("Data type imageblk had error initializing store: %v\n", err)
		return
	}

	callback := func(ready chan error) {
		if ready != nil {
			if resperr := <-ready; resperr != nil {
				dvid.Errorf("Unable to PUT voxel data for block %v: %v\n", bcoord, resperr)
				return
			}
		}
		var event string
		var delta interface{}
		if oldBlock != nil && op.mutate {
			event = labels.MutateBlockEvent
			block := MutatedBlock{op.mutID, bcoord, &(oldBlock.Block), curBlock}
			d.handleBlockMutate(op.version, op.blockCh, block)
			delta = block
		} else {
			event = labels.IngestBlockEvent
			block := IngestedBlock{op.mutID, bcoord, curBlock}
			d.handleBlockIndexing(op.version, op.blockCh, block)
			delta = block
		}
		if err := op.downresMut.BlockMutated(bcoord, curBlock); err != nil {
			dvid.Errorf("data %q publishing downres: %v\n", d.DataName(), err)
		}
		evt := datastore.SyncEvent{d.DataUUID(), event}
		msg := datastore.SyncMessage{event, op.version, delta}
		if err := datastore.NotifySubscribers(evt, msg); err != nil {
			dvid.Errorf("Unable to notify subscribers of event %s in %s\n", event, d.DataName())
		}
	}

	// put data -- use buffer if available
	tk := NewBlockTKeyByCoord(op.scale, bcoord)
	if putbuffer != nil {
		ready := make(chan error, 1)
		go callback(ready)
		putbuffer.PutCallback(ctx, tk, serialization, ready)
	} else {
		if err := store.Put(ctx, tk, serialization); err != nil {
			dvid.Errorf("Unable to PUT voxel data for block %s: %v\n", bcoord, err)
			return
		}
		callback(nil)
	}
}

// Writes a XY image into the blocks that intersect it.  This function assumes the
// blocks have been allocated and if necessary, filled with old data.
func (d *Data) writeXYImage(v dvid.VersionID, vox *imageblk.Voxels, b storage.TKeyValues) (extentChanged bool, err error) {

	// Setup concurrency in image -> block transfers.
	var wg sync.WaitGroup
	defer wg.Wait()

	// Iterate through index space for this data using ZYX ordering.
	blockSize := d.BlockSize()
	var startingBlock int32

	for it, err := vox.NewIndexIterator(blockSize); err == nil && it.Valid(); it.NextSpan() {
		indexBeg, indexEnd, err := it.IndexSpan()
		if err != nil {
			return extentChanged, err
		}

		ptBeg := indexBeg.Duplicate().(dvid.ChunkIndexer)
		ptEnd := indexEnd.Duplicate().(dvid.ChunkIndexer)

		// Track point extents
		if d.Extents().AdjustIndices(ptBeg, ptEnd) {
			extentChanged = true
		}

		// Do image -> block transfers in concurrent goroutines.
		begX := ptBeg.Value(0)
		endX := ptEnd.Value(0)

		server.CheckChunkThrottling()
		wg.Add(1)
		go func(blockNum int32) {
			c := dvid.ChunkPoint3d{begX, ptBeg.Value(1), ptBeg.Value(2)}
			for x := begX; x <= endX; x++ {
				c[0] = x
				curIndex := dvid.IndexZYX(c)
				b[blockNum].K = NewBlockTKey(0, &curIndex)

				// Write this slice data into the block.
				vox.WriteBlock(&(b[blockNum]), blockSize)
				blockNum++
			}
			server.HandlerToken <- 1
			wg.Done()
		}(startingBlock)

		startingBlock += (endX - begX + 1)
	}
	return
}

// KVWriteSize is the # of key-value pairs we will write as one atomic batch write.
const KVWriteSize = 500

// TODO -- Clean up all the writing and simplify now that we have block-aligned writes.
// writeBlocks ingests blocks of voxel data asynchronously using batch writes.
func (d *Data) writeBlocks(v dvid.VersionID, b storage.TKeyValues, wg1, wg2 *sync.WaitGroup) error {
	batcher, err := datastore.GetKeyValueBatcher(d)
	if err != nil {
		return err
	}

	preCompress, postCompress := 0, 0
	blockSize := d.BlockSize().(dvid.Point3d)

	ctx := datastore.NewVersionedCtx(d, v)
	evt := datastore.SyncEvent{d.DataUUID(), labels.IngestBlockEvent}

	server.CheckChunkThrottling()
	blockCh := make(chan blockChange, 100)
	svmap, err := getMapping(d, v)
	if err != nil {
		return fmt.Errorf("writeBlocks couldn't get mapping for data %q, version %d: %v", d.DataName(), v, err)
	}
	go d.aggregateBlockChanges(v, svmap, blockCh)
	go func() {
		defer func() {
			wg1.Done()
			wg2.Done()
			dvid.Debugf("Wrote voxel blocks.  Before %s: %d bytes.  After: %d bytes\n", d.Compression(), preCompress, postCompress)
			close(blockCh)
			server.HandlerToken <- 1
		}()

		mutID := d.NewMutationID()
		batch := batcher.NewBatch(ctx)
		for i, block := range b {
			preCompress += len(block.V)
			lblBlock, err := labels.MakeBlock(block.V, blockSize)
			if err != nil {
				dvid.Errorf("unable to compute dvid block compression in %q: %v\n", d.DataName(), err)
				return
			}
			go d.updateBlockMaxLabel(v, lblBlock)

			compressed, _ := lblBlock.MarshalBinary()
			serialization, err := dvid.SerializeData(compressed, d.Compression(), d.Checksum())
			if err != nil {
				dvid.Errorf("Unable to serialize block in %q: %v\n", d.DataName(), err)
				return
			}
			postCompress += len(serialization)
			batch.Put(block.K, serialization)

			_, indexZYX, err := DecodeBlockTKey(block.K)
			if err != nil {
				dvid.Errorf("Unable to recover index from block key: %v\n", block.K)
				return
			}

			block := IngestedBlock{mutID, indexZYX.ToIZYXString(), lblBlock}
			d.handleBlockIndexing(v, blockCh, block)

			msg := datastore.SyncMessage{labels.IngestBlockEvent, v, block}
			if err := datastore.NotifySubscribers(evt, msg); err != nil {
				dvid.Errorf("Unable to notify subscribers of ChangeBlockEvent in %s\n", d.DataName())
				return
			}

			// Check if we should commit
			if i%KVWriteSize == KVWriteSize-1 {
				if err := batch.Commit(); err != nil {
					dvid.Errorf("Error on trying to write batch: %v\n", err)
					return
				}
				batch = batcher.NewBatch(ctx)
			}
		}
		if err := batch.Commit(); err != nil {
			dvid.Errorf("Error on trying to write batch: %v\n", err)
			return
		}
	}()
	return nil
}

func (d *Data) blockChangesExtents(extents *dvid.Extents, bx, by, bz int32) bool {
	blockSize := d.BlockSize().(dvid.Point3d)
	start := dvid.Point3d{bx * blockSize[0], by * blockSize[1], bz * blockSize[2]}
	end := dvid.Point3d{start[0] + blockSize[0] - 1, start[1] + blockSize[1] - 1, start[2] + blockSize[2] - 1}
	return extents.AdjustPoints(start, end)
}

// storeBlocks reads blocks from io.ReadCloser and puts them in store, handling metadata bookkeeping
// unlike ingestBlocks function.
func (d *Data) storeBlocks(ctx *datastore.VersionedCtx, r io.ReadCloser, scale uint8, downscale bool, compression string, indexing bool) error {
	if r == nil {
		return fmt.Errorf("no data blocks POSTed")
	}

	if downscale && scale != 0 {
		return fmt.Errorf("cannot downscale blocks of scale > 0")
	}

	switch compression {
	case "", "blocks":
	default:
		return fmt.Errorf(`compression must be "blocks" (default) at this time`)
	}

	timedLog := dvid.NewTimeLog()
	store, err := datastore.GetOrderedKeyValueDB(d)
	if err != nil {
		return fmt.Errorf("Data type labelmap had error initializing store: %v", err)
	}

	// Only do voxel-based mutations one at a time.  This lets us remove handling for block-level concurrency.
	d.voxelMu.Lock()
	defer d.voxelMu.Unlock()

	d.StartUpdate()
	defer d.StopUpdate()

	// extract buffer interface if it exists
	var putbuffer storage.RequestBuffer
	if req, ok := store.(storage.KeyValueRequester); ok {
		putbuffer = req.NewBuffer(ctx)
	}

	mutID := d.NewMutationID()
	var downresMut *downres.Mutation
	if downscale {
		downresMut = downres.NewMutation(d, ctx.VersionID(), mutID)
	}

	svmap, err := getMapping(d, ctx.VersionID())
	if err != nil {
		return fmt.Errorf("ReceiveBlocks couldn't get mapping for data %q, version %d: %v", d.DataName(), ctx.VersionID(), err)
	}
	var blockCh chan blockChange
	var putWG, processWG sync.WaitGroup
	if indexing {
		blockCh = make(chan blockChange, 100)
		processWG.Add(1)
		go func() {
			d.aggregateBlockChanges(ctx.VersionID(), svmap, blockCh)
			processWG.Done()
		}()
	}

	callback := func(bcoord dvid.IZYXString, block *labels.Block, ready chan error) {
		if ready != nil {
			if resperr := <-ready; resperr != nil {
				dvid.Errorf("Unable to PUT voxel data for block %v: %v\n", bcoord, resperr)
				return
			}
		}
		event := labels.IngestBlockEvent
		ingestBlock := IngestedBlock{mutID, bcoord, block}
		if scale == 0 {
			if indexing {
				d.handleBlockIndexing(ctx.VersionID(), blockCh, ingestBlock)
			}
			go d.updateBlockMaxLabel(ctx.VersionID(), ingestBlock.Data)
			evt := datastore.SyncEvent{d.DataUUID(), event}
			msg := datastore.SyncMessage{event, ctx.VersionID(), ingestBlock}
			if err := datastore.NotifySubscribers(evt, msg); err != nil {
				dvid.Errorf("Unable to notify subscribers of event %s in %s\n", event, d.DataName())
			}
			if downscale {
				if err := downresMut.BlockMutated(bcoord, block); err != nil {
					dvid.Errorf("data %q publishing downres: %v\n", d.DataName(), err)
				}
			}
		}

		putWG.Done()
	}

	if d.Compression().Format() != dvid.Gzip {
		return fmt.Errorf("labelmap %q cannot accept GZIP /blocks POST since it internally uses %s", d.DataName(), d.Compression().Format())
	}
	var extentsChanged bool
	extents, err := d.GetExtents(ctx)
	if err != nil {
		return err
	}
	var numBlocks int
	for {
		block, compressed, bx, by, bz, err := readStreamedBlock(r, scale)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		bcoord := dvid.ChunkPoint3d{bx, by, bz}.ToIZYXString()
		tk := NewBlockTKeyByCoord(scale, bcoord)
		if scale == 0 {
			if mod := d.blockChangesExtents(&extents, bx, by, bz); mod {
				extentsChanged = true
			}
			go d.updateBlockMaxLabel(ctx.VersionID(), block)
		}
		serialization, err := dvid.SerializePrecompressedData(compressed, d.Compression(), d.Checksum())
		if err != nil {
			return fmt.Errorf("can't serialize received block %s data: %v", bcoord, err)
		}
		putWG.Add(1)
		if putbuffer != nil {
			ready := make(chan error, 1)
			go callback(bcoord, block, ready)
			putbuffer.PutCallback(ctx, tk, serialization, ready)
		} else {
			if err := store.Put(ctx, tk, serialization); err != nil {
				return fmt.Errorf("Unable to PUT voxel data for block %s: %v", bcoord, err)
			}
			go callback(bcoord, block, nil)
		}
		numBlocks++
	}

	putWG.Wait()
	if blockCh != nil {
		close(blockCh)
	}
	processWG.Wait()

	if extentsChanged {
		if err := d.PostExtents(ctx, extents.StartPoint(), extents.EndPoint()); err != nil {
			dvid.Criticalf("could not modify extents for labelmap %q: %v\n", d.DataName(), err)
		}
	}

	// if a bufferable op, flush
	if putbuffer != nil {
		putbuffer.Flush()
	}
	if downscale {
		if err := downresMut.Execute(); err != nil {
			return err
		}
	}
	timedLog.Infof("Received and stored %d blocks for labelmap %q", numBlocks, d.DataName())
	return nil
}

// Writes supervoxel blocks without worrying about overlap or computation of indices, syncs, maxlabel, or extents.
// Additional goroutines are not spawned so caller can set concurrency through parallel evocations.
func (d *Data) ingestBlocks(ctx *datastore.VersionedCtx, r io.ReadCloser, scale uint8) error {
	if r == nil {
		return fmt.Errorf("no data blocks POSTed")
	}

	timedLog := dvid.NewTimeLog()
	store, err := datastore.GetKeyValueDB(d)
	if err != nil {
		return fmt.Errorf("Data type labelmap had error initializing store: %v", err)
	}

	if d.Compression().Format() != dvid.Gzip {
		return fmt.Errorf("labelmap %q cannot accept GZIP /blocks POST since it internally uses %s", d.DataName(), d.Compression().Format())
	}

	d.StartUpdate()
	defer d.StopUpdate()

	var numBlocks int
	for {
		_, compressed, bx, by, bz, err := readStreamedBlock(r, scale)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		bcoord := dvid.ChunkPoint3d{bx, by, bz}.ToIZYXString()
		tk := NewBlockTKeyByCoord(scale, bcoord)
		serialization, err := dvid.SerializePrecompressedData(compressed, d.Compression(), d.Checksum())
		if err != nil {
			return fmt.Errorf("can't serialize received block %s data: %v", bcoord, err)
		}
		if err := store.Put(ctx, tk, serialization); err != nil {
			return fmt.Errorf("unable to PUT voxel data for block %s: %v", bcoord, err)
		}
		numBlocks++
	}

	timedLog.Infof("Received and stored %d blocks for labelmap %q", numBlocks, d.DataName())
	return nil
}
