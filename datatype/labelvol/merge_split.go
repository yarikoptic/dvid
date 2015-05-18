/*
	This file contains code that manages long-lived merge/split operations using small
	amount of globally-coordinated metadata.
*/

package labelvol

import (
	"container/list"
	"fmt"
	"io"
	"sort"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/common/labels"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/storage"
)

var (
	// These are the labels that are in the process of modification from merge, split, or other sync events.
	dirtyLabels labels.DirtyCache
)

type sizeChange struct {
	oldSize, newSize uint64
}

// MergeLabels handles merging of any number of labels throughout the various label data
// structures.  It assumes that the merges aren't cascading, e.g., there is no attempt
// to merge label 3 into 4 and also 4 into 5.  The caller should have flattened the merges.
// TODO: Provide some indication that subset of labels are under evolution, returning
//   an "unavailable" status or 203 for non-authoritative response.  This might not be
//   feasible for clustered DVID front-ends due to coordination issues.
//
// EVENTS
//
// labels.MergeStartEvent occurs at very start of merge and transmits labels.DeltaMergeStart struct.
//
// labels.MergeBlockEvent occurs for every block of a merged label and transmits labels.DeltaMerge struct.
//
// labels.MergeEndEvent occurs at end of merge and transmits labels.DeltaMergeEnd struct.
//
func (d *Data) MergeLabels(v dvid.VersionID, m labels.MergeOp) error {
	store, err := storage.SmallDataStore()
	if err != nil {
		return fmt.Errorf("Data type labelvol had error initializing store: %s\n", err.Error())
	}
	batcher, ok := store.(storage.KeyValueBatcher)
	if !ok {
		return fmt.Errorf("Data type labelvol requires batch-enabled store, which %q is not\n", store)
	}

	dvid.Debugf("Merging %s into label %d ...\n", m.Merged, m.Target)

	// Signal that we are starting a merge.
	evt := datastore.SyncEvent{d.DataName(), labels.MergeStartEvent}
	msg := datastore.SyncMessage{v, labels.DeltaMergeStart{m}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return err
	}

	// Mark these labels as dirty until done.
	iv := dvid.InstanceVersion{d.DataName(), v}
	dirtyLabels.AddMerge(iv, m)
	defer dirtyLabels.RemoveMerge(iv, m)

	// All blocks that have changed during this merge.  Key = string of block index
	blocksChanged := make(map[dvid.IZYXString]struct{})

	// Get the block-level RLEs for the toLabel
	toLabel := m.Target
	toLabelRLEs, err := d.GetLabelRLEs(v, toLabel)
	if err != nil {
		return fmt.Errorf("Can't get block-level RLEs for label %d: %s", toLabel, err.Error())
	}
	toLabelSize := toLabelRLEs.NumVoxels()

	// Iterate through all labels to be merged.
	var addedVoxels uint64
	for fromLabel := range m.Merged {
		dvid.Debugf("Merging label %d to label %d...\n", fromLabel, toLabel)

		fromLabelRLEs, err := d.GetLabelRLEs(v, fromLabel)
		if err != nil {
			return fmt.Errorf("Can't get block-level RLEs for label %d: %s", fromLabel, err.Error())
		}
		fromLabelSize := fromLabelRLEs.NumVoxels()
		if fromLabelSize == 0 || len(fromLabelRLEs) == 0 {
			dvid.Debugf("Label %d is empty.  Skipping.\n", fromLabel)
			continue
		}
		addedVoxels += fromLabelSize

		// Notify linked labelsz instances
		delta := labels.DeltaDeleteSize{
			Label:    fromLabel,
			OldSize:  fromLabelSize,
			OldKnown: true,
		}
		evt := datastore.SyncEvent{d.DataName(), labels.ChangeSizeEvent}
		msg := datastore.SyncMessage{v, delta}
		if err := datastore.NotifySubscribers(evt, msg); err != nil {
			return err
		}

		// Append or insert RLE runs from fromLabel blocks into toLabel blocks.
		for blockStr, fromRLEs := range fromLabelRLEs {
			// Mark the fromLabel blocks as modified
			blocksChanged[blockStr] = struct{}{}

			// Get the toLabel RLEs for this block and add the fromLabel RLEs
			toRLEs, found := toLabelRLEs[blockStr]
			if found {
				toRLEs.Add(fromRLEs)
			} else {
				toRLEs = fromRLEs
			}
			toLabelRLEs[blockStr] = toRLEs
		}

		// Delete all fromLabel RLEs since they are all integrated into toLabel RLEs
		minTKey := NewTKey(fromLabel, dvid.MinIndexZYX.ToIZYXString())
		maxTKey := NewTKey(fromLabel, dvid.MaxIndexZYX.ToIZYXString())
		ctx := datastore.NewVersionedContext(d, v)
		if err := store.DeleteRange(ctx, minTKey, maxTKey); err != nil {
			return fmt.Errorf("Can't delete label %d RLEs: %s", fromLabel, err.Error())
		}
	}

	if len(blocksChanged) == 0 {
		dvid.Debugf("No changes needed when merging %s into %d.  Aborting.\n", m.Merged, m.Target)
		return nil
	}

	// Publish block-level merge
	evt = datastore.SyncEvent{d.DataName(), labels.MergeBlockEvent}
	msg = datastore.SyncMessage{v, labels.DeltaMerge{m, blocksChanged}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return err
	}

	// Update datastore with all toLabel RLEs that were changed
	ctx := datastore.NewVersionedContext(d, v)
	batch := batcher.NewBatch(ctx)
	for blockStr := range blocksChanged {
		tk := NewTKey(toLabel, blockStr)
		serialization, err := toLabelRLEs[blockStr].MarshalBinary()
		if err != nil {
			return fmt.Errorf("Error serializing RLEs for label %d: %s\n", toLabel, err.Error())
		}
		batch.Put(tk, serialization)
	}
	if err := batch.Commit(); err != nil {
		dvid.Errorf("Error on updating RLEs for label %d: %s\n", toLabel, err.Error())
	}
	delta := labels.DeltaReplaceSize{
		Label:   toLabel,
		OldSize: toLabelSize,
		NewSize: toLabelSize + addedVoxels,
	}
	evt = datastore.SyncEvent{d.DataName(), labels.ChangeSizeEvent}
	msg = datastore.SyncMessage{v, delta}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return err
	}

	evt = datastore.SyncEvent{d.DataName(), labels.MergeEndEvent}
	msg = datastore.SyncMessage{v, labels.DeltaMergeEnd{m}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return err
	}

	return nil
}

// SplitLabels splits a portion of a label's voxels into a new label, which is returned.
// The input is a binary sparse volume and should preferably be the smaller portion of a labeled region.
// In other words, the caller should chose to submit for relabeling the smaller portion of any split.
//
// EVENTS
//
// labels.SplitStartEvent occurs at very start of split and transmits labels.DeltaSplitStart struct.
//
// labels.SplitBlockEvent occurs for every block of a split label and transmits labels.DeltaSplit struct.
//
// labels.SplitEndEvent occurs at end of split and transmits labels.DeltaSplitEnd struct.
//
func (d *Data) SplitLabels(v dvid.VersionID, fromLabel uint64, r io.ReadCloser) (toLabel uint64, err error) {
	store, err := storage.SmallDataStore()
	if err != nil {
		err = fmt.Errorf("Data type labelvol had error initializing store: %s\n", err.Error())
		return
	}
	batcher, ok := store.(storage.KeyValueBatcher)
	if !ok {
		err = fmt.Errorf("Data type labelvol requires batch-enabled store, which %q is not\n", store)
		return
	}

	// Mark these labels as dirty until done.
	iv := dvid.InstanceVersion{d.DataName(), v}
	dirtyLabels.Incr(iv, fromLabel)
	defer dirtyLabels.Decr(iv, fromLabel)

	// Create a new label id for this version that will persist to store
	toLabel, err = d.NewLabel(v)
	if err != nil {
		return
	}
	dvid.Debugf("Splitting subset of label %d into label %d ...\n", fromLabel, toLabel)

	// Signal that we are starting a split.
	evt := datastore.SyncEvent{d.DataName(), labels.SplitStartEvent}
	msg := datastore.SyncMessage{v, labels.DeltaSplitStart{fromLabel, toLabel}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return 0, err
	}

	// Read the sparse volume from reader.
	var split dvid.RLEs
	split, err = dvid.ReadRLEs(r)
	if err != nil {
		return
	}
	toLabelSize, _ := split.Stats()

	// Partition the split spans into blocks.
	var splitmap dvid.BlockRLEs
	splitmap, err = split.Partition(d.BlockSize)
	if err != nil {
		return
	}

	// Get a sorted list of blocks that cover split.
	splitblks := splitmap.SortedKeys()

	// Publish split event
	evt = datastore.SyncEvent{d.DataName(), labels.SplitLabelEvent}
	msg = datastore.SyncMessage{v, labels.DeltaSplit{fromLabel, toLabel, splitmap, splitblks}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return 0, err
	}

	// Write the split sparse vol.
	if err = d.writeLabelVol(v, toLabel, splitmap, splitblks); err != nil {
		return
	}

	// Iterate through the split blocks, read the original block.  If the RLEs
	// are identical, just delete the original.  If not, modify the original.
	// TODO: Modifications should be transactional since it's GET-PUT, therefore use
	// hash on block coord to direct it to block-specific goroutine; we serialize
	// requests to handle concurrency.
	ctx := datastore.NewVersionedContext(d, v)
	batch := batcher.NewBatch(ctx)

	for _, splitblk := range splitblks {

		// Get original block
		tk := NewTKey(fromLabel, splitblk)
		val, err := store.Get(ctx, tk)
		if err != nil {
			return toLabel, err
		}

		if val == nil {
			return toLabel, fmt.Errorf("Split RLEs at block %s are not part of original label %d", splitblk.Print(), fromLabel)
		}
		var rles dvid.RLEs
		if err := rles.UnmarshalBinary(val); err != nil {
			return toLabel, fmt.Errorf("Unable to unmarshal RLE for original labels in block %s", splitblk.Print())
		}

		// Compare and process based on modifications required.
		modified, dup, err := diffRLEs(splitmap[splitblk], rles)
		if err != nil {
			return toLabel, err
		}
		if dup {
			fmt.Printf("Deleting label %d block %s\n", fromLabel, splitblk.Print())
			batch.Delete(tk)
		} else {
			fmt.Printf("Storing modified block %s:\n%v\n", splitblk.Print(), modified)
			rleBytes, err := modified.MarshalBinary()
			if err != nil {
				return toLabel, fmt.Errorf("can't serialize modified RLEs for split of %d: %s\n", fromLabel, err.Error())
			}
			batch.Put(tk, rleBytes)
		}
	}

	if err := batch.Commit(); err != nil {
		dvid.Errorf("Batch PUT during split of %q label %d: %s\n", d.DataName(), fromLabel, err.Error())
	}

	// Publish change in label sizes.
	delta := labels.DeltaNewSize{
		Label: toLabel,
		Size:  toLabelSize,
	}
	evt = datastore.SyncEvent{d.DataName(), labels.ChangeSizeEvent}
	msg = datastore.SyncMessage{v, delta}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return 0, err
	}

	delta2 := labels.DeltaModSize{
		Label:      fromLabel,
		SizeChange: int64(-toLabelSize),
	}
	evt = datastore.SyncEvent{d.DataName(), labels.ChangeSizeEvent}
	msg = datastore.SyncMessage{v, delta2}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return 0, err
	}

	// Publish split end
	evt = datastore.SyncEvent{d.DataName(), labels.SplitEndEvent}
	msg = datastore.SyncMessage{v, labels.DeltaSplitEnd{fromLabel, toLabel}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return 0, err
	}

	return toLabel, nil
}

// Assumes split is a subset of original RLE.  Removes the split from the original
// and returns the result and a bool if split was a duplicate.
func diffRLEs(split, orig dvid.RLEs) (modified dvid.RLEs, dup bool, err error) {
	if split == nil || len(split) == 0 {
		err = fmt.Errorf("diffRLEs called with no splits")
		return
	}

	// Copy the original and split RLEs, then sort them.
	srles := make(dvid.RLEs, len(split))
	copy(srles, split)
	orles := make(dvid.RLEs, len(orig))
	copy(orles, orig)

	sort.Sort(srles)
	sort.Sort(orles)

	// Make original RLEs into a linked list
	out := list.New()
	for _, rle := range orles {
		out.PushBack(rle)
	}
	orles = nil

	// Iterate through all splits until we are done.
	e := out.Front()
	spos := 0
	for {
		if spos >= len(srles) {
			break
		}
		srle := srles[spos]

		// Fast forward original (superset of splits) until it meets this split.
		for {
			if e == nil {
				err = fmt.Errorf("Split RLE %s is not contained in original RLE", srle)
				return
			}
			orle := e.Value.(dvid.RLE)

			frags := orle.Excise(srle)
			if frags == nil { // no intersection, move forward
				e = e.Next()
				continue
			}
			switch len(frags) {
			case 0:
				// Split fully covers and could be larger than RLE
				next := e.Next()
				out.Remove(e)
				e = next
				if e != nil {
					peek := e.Value.(dvid.RLE)
					if srle.Intersects(peek) { // Split is larger than RLE so reuse it.
						continue
					}
				}
			case 1:
				// Replace the current RLE
				next := out.InsertAfter(frags[0], e)
				out.Remove(e)
				e = next
			case 2:
				// There's a left and right portion.  All future splits can only intersect right one.
				out.InsertBefore(frags[0], e)
				next := out.InsertAfter(frags[1], e)
				out.Remove(e)
				e = next
			default:
				err = fmt.Errorf("bad excise results - %d fragments", len(frags))
				return
			}
			break
		}

		// Move to the next split
		spos++
	}

	numRLEs := out.Len()
	if numRLEs == 0 {
		dup = true
		return
	}
	dup = false
	modified = make(dvid.RLEs, numRLEs)
	i := 0
	for e := out.Front(); e != nil; e = e.Next() {
		modified[i] = e.Value.(dvid.RLE)
		i++
	}
	return
}

// write label volume in sorted order if available.
func (d *Data) writeLabelVol(v dvid.VersionID, label uint64, brles dvid.BlockRLEs, sortblks []dvid.IZYXString) error {
	store, err := storage.SmallDataStore()
	if err != nil {
		return fmt.Errorf("Data type labelvol had error initializing store: %s\n", err.Error())
	}
	batcher, ok := store.(storage.KeyValueBatcher)
	if !ok {
		return fmt.Errorf("Data type labelvol requires batch-enabled store, which %q is not\n", store)
	}

	ctx := datastore.NewVersionedContext(d, v)
	batch := batcher.NewBatch(ctx)
	if sortblks != nil {
		for _, izyxStr := range sortblks {
			serialization, err := brles[izyxStr].MarshalBinary()
			if err != nil {
				return fmt.Errorf("Error serializing RLEs for label %d: %s\n", label, err.Error())
			}
			batch.Put(NewTKey(label, izyxStr), serialization)
		}
	} else {
		for izyxStr, rles := range brles {
			serialization, err := rles.MarshalBinary()
			if err != nil {
				return fmt.Errorf("Error serializing RLEs for label %d: %s\n", label, err.Error())
			}
			batch.Put(NewTKey(label, izyxStr), serialization)
		}
	}
	if err := batch.Commit(); err != nil {
		return fmt.Errorf("Error on updating RLEs for label %d: %s\n", label, err.Error())
	}
	return nil
}
