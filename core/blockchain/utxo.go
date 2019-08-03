// Copyright (c) 2017-2018 The qitmeer developers
package blockchain

import (
	"fmt"
	"github.com/HalalChain/qitmeer/core/dbnamespace"
	"github.com/HalalChain/qitmeer-lib/common/hash"
	"github.com/HalalChain/qitmeer-lib/core/types"
	"github.com/HalalChain/qitmeer/database"
	"github.com/HalalChain/qitmeer-lib/engine/txscript"
)

// utxoOutput houses details about an individual unspent transaction output such
// as whether or not it is spent, its public key script, and how much it pays.
//
// Standard public key scripts are stored in the database using a compressed
// format. Since the vast majority of scripts are of the standard form, a fairly
// significant savings is achieved by discarding the portions of the standard
// scripts that can be reconstructed.
//
// Also, since it is common for only a specific output in a given utxo entry to
// be referenced from a redeeming transaction, the script and amount for a given
// output is not uncompressed until the first time it is accessed.  This
// provides a mechanism to avoid the overhead of needlessly uncompressing all
// outputs for a given utxo entry at the time of load.
//
// The struct is aligned for memory efficiency.
type utxoOutput struct {
	scriptVersion uint16 // The script version
	pkScript      []byte // The public key script for the output.
	amount        uint64 // The amount of the output.
	spent         bool   // Output is spent.
}

// UtxoViewpoint represents a view into the set of unspent transaction outputs
// from a specific point of view in the chain.  For example, it could be for
// the end of the main chain, some point in the history of the main chain, or
// down a side chain.
//
// The unspent outputs are needed by other transactions for things such as
// script validation and double spend prevention.
type UtxoViewpoint struct {
	entries  map[hash.Hash]*UtxoEntry
	bestHash hash.Hash
}

// NewUtxoViewpoint returns a new empty unspent transaction output view.
func NewUtxoViewpoint() *UtxoViewpoint {
	return &UtxoViewpoint{
		entries: make(map[hash.Hash]*UtxoEntry),
	}
}

// Entries returns the underlying map that stores of all the utxo entries.
func (view *UtxoViewpoint) Entries() map[hash.Hash]*UtxoEntry {
	return view.entries
}

// AddTxOuts adds all outputs in the passed transaction which are not provably
// unspendable to the view.  When the view already has entries for any of the
// outputs, they are simply marked unspent.  All fields will be updated for
// existing entries since it's possible it has changed during a reorg.
func (view *UtxoViewpoint) AddTxOuts(theTx *types.Tx, blockOrder int64, blockIndex uint32) {
	tx := theTx.Transaction()
	// When there are not already any utxos associated with the transaction,
	// add a new entry for it to the view.
	entry := view.LookupEntry(theTx.Hash())
	if entry == nil {
		txType := types.DetermineTxType(tx)
		entry = newUtxoEntry(tx.Version, uint32(blockOrder),
			blockIndex, tx.IsCoinBaseTx(), tx.Expire != 0, txType)
		view.entries[*theTx.Hash()] = entry
	} else {
		entry.order = uint32(blockOrder)
		entry.index = blockIndex
	}
	entry.modified = true

	// Loop all of the transaction outputs and add those which are not
	// provably unspendable.
	for txOutIdx, txOut := range theTx.Transaction().TxOut {
		// TODO allow pruning of stake utxs after all other outputs are spent
		if txscript.IsUnspendable(txOut.Amount, txOut.PkScript) {
			continue
		}

		// Update existing entries.  All fields are updated because it's
		// possible (although extremely unlikely) that the existing
		// entry is being replaced by a different transaction with the
		// same hash.  This is allowed so long as the previous
		// transaction is fully spent.
		if output, ok := entry.sparseOutputs[uint32(txOutIdx)]; ok {
			output.spent = false
			output.amount = txOut.Amount
			output.pkScript = txOut.PkScript
			continue
		}

		// Add the unspent transaction output.
		entry.sparseOutputs[uint32(txOutIdx)] = &utxoOutput{
			spent:      false,
			amount:     txOut.Amount,
			pkScript:   txOut.PkScript,
		}
	}
}

// FetchUtxoView loads utxo details about the input transactions referenced by
// the passed transaction from the point of view of the end of the main chain.
// It also attempts to fetch the utxo details for the transaction itself so the
// returned view can be examined for duplicate unspent transaction outputs.
//
// This function is safe for concurrent access however the returned view is NOT.
func (b *BlockChain) FetchUtxoView(tx *types.Tx) (*UtxoViewpoint, error) {
	b.chainLock.RLock()
	defer b.chainLock.RUnlock()

	// The genesis block does not have any spendable transactions, so there
	// can't possibly be any details about it.  This is also necessary
	// because the code below requires the parent block and the genesis
	// block doesn't have one.
	view := NewUtxoViewpoint()
	// Create a set of needed transactions based on those referenced by the
	// inputs of the passed transaction.  Also, add the passed transaction
	// itself as a way for the caller to detect duplicates that are not
	// fully spent.
	txNeededSet := make(map[hash.Hash]struct{})
	txNeededSet[*tx.Hash()] = struct{}{}
	msgTx := tx.Transaction()
	if !msgTx.IsCoinBaseTx() {
		for _, txIn := range msgTx.TxIn {
			txNeededSet[txIn.PreviousOut.Hash] = struct{}{}
		}
	}

	err := view.fetchUtxosMain(b.db, txNeededSet)

	return view, err
}

// FetchUtxoEntry loads and returns the unspent transaction output entry for the
// passed hash from the point of view of the end of the main chain.
//
// NOTE: Requesting a hash for which there is no data will NOT return an error.
// Instead both the entry and the error will be nil.  This is done to allow
// pruning of fully spent transactions.  In practice this means the caller must
// check if the returned entry is nil before invoking methods on it.
//
// This function is safe for concurrent access however the returned entry (if
// any) is NOT.
func (b *BlockChain) FetchUtxoEntry(txHash *hash.Hash) (*UtxoEntry, error) {
	b.chainLock.RLock()
	defer b.chainLock.RUnlock()
	return b.fetchUtxoEntry(txHash)
}

// fetchUtxoEntry without chainLock
func (b *BlockChain) fetchUtxoEntry(txHash *hash.Hash) (*UtxoEntry, error) {
	var entry *UtxoEntry
	err := b.db.View(func(dbTx database.Tx) error {
		var err error
		entry, err = dbFetchUtxoEntry(dbTx, txHash)
		return err
	})
	if err != nil {
		return nil, err
	}

	return entry, nil
}

// BestHash returns the hash of the best block in the chain the view currently
// respresents.
func (view *UtxoViewpoint) BestHash() *hash.Hash {
	return &view.bestHash
}

// SetBestHash sets the hash of the best block in the chain the view currently
// respresents.
func (view *UtxoViewpoint) SetBestHash(hash *hash.Hash) {
	view.bestHash = *hash
}

// fetchUtxosMain fetches unspent transaction output data about the provided
// set of transactions from the point of view of the end of the main chain at
// the time of the call.
//
// Upon completion of this function, the view will contain an entry for each
// requested transaction.  Fully spent transactions, or those which otherwise
// don't exist, will result in a nil entry in the view.
func (view *UtxoViewpoint) fetchUtxosMain(db database.DB, txSet map[hash.Hash]struct{}) error {
	// Nothing to do if there are no requested hashes.
	if len(txSet) == 0 {
		return nil
	}

	// Load the unspent transaction output information for the requested set
	// of transactions from the point of view of the end of the main chain.
	//
	// NOTE: Missing entries are not considered an error here and instead
	// will result in nil entries in the view.  This is intentionally done
	// since other code uses the presence of an entry in the store as a way
	// to optimize spend and unspend updates to apply only to the specific
	// utxos that the caller needs access to.
	return db.View(func(dbTx database.Tx) error {
		for hash := range txSet {
			hashCopy := hash
			// If the UTX already exists in the view, skip adding it.
			if _, ok := view.entries[hashCopy]; ok {
				continue
			}
			entry, err := dbFetchUtxoEntry(dbTx, &hashCopy)
			if err != nil {
				return err
			}

			view.entries[hash] = entry
		}

		return nil
	})
}

// dbFetchUtxoEntry uses an existing database transaction to fetch all unspent
// outputs for the provided Bitcoin transaction hash from the utxo set.
//
// When there is no entry for the provided hash, nil will be returned for the
// both the entry and the error.
func dbFetchUtxoEntry(dbTx database.Tx, hash *hash.Hash) (*UtxoEntry, error) {
	// Fetch the unspent transaction output information for the passed
	// transaction hash.  Return now when there is no entry.
	utxoBucket := dbTx.Metadata().Bucket(dbnamespace.UtxoSetBucketName)
	serializedUtxo := utxoBucket.Get(hash[:])
	if serializedUtxo == nil {
		return nil, nil
	}

	// A non-nil zero-length entry means there is an entry in the database
	// for a fully spent transaction which should never be the case.
	if len(serializedUtxo) == 0 {
		return nil, AssertError(fmt.Sprintf("database contains entry "+
			"for fully spent tx %v", hash))
	}

	// Deserialize the utxo entry and return it.
	entry, err := deserializeUtxoEntry(serializedUtxo)
	if err != nil {
		// Ensure any deserialization errors are returned as database
		// corruption errors.
		if isDeserializeErr(err) {
			return nil, database.Error{
				ErrorCode: database.ErrCorruption,
				Description: fmt.Sprintf("corrupt utxo entry "+
					"for %v: %v", hash, err),
			}
		}

		return nil, err
	}

	return entry, nil
}

// newUtxoEntry returns a new unspent transaction output entry with the provided
// coinbase flag and block height ready to have unspent outputs added.
func newUtxoEntry(txVersion uint32, order uint32, index uint32, isCoinBase bool, hasExpiry bool, tt types.TxType) *UtxoEntry {
	return &UtxoEntry{
		sparseOutputs: make(map[uint32]*utxoOutput),
		txVersion:     txVersion,
		order:         order,
		index:         index,
		isCoinBase:    isCoinBase,
		hasExpiry:     hasExpiry,
		txType:        tt,
	}
}

// LookupEntry returns information about a given transaction according to the
// current state of the view.  It will return nil if the passed transaction
// hash does not exist in the view or is otherwise not available such as when
// it has been disconnected during a reorg.
func (view *UtxoViewpoint) LookupEntry(txHash *hash.Hash) *UtxoEntry {
	entry, ok := view.entries[*txHash]
	if !ok {
		return nil
	}

	return entry
}

// fetchInputUtxos loads utxo details about the input transactions referenced
// by the transactions in the given block into the view from the database as
// needed.  In particular, referenced entries that are earlier in the block are
// added to the view and entries that are already in the view are not modified.
// TODO, revisit the usage on the parent block
func (view *UtxoViewpoint) fetchInputUtxos(db database.DB, block *types.SerializedBlock, bc *BlockChain) error {
	// Build a map of in-flight transactions because some of the inputs in
	// this block could be referencing other transactions earlier in this
	// block which are not yet in the chain.
	txInFlight := map[hash.Hash]int{}
	txNeededSet := make(map[hash.Hash]struct{})

	transactions := block.Transactions()
	for i, tx := range transactions {
		txInFlight[*tx.Hash()] = i
	}

	// Loop through all of the transaction inputs (except for the coinbase
	// which has no inputs) collecting them into sets of what is needed and
	// what is already known (in-flight).
	for i, tx := range transactions[1:] {
		if bc.IsBadTx(tx.Hash()) {
			continue
		}
		for _, txIn := range tx.Transaction().TxIn {
			// It is acceptable for a transaction input to reference
			// the output of another transaction in this block only
			// if the referenced transaction comes before the
			// current one in this block.  Add the outputs of the
			// referenced transaction as available utxos when this
			// is the case.  Otherwise, the utxo details are still
			// needed.
			//
			// NOTE: The >= is correct here because i is one less
			// than the actual position of the transaction within
			// the block due to skipping the coinbase.
			originHash := &txIn.PreviousOut.Hash
			if bc.IsBadTx(originHash) {
				bc.AddBadTx(tx.Hash(), block.Hash())
				break
			}
			if inFlightIndex, ok := txInFlight[*originHash]; ok &&
				i >= inFlightIndex {

				originTx := transactions[inFlightIndex]
				//TODO, remove type conversion
				view.AddTxOuts(originTx, int64(block.Order()), uint32(i+1))
				continue
			}

			// Don't request entries that are already in the view
			// from the database.
			if _, ok := view.entries[*originHash]; ok {
				continue
			}

			txNeededSet[*originHash] = struct{}{}
		}
	}

	// Request the input utxos from the database.
	return view.fetchUtxosMain(db, txNeededSet)

}

// connectTransaction updates the view by adding all new utxos created by the
// passed transaction and marking all utxos that the transactions spend as
// spent.  In addition, when the 'stxos' argument is not nil, it will be updated
// to append an entry for each spent txout.  An error will be returned if the
// view does not contain the required utxos.
func (view *UtxoViewpoint) connectTransaction(tx *types.Tx, blockOrder uint64, blockIndex uint32, stxos *[]SpentTxOut) error {
	msgTx := tx.Transaction()
	// Coinbase transactions don't have any inputs to spend.
	if msgTx.IsCoinBaseTx() {
		// Add the transaction's outputs as available utxos.
		view.AddTxOuts(tx, int64(blockOrder), blockIndex) //TODO, remove type conversion
		return nil
	}

	// Spend the referenced utxos by marking them spent in the view and,
	// if a slice was provided for the spent txout details, append an entry
	// to it.
	for inIndex, txIn := range msgTx.TxIn {

		originIndex := txIn.PreviousOut.OutIndex
		entry := view.entries[txIn.PreviousOut.Hash]

		// Ensure the referenced utxo exists in the view.  This should
		// never happen unless there is a bug is introduced in the code.
		if entry == nil {
			return AssertError(fmt.Sprintf("view missing input %v",
				txIn.PreviousOut))
		}
		entry.SpendOutput(originIndex)

		// Don't create the stxo details if not requested.
		if stxos == nil {
			continue
		}

		// Populate the stxo details using the utxo entry.  When the
		// transaction is fully spent, set the additional stxo fields
		// accordingly since those details will no longer be available
		// in the utxo set.
		var stxo = SpentTxOut{
			amount:        entry.AmountByIndex(originIndex),
			scriptVersion: entry.ScriptVersionByIndex(originIndex),
			pkScript:      entry.PkScriptByIndex(originIndex),
			txIndex:       blockIndex,
			inIndex:       uint32(inIndex),
		}
		stxo.txVersion = entry.TxVersion()
		stxo.order = uint32(entry.BlockOrder())
		stxo.isCoinBase = entry.IsCoinBase()
		stxo.hasExpiry = entry.HasExpiry()
		stxo.txType = entry.txType
		stxo.txFullySpent = entry.IsFullySpent()

		// Append the entry to the provided spent txouts slice.
		*stxos = append(*stxos, stxo)
	}

	// Add the transaction's outputs as available utxos.
	view.AddTxOuts(tx, int64(blockOrder), blockIndex) //TODO, remove type conversion

	return nil
}

// disconnectTransactions updates the view by removing all of the transactions
// created by the passed block, restoring all utxos the transactions spent by
// using the provided spent txo information, and setting the best hash for the
// view to the block before the passed block.
//
// This function will ONLY work correctly for a single transaction tree at a
// time because of index tracking.
func (b *BlockChain) disconnectTransactions(view *UtxoViewpoint, block *types.SerializedBlock, stxos []SpentTxOut) error {

	transactions := block.Transactions()
	for txIdx := len(transactions) - 1; txIdx > -1; txIdx-- {
		tx := transactions[txIdx]

		// Clear this transaction from the view if it already exists or
		// create a new empty entry for when it does not.  This is done
		// because the code relies on its existence in the view in order
		// to signal modifications have happened.
		isCoinbase := txIdx == 0
		entry := view.entries[*tx.Hash()]
		if entry == nil {
			entry = newUtxoEntry(tx.Transaction().Version,
				uint32(block.Order()), uint32(txIdx), isCoinbase,
				tx.Transaction().Expire != 0, types.TxTypeRegular)
			view.entries[*tx.Hash()] = entry
		}
		entry.modified = true
		entry.sparseOutputs = make(map[uint32]*utxoOutput)

		// Loop backwards through all of the transaction inputs (except
		// for the coinbase which has no inputs) and unspend the
		// referenced txos.  This is necessary to match the order of the
		// spent txout entries.
		if isCoinbase {
			continue
		}
		for txInIdx := len(tx.Transaction().TxIn) - 1; txInIdx > -1; txInIdx-- {
			// Ensure the spent txout index is decremented to stay
			// in sync with the transaction input.
			stxo:=GetSpentTxOut(uint(txIdx),uint(txInIdx),stxos)
			if stxo == nil {
				continue
			}
			// When there is not already an entry for the referenced
			// transaction in the view, it means it was fully spent,
			// so create a new utxo entry in order to resurrect it.
			txIn := tx.Transaction().TxIn[txInIdx]
			originHash := &txIn.PreviousOut.Hash
			originIndex := txIn.PreviousOut.OutIndex
			entry := view.entries[*originHash]
			if entry == nil {
				if !stxo.txFullySpent {
					return AssertError(fmt.Sprintf("tried to "+
						"revive utx %v from non-fully spent stx entry",
						originHash))
				}
				entry = newUtxoEntry(tx.Transaction().Version,
					stxo.order, stxo.txIndex, stxo.isCoinBase,
					stxo.hasExpiry, stxo.txType)
				view.entries[*originHash] = entry
			}

			// Mark the entry as modified since it is either new
			// or will be changed below.
			entry.modified = true

			// Restore the specific utxo using the stxo data from
			// the spend journal if it doesn't already exist in the
			// view.
			output, ok := entry.sparseOutputs[originIndex]
			if !ok {
				// Add the unspent transaction output.
				entry.sparseOutputs[originIndex] = &utxoOutput{
					spent:         false,
					amount:        stxo.amount,
					scriptVersion: stxo.scriptVersion,
					pkScript:      stxo.pkScript,
				}
				continue
			}

			// Mark the existing referenced transaction output as
			// unspent.
			output.spent = false
		}
	}

	// Update the best hash for view to the previous block since all of the
	// transactions for the current block have been disconnected.
	view.SetBestHash(block.Hash())
	return nil
}

// connectTransactions updates the view by adding all new utxos created by all
// of the transactions in the passed block, marking all utxos the transactions
// spend as spent, and setting the best hash for the view to the passed block.
// In addition, when the 'stxos' argument is not nil, it will be updated to
// append an entry for each spent txout.
func (b *BlockChain) connectTransactions(view *UtxoViewpoint, block, parent *types.SerializedBlock, stxos *[]SpentTxOut) error {

	if parent != nil && block.Order() != 0 {
		err := view.fetchInputUtxos(b.db, block, b)
		if err != nil {
			return err
		}
		for i, tx := range parent.Transactions() {
			err := view.connectTransaction(tx, parent.Order(), uint32(i),
				stxos)
			if err != nil {
				return err
			}
		}
	}

	err := view.fetchInputUtxos(b.db, block, b)
	if err != nil {
		return err
	}

	// Update the best hash for view to include this block since all of its
	// transactions have been connected.
	view.SetBestHash(block.Hash())
	return nil
}

// commit prunes all entries marked modified that are now fully spent and marks
// all entries as unmodified.
func (view *UtxoViewpoint) commit() {
	for txHash, entry := range view.entries {
		if entry == nil || (entry.modified && entry.IsFullySpent()) {
			delete(view.entries, txHash)
			continue
		}

		entry.modified = false
	}
}

// fetchUtxos loads utxo details about provided set of transaction hashes into
// the view from the database as needed unless they already exist in the view in
// which case they are ignored.
func (view *UtxoViewpoint) fetchUtxos(db database.DB, txSet map[hash.Hash]struct{}) error {
	// Nothing to do if there are no requested hashes.
	if len(txSet) == 0 {
		return nil
	}

	// Filter entries that are already in the view.
	txNeededSet := make(map[hash.Hash]struct{})
	for hash := range txSet {
		// Already loaded into the current view.
		if _, ok := view.entries[hash]; ok {
			continue
		}

		txNeededSet[hash] = struct{}{}
	}

	// Request the input utxos from the database.
	return view.fetchUtxosMain(db, txNeededSet)
}

// disconnectTransactionSlice updates the view by removing all of the transactions
// created by the passed slice of transactions, restoring all utxos the
// transactions spent by using the provided spent txo information, and setting
// the best hash for the view to the block before the passed block.
func (view *UtxoViewpoint) disconnectTransactionSlice(transactions []*types.Tx, height int64, stxosPtr *[]SpentTxOut) (int, error) {
	if stxosPtr == nil {
		return 0, AssertError("passed pointer to non-existing stxos slice")
	}

	stxos := *stxosPtr
	stxoIdx := len(stxos) - 1
	if stxoIdx == -1 {
		return 0, nil
	}
	for txIdx := len(transactions) - 1; txIdx > -1; txIdx-- {
		tx := transactions[txIdx]
		msgTx := tx.Transaction()
		txType := types.DetermineTxType(msgTx)

		// Clear this transaction from the view if it already exists or
		// create a new empty entry for when it does not.  This is done
		// because the code relies on its existence in the view in order
		// to signal modifications have happened.
		isCoinbase := txIdx == 0
		entry := view.entries[*tx.Hash()]
		if entry == nil {
			entry = newUtxoEntry(msgTx.Version, uint32(height),
				uint32(txIdx), msgTx.IsCoinBaseTx(), msgTx.Expire != 0, txType)
			view.entries[*tx.Hash()] = entry
		}
		entry.modified = true
		entry.sparseOutputs = make(map[uint32]*utxoOutput)

		// Loop backwards through all of the transaction inputs (except
		// for the coinbase which has no inputs) and unspend the
		// referenced txos.  This is necessary to match the order of the
		// spent txout entries.
		if isCoinbase {
			continue
		}
		for txInIdx := len(msgTx.TxIn) - 1; txInIdx > -1; txInIdx-- {
			// Ensure the spent txout index is decremented to stay
			// in sync with the transaction input.
			stxo := &stxos[stxoIdx]
			stxoIdx--

			// When there is not already an entry for the referenced
			// transaction in the view, it means it was fully spent,
			// so create a new utxo entry in order to resurrect it.
			txIn := msgTx.TxIn[txInIdx]
			originHash := &txIn.PreviousOut.Hash
			originInIndex := txIn.PreviousOut.OutIndex
			//originHeight := txIn.BlockHeight
			// originIndex := txIn.BlockIndex
			entry := view.entries[*originHash]
			if entry == nil {
				entry = newUtxoEntry(stxo.txVersion, stxo.order,
					stxo.txIndex, stxo.isCoinBase, stxo.hasExpiry,
					stxo.txType)
				view.entries[*originHash] = entry
			}

			// Mark the entry as modified since it is either new
			// or will be changed below.
			entry.modified = true

			// Restore the specific utxo using the stxo data from
			// the spend journal if it doesn't already exist in the
			// view.
			output, ok := entry.sparseOutputs[originInIndex]
			if !ok {
				// Add the unspent transaction output.
				entry.sparseOutputs[originInIndex] = &utxoOutput{
					spent:         false,
					amount:        txIn.AmountIn,
					scriptVersion: stxo.scriptVersion,
					pkScript:      stxo.pkScript,
				}
				continue
			}

			// Mark the existing referenced transaction output as
			// unspent.
			output.spent = false
		}
	}

	return stxoIdx + 1, nil
}

// GetSpentTxOut can return the spent transaction out
func GetSpentTxOut(txIndex uint,inIndex uint,stxos []SpentTxOut) *SpentTxOut {
	if len(stxos)==0 {
		return nil
	}
	var result SpentTxOut
	for _,stxo:=range stxos {
		if stxo.txIndex==uint32(txIndex) && stxo.inIndex==uint32(inIndex) {
			result=stxo
			break
		}
	}
	return &result
}