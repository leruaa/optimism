package fromda

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-supervisor/supervisor/types"
)

func (db *DB) AddDerived(derivedFrom eth.BlockRef, derived eth.BlockRef) error {
	db.rwLock.Lock()
	defer db.rwLock.Unlock()
	return db.addLink(derivedFrom, derived, common.Hash{})
}

// ReplaceInvalidatedBlock replaces the current Invalidated block with the given replacement.
// The to-be invalidated hash must be provided for consistency checks.
func (db *DB) ReplaceInvalidatedBlock(replacementDerived eth.BlockRef, invalidated common.Hash) (types.DerivedBlockSealPair, error) {
	db.rwLock.Lock()
	defer db.rwLock.Unlock()

	db.log.Warn("Replacing invalidated block", "replacement", replacementDerived, "invalidated", invalidated)

	// We take the last occurrence. This is where it started to be considered invalid,
	// and where we thus stopped building additional entries for it.
	lastIndex := db.store.LastEntryIdx()
	if lastIndex < 0 {
		return types.DerivedBlockSealPair{}, types.ErrFuture
	}
	last, err := db.readAt(lastIndex)
	if err != nil {
		return types.DerivedBlockSealPair{}, fmt.Errorf("failed to read last derivation data: %w", err)
	}
	if !last.invalidated {
		return types.DerivedBlockSealPair{}, fmt.Errorf("cannot replace block %d, that was not invalidated, with block %s: %w", last.derived, replacementDerived, types.ErrConflict)
	}
	if last.derived.Hash != invalidated {
		return types.DerivedBlockSealPair{}, fmt.Errorf("cannot replace invalidated %s, DB contains %s: %w", invalidated, last.derived, types.ErrConflict)
	}
	// Find the parent-block of derived-from.
	// We need this to build a block-ref, so the DB can be consistency-checked when the next entry is added.
	// There is always one, since the first entry in the DB should never be an invalidated one.
	prevDerivedFrom, err := db.previousDerivedFrom(last.derivedFrom.ID())
	if err != nil {
		return types.DerivedBlockSealPair{}, err
	}
	// Remove the invalidated placeholder and everything after
	err = db.store.Truncate(lastIndex - 1)
	if err != nil {
		return types.DerivedBlockSealPair{}, err
	}
	replacement := types.DerivedBlockRefPair{
		DerivedFrom: last.derivedFrom.ForceWithParent(prevDerivedFrom.ID()),
		Derived:     replacementDerived,
	}
	// Insert the replacement
	if err := db.addLink(replacement.DerivedFrom, replacement.Derived, invalidated); err != nil {
		return types.DerivedBlockSealPair{}, fmt.Errorf("failed to add %s as replacement at %s: %w", replacement.Derived, replacement.DerivedFrom, err)
	}
	return replacement.Seals(), nil
}

// RewindAndInvalidate rolls back the database to just before the invalidated block,
// and then marks the block as invalidated, so that no new data can be added to the DB
// until a Rewind or ReplaceInvalidatedBlock.
func (db *DB) RewindAndInvalidate(invalidated types.DerivedBlockRefPair) error {
	db.rwLock.Lock()
	defer db.rwLock.Unlock()

	invalidatedSeals := types.DerivedBlockSealPair{
		DerivedFrom: types.BlockSealFromRef(invalidated.DerivedFrom),
		Derived:     types.BlockSealFromRef(invalidated.Derived),
	}
	if err := db.rewindLocked(invalidatedSeals, true); err != nil {
		return err
	}
	if err := db.addLink(invalidated.DerivedFrom, invalidated.Derived, invalidated.Derived.Hash); err != nil {
		return fmt.Errorf("failed to add invalidation entry %s: %w", invalidated, err)
	}
	return nil
}

// Rewind rolls back the database to the target, including the target if the including flag is set.
// it locks the DB and calls rewindLocked.
func (db *DB) Rewind(target types.DerivedBlockSealPair, including bool) error {
	db.rwLock.Lock()
	defer db.rwLock.Unlock()
	return db.rewindLocked(target, including)
}

// RewindToL2 rewinds to the first entry where the L2 block with the given number was derived.
func (db *DB) RewindToL2(derived uint64) error {
	db.rwLock.Lock()
	defer db.rwLock.Unlock()
	_, link, err := db.firstDerivedFrom(derived)
	if err != nil {
		return fmt.Errorf("failed to find first derived-from %d: %w", derived, err)
	}
	return db.rewindLocked(types.DerivedBlockSealPair{
		DerivedFrom: link.derivedFrom,
		Derived:     link.derived,
	}, false)
}

// RewindToL1 rewinds to the last entry that was derived from a L1 block with the given block number.
func (db *DB) RewindToL1(derivedFrom uint64) error {
	db.rwLock.Lock()
	defer db.rwLock.Unlock()
	_, link, err := db.lastDerivedAt(derivedFrom)
	if err != nil {
		return fmt.Errorf("failed to find last derived %d: %w", derivedFrom, err)
	}
	return db.rewindLocked(types.DerivedBlockSealPair{
		DerivedFrom: link.derivedFrom,
		Derived:     link.derived,
	}, false)
}

// rewindLocked performs the truncate operation to a specified block seal pair.
// data beyond the specified block seal pair is truncated from the database.
// if including is true, the block seal pair itself is removed as well.
// Note: This function must be called with the rwLock held.
// Callers are responsible for locking and unlocking the Database.
func (db *DB) rewindLocked(t types.DerivedBlockSealPair, including bool) error {
	i, link, err := db.lookup(t.DerivedFrom.Number, t.Derived.Number)
	if err != nil {
		return err
	}
	if link.derivedFrom.Hash != t.DerivedFrom.Hash {
		return fmt.Errorf("found derived-from %s, but expected %s: %w",
			link.derivedFrom, t.DerivedFrom, types.ErrConflict)
	}
	if link.derived.Hash != t.Derived.Hash {
		return fmt.Errorf("found derived %s, but expected %s: %w",
			link.derived, t.Derived, types.ErrConflict)
	}
	// adjust the target index to include the block seal pair itself if requested
	target := i
	if including {
		target = i - 1
	}
	if err := db.store.Truncate(target); err != nil {
		return fmt.Errorf("failed to rewind upon block invalidation of %s: %w", t, err)
	}
	db.m.RecordDBDerivedEntryCount(int64(target) + 1)
	return nil
}

// addLink adds a L1/L2 derivation link, with strong consistency checks.
// if the link invalidates a prior L2 block, that was valid in a prior L1,
// the invalidated hash needs to match it, even if a new derived block replaces it.
func (db *DB) addLink(derivedFrom eth.BlockRef, derived eth.BlockRef, invalidated common.Hash) error {
	link := LinkEntry{
		derivedFrom: types.BlockSeal{
			Hash:      derivedFrom.Hash,
			Number:    derivedFrom.Number,
			Timestamp: derivedFrom.Time,
		},
		derived: types.BlockSeal{
			Hash:      derived.Hash,
			Number:    derived.Number,
			Timestamp: derived.Time,
		},
		invalidated: (invalidated != common.Hash{}) && derived.Hash == invalidated,
	}
	// If we don't have any entries yet, allow any block to start things off
	if db.store.Size() == 0 {
		if link.invalidated {
			return fmt.Errorf("first DB entry %s cannot be an invalidated entry: %w", link, types.ErrConflict)
		}
		e := link.encode()
		if err := db.store.Append(e); err != nil {
			return err
		}
		db.m.RecordDBDerivedEntryCount(db.store.Size())
		return nil
	}

	last, err := db.latest()
	if err != nil {
		return err
	}
	if last.invalidated {
		return fmt.Errorf("cannot build %s on top of invalidated entry %s: %w", link, last, types.ErrConflict)
	}
	lastDerivedFrom := last.derivedFrom
	lastDerived := last.derived

	if lastDerived.ID() == derived.ID() && lastDerivedFrom.ID() == derivedFrom.ID() {
		// it shouldn't be possible, but the ID component of a block ref doesn't include the timestamp
		// so if the timestampt doesn't match, still return no error to the caller, but at least log a warning
		if lastDerived.Timestamp != derived.Time {
			db.log.Warn("Derived block already exists with different timestamp", "derived", derived, "lastDerived", lastDerived)
		}
		if lastDerivedFrom.Timestamp != derivedFrom.Time {
			db.log.Warn("Derived-from block already exists with different timestamp", "derivedFrom", derivedFrom, "lastDerivedFrom", lastDerivedFrom)
		}
		// Repeat of same information. No entries to be written.
		// But we can silently ignore and not return an error, as that brings the caller
		// in a consistent state, after which it can insert the actual new derived-from information.
		return nil
	}

	// Check derived relation: the L2 chain has to be sequential without gaps. An L2 block may repeat if the L1 block is empty.
	if lastDerived.Number == derived.Number {
		// Same block height? Then it must be the same block.
		// I.e. we encountered an empty L1 block, and the same L2 block continues to be the last block that was derived from it.
		if invalidated != (common.Hash{}) {
			if lastDerived.Hash != invalidated {
				return fmt.Errorf("inserting block %s that invalidates %s at height %d, but expected %s", derived.Hash, invalidated, lastDerived.Number, lastDerived.Hash)
			}
		} else {
			if lastDerived.Hash != derived.Hash {
				return fmt.Errorf("derived block %s conflicts with known derived block %s at same height: %w",
					derived, lastDerived, types.ErrConflict)
			}
		}
	} else if lastDerived.Number+1 == derived.Number {
		if lastDerived.Hash != derived.ParentHash {
			return fmt.Errorf("derived block %s (parent %s) does not build on %s: %w",
				derived, derived.ParentHash, lastDerived, types.ErrConflict)
		}
	} else if lastDerived.Number+1 < derived.Number {
		return fmt.Errorf("cannot add block (%s derived from %s), last block (%s derived from %s) is too far behind: (%w)",
			derived, derivedFrom,
			lastDerived, lastDerivedFrom,
			types.ErrOutOfOrder)
	} else {
		return fmt.Errorf("derived block %s is older than current derived block %s: %w",
			derived, lastDerived, types.ErrOutOfOrder)
	}

	// Check derived-from relation: multiple L2 blocks may be derived from the same L1 block. But everything in sequence.
	if lastDerivedFrom.Number == derivedFrom.Number {
		// Same block height? Then it must be the same block.
		if lastDerivedFrom.Hash != derivedFrom.Hash {
			return fmt.Errorf("cannot add block %s as derived from %s, expected to be derived from %s at this block height: %w",
				derived, derivedFrom, lastDerivedFrom, types.ErrConflict)
		}
	} else if lastDerivedFrom.Number+1 == derivedFrom.Number {
		// parent hash check
		if lastDerivedFrom.Hash != derivedFrom.ParentHash {
			return fmt.Errorf("cannot add block %s as derived from %s (parent %s) derived on top of %s: %w",
				derived, derivedFrom, derivedFrom.ParentHash, lastDerivedFrom, types.ErrConflict)
		}
	} else if lastDerivedFrom.Number+1 < derivedFrom.Number {
		// adding block that is derived from something too far into the future
		return fmt.Errorf("cannot add block (%s derived from %s), last block (%s derived from %s) is too far behind: (%w)",
			derived, derivedFrom,
			lastDerived, lastDerivedFrom,
			types.ErrOutOfOrder)
	} else {
		// adding block that is derived from something too old
		return fmt.Errorf("cannot add block %s as derived from %s, deriving already at %s: %w",
			derived, derivedFrom, lastDerivedFrom, types.ErrOutOfOrder)
	}

	e := link.encode()
	if err := db.store.Append(e); err != nil {
		return err
	}
	db.m.RecordDBDerivedEntryCount(db.store.Size())
	return nil
}
