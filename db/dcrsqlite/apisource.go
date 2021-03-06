// Copyright (c) 2018-2019, The Decred developers
// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package dcrsqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/decred/dcrd/blockchain/stake"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrjson/v2"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/rpcclient/v2"
	"github.com/decred/dcrd/wire"
	apitypes "github.com/decred/dcrdata/api/types"
	"github.com/decred/dcrdata/db/dbtypes"
	"github.com/decred/dcrdata/rpcutils"
	"github.com/decred/dcrdata/txhelpers"
	exptypes "github.com/decred/dcrdata/v4/explorer/types"
	"github.com/decred/dcrdata/v4/mempool"
	"github.com/decred/dcrdata/v4/stakedb"
	humanize "github.com/dustin/go-humanize"
)

const (
	MaxAddressRows int64 = 1000
)

// WiredDB is intended to satisfy DataSourceLite interface. The block header is
// not stored in the DB, so the RPC client is used to get it on demand.
type WiredDB struct {
	*DBDataSaver
	MPC      *mempool.MempoolDataCache
	client   *rpcclient.Client
	params   *chaincfg.Params
	sDB      *stakedb.StakeDatabase
	waitChan chan chainhash.Hash
}

func newWiredDB(DB *DB, statusC chan uint32, cl *rpcclient.Client,
	p *chaincfg.Params, datadir string) (*WiredDB, func() error) {
	// Initialize the block summary cache.
	DB.BlockCache = apitypes.NewAPICache(1e4)

	wDB := &WiredDB{
		DBDataSaver: &DBDataSaver{DB, statusC},
		MPC:         new(mempool.MempoolDataCache),
		client:      cl,
		params:      p,
	}

	var err error
	var height int64
	wDB.sDB, height, err = stakedb.NewStakeDatabase(cl, p, datadir)
	if err != nil {
		log.Errorf("Unable to create stake DB: %v", err)
		if height >= 0 {
			log.Infof("Attempting to recover stake DB...")
			wDB.sDB, err = stakedb.LoadAndRecover(cl, p, datadir, height-288)
		}
		if err != nil {
			if wDB.sDB != nil {
				_ = wDB.sDB.Close()
			}
			log.Errorf("StakeDatabase recovery failed: %v", err)
			return wDB, func() error { return nil }
		}
	}
	return wDB, wDB.sDB.Close
}

// NewWiredDB creates a new WiredDB from a *sql.DB, a node client, network
// parameters, and a status update channel. It calls dcrsqlite.NewDB to create a
// new DB that wrapps the sql.DB.
func NewWiredDB(db *sql.DB, statusC chan uint32, cl *rpcclient.Client,
	p *chaincfg.Params, datadir string, shutdown func()) (*WiredDB, func() error, error) {
	// Create the sqlite.DB
	DB, err := NewDB(db, shutdown)
	if err != nil || DB == nil {
		return nil, func() error { return nil }, err
	}
	// Create the WiredDB
	wDB, cleanup := newWiredDB(DB, statusC, cl, p, datadir)
	if wDB.sDB == nil {
		err = fmt.Errorf("failed to create StakeDatabase")
	}
	return wDB, cleanup, err
}

// InitWiredDB creates a new WiredDB from a file containing the data for a
// sql.DB. The other parameters are same as those for NewWiredDB.
func InitWiredDB(dbInfo *DBInfo, statusC chan uint32, cl *rpcclient.Client,
	p *chaincfg.Params, datadir string, shutdown func()) (*WiredDB, func() error, error) {
	db, err := InitDB(dbInfo, shutdown)
	if err != nil {
		return nil, func() error { return nil }, err
	}

	wDB, cleanup := newWiredDB(db, statusC, cl, p, datadir)
	if wDB.sDB == nil {
		err = fmt.Errorf("failed to create StakeDatabase")
	}
	return wDB, cleanup, err
}

func (db *WiredDB) EnableCache() {
	db.BlockCache.Enable()
}

func (db *WiredDB) DisableCache() {
	db.BlockCache.Disable()
}

func (db *WiredDB) NewStakeDBChainMonitor(ctx context.Context, wg *sync.WaitGroup,
	reorgChan chan *txhelpers.ReorgData) *stakedb.ChainMonitor {
	return db.sDB.NewChainMonitor(ctx, wg, reorgChan)
}

func (db *WiredDB) ChargePoolInfoCache(startHeight int64) error {
	if startHeight < 0 {
		startHeight = 0
	}
	endHeight, err := db.GetStakeInfoHeight()
	if err != nil {
		return err
	}
	if startHeight > endHeight {
		log.Debug("No pool info to load into cache")
		return nil
	}
	tpis, blockHashes, err := db.DB.RetrievePoolInfoRange(startHeight, endHeight)
	if err != nil {
		return err
	}
	log.Debugf("Pre-loading pool info for %d blocks ([%d, %d]) into cache.",
		len(tpis), startHeight, endHeight)
	for i := range tpis {
		hash, err := chainhash.NewHashFromStr(blockHashes[i])
		if err != nil {
			log.Warnf("Invalid block hash: %s", blockHashes[i])
		}
		db.sDB.SetPoolInfo(*hash, &tpis[i])
	}
	// for i := startHeight; i <= endHeight; i++ {
	// 	winners, blockHash, err := db.DB.RetrieveWinners(i)
	// 	if err != nil {
	// 		return err
	// 	}
	// 	db.sDB.SetPoolInfo(blockHash)
	// }
	return nil
}

// ReportHeights logs the SQLite table heights, and the stake database height,
// returning a non-nil error only when the SQLite tables are not at the same
// height.
func (db *WiredDB) ReportHeights() error {
	// Check and report heights of the DBs. dbHeight is the lowest of the
	// heights, and may be -1 with an empty SQLite DB.
	dbHeight, summaryHeight, stakeInfoHeight, stakeDBHeight, err := db.DBHeights()
	if err != nil {
		return fmt.Errorf("DBHeights failed: %v", err)
	}
	if dbHeight < -1 {
		panic("invalid starting height")
	}

	log.Info("SQLite block summary table height: ", summaryHeight)
	log.Info("SQLite stake info table height:    ", stakeInfoHeight)
	if stakeInfoHeight != summaryHeight {
		err = fmt.Errorf("SQLite database (dcrdata.sqlt.db) is corrupted")
	}
	log.Info("StakeDatabase height:              ", stakeDBHeight)

	return err
}

// CheckConnectivity ensures the db and RPC client are working.
func (db *WiredDB) CheckConnectivity() error {
	var err error
	if err = db.Ping(); err != nil {
		return err
	}
	if err = db.client.Ping(); err != nil {
		return err
	}
	return err
}

// SyncDBAsync is like SyncDB except it also takes a result channel where the
// caller should wait to receive the result. When a slave BlockGetter is in use,
// fetchToHeight is used to indicate at what height the MasterBlockGetter will
// start sending blocks for processing. e.g. When an auxiliary DB owns the
// MasterBlockGetter, fetchToHeight should be one past the best block in the aux
// DB, thus putting WiredDB sync into "catch up" mode where it just pulls blocks
// from RPC until it matches the auxDB height and coordination begins.
func (db *WiredDB) SyncDBAsync(ctx context.Context, res chan dbtypes.SyncResult, blockGetter rpcutils.BlockGetter, fetchToHeight int64,
	updateExplorer chan *chainhash.Hash, barLoad chan *dbtypes.ProgressBarLoad) {
	// Ensure the db is working.
	if err := db.CheckConnectivity(); err != nil {
		res <- dbtypes.SyncResult{
			Height: -1,
			Error:  fmt.Errorf("CheckConnectivity failed: %v", err),
		}
		return
	}
	// Set the first height at which the smart client should wait for the block.
	if !(blockGetter == nil || blockGetter.(*rpcutils.BlockGate) == nil) {
		// Set the first block notification to come on the waitChan to
		// fetchToHeight, which as described above should be set as the first
		// block to be processed (and relayed to this BlockGetter) by the
		// auxiliary DB sync function. e.g. (*ChainDB).SyncChainDB.
		log.Debugf("Setting block gate height to %d", fetchToHeight)
		db.initWaitChan(blockGetter.WaitForHeight(fetchToHeight))
	}
	// Begin sync in a goroutine, and return the result when done on the
	// provided channel.
	go func() {
		height, err := db.resyncDB(ctx, blockGetter, fetchToHeight, updateExplorer, barLoad)
		res <- dbtypes.SyncResult{
			Height: height,
			Error:  err,
		}
	}()
}

// SyncDB is like SyncDBAsync, except it uses synchronous execution (the call to
// resyncDB is a blocking call).
func (db *WiredDB) SyncDB(ctx context.Context, blockGetter rpcutils.BlockGetter, fetchToHeight int64) (int64, error) {
	// Ensure the db is working.
	if err := db.CheckConnectivity(); err != nil {
		return -1, fmt.Errorf("CheckConnectivity failed: %v", err)
	}

	// Set the first height at which the smart client should wait for the block.
	if !(blockGetter == nil || blockGetter.(*rpcutils.BlockGate) == nil) {
		log.Debugf("Setting block gate height to %d", fetchToHeight)
		db.initWaitChan(blockGetter.WaitForHeight(fetchToHeight))
	}
	return db.resyncDB(ctx, blockGetter, fetchToHeight, nil, nil)
}

func (db *WiredDB) GetStakeDB() *stakedb.StakeDatabase {
	return db.sDB
}

func (db *WiredDB) GetHeight() (int64, error) {
	return db.GetBlockSummaryHeight()
}

func (db *WiredDB) GetBestBlockHash() (string, error) {
	hash := db.DBDataSaver.GetBestBlockHash()
	var err error
	if hash == "" {
		err = fmt.Errorf("unable to get best block hash")
	}
	return hash, err
}

// BlockchainInfo retrieves the result of the getblockchaininfo node RPC.
func (db *WiredDB) BlockchainInfo() (*dcrjson.GetBlockChainInfoResult, error) {
	return db.client.GetBlockChainInfo()
}

// PurgeBlocksAboveHeight deletes all data across all tables for the blocks
// above the given height, including side chain blocks. The numbers of blocks
// removed from the block summary table and stake info table are returned.
// PurgeBlocksAboveHeight will not return sql.ErrNoRows, but it will return
// without removing a block if the tables are empty.
func (db *WiredDB) PurgeBlocksAboveHeight(height int64) (NSummaryRows, NStakeInfoRows int64, err error) {
	var summaryHeight, stakeInfoHeight int64
	_, summaryHeight, stakeInfoHeight, _, err = db.DBHeights()
	if err != nil {
		return
	}
	if summaryHeight != stakeInfoHeight {
		log.Warnf("Tables are at different heights. "+
			"Block summary height = %d. Stake info height = %d. Purging anyway!",
			summaryHeight, stakeInfoHeight)
	}

	NSummaryRows, NStakeInfoRows, err = db.DeleteBlocksAboveHeight(height)
	// DeleteBlock should not return sql.ErrNoRows, but check anyway.
	if err == sql.ErrNoRows {
		err = nil
	}
	if err != nil {
		return
	}

	_, summaryHeight, stakeInfoHeight, _, err = db.DBHeights()
	if err != nil {
		return
	}
	if summaryHeight != stakeInfoHeight {
		err = fmt.Errorf("tables are at different heights after the purge! "+
			"Block summary height = %d. Stake info height = %d.",
			summaryHeight, stakeInfoHeight)
		return
	}

	// Rewind stake database to this height.
	var stakeDBHeight int64
	stakeDBHeight, err = db.RewindStakeDB(context.Background(), height, true)
	if err != nil {
		return
	}
	if stakeDBHeight != height {
		err = fmt.Errorf("rewind of StakeDatabase to height %d failed, "+
			"reaching height %d instead", height, stakeDBHeight)
		return
	}

	if height != summaryHeight {
		err = fmt.Errorf("failed to purge to %d, got to %d", height, summaryHeight)
	}

	return
}

// PurgeBlock deletes all data across all tables for the block with the
// specified hash. The numbers of blocks removed from the block summary table
// and stake info table are returned. PurgeBlock will not return sql.ErrNoRows,
// but it may return without removing a block.
func (db *WiredDB) PurgeBlock(hash string) (NSummaryRows, NStakeInfoRows int64, err error) {
	NSummaryRows, NStakeInfoRows, err = db.DeleteBlock(hash)
	// DeleteBlock should not return sql.ErrNoRows, but check anyway.
	if err == sql.ErrNoRows {
		err = nil
	}
	return
}

// PurgeBestBlock deletes all data across all tables for the best block in the
// block summary table. The numbers of blocks removed from the block summary
// table and stake info table are returned. PurgeBestBlock will not return
// sql.ErrNoRows, but it will return without removing a block if the tables are
// empty. The returned height and hash values represent the best block after
// successful data removal, or before a failed removal attempt.
func (db *WiredDB) PurgeBestBlock() (NSummaryRows, NStakeInfoRows, height int64, hash string, err error) {
	var summaryHeight, stakeInfoHeight int64
	_, summaryHeight, stakeInfoHeight, _, err = db.DBHeights()
	if err != nil {
		return
	}
	if summaryHeight != stakeInfoHeight {
		err = fmt.Errorf("tables are at different heights. "+
			"Block summary height = %d. Stake info height = %d.",
			summaryHeight, stakeInfoHeight)
		return
	}

	var h chainhash.Hash
	h, height, err = db.GetBestBlockHeightHash()
	if err != nil {
		if err == sql.ErrNoRows {
			log.Warnf("No blocks to remove from SQLite summary table.")
			err = nil
		}
		return
	}
	hash = h.String()

	NSummaryRows, NStakeInfoRows, err = db.PurgeBlock(hash)
	if err != nil {
		return
	}

	h, height, err = db.GetBestBlockHeightHash()
	if err != nil {
		if err == sql.ErrNoRows {
			// Last block removed.
			err = nil
		}
		hash = ""
		return
	}
	hash = h.String()

	return
}

// PurgeBestBlocks deletes all data across all tables for the N best blocks in
// the block summary table. The number of blocks removed is returned.
// PurgeBestBlocks will not return sql.ErrNoRows, but it will return without
// removing the requested number of blocks if the tables are empty or become
// empty. The returned height and hash values represent the best block after
// successful data removal, or before a failed removal attempt.
func (db *WiredDB) PurgeBestBlocks(N int64) (NSummaryRows, NStakeInfoRows, height int64, hash string, err error) {
	// If N is less than 1, get the current best block height and hash, then
	// return.
	if N < 1 {
		var h chainhash.Hash
		h, height, err = db.GetBestBlockHeightHash()
		if err == sql.ErrNoRows {
			err = nil
		}
		hash = h.String()
		return
	}

	for i := int64(0); i < N; i++ {
		// Attempt removal of the best block.
		var NSumi, Nstakei, heighti int64
		var hashi string
		NSumi, Nstakei, heighti, hashi, err = db.PurgeBestBlock()
		if err != nil {
			// Return with previous (or initial) best block info and block
			// removal count.
			return
		}

		if (i%100 == 0 && i > 0) || i == N-1 {
			log.Debugf("Removed data for %d blocks.", i+1)
		}

		// Removal succeeded. Returned best block values are valid.
		NSummaryRows += NSumi
		NStakeInfoRows += Nstakei
		height = heighti
		hash = hashi

		// Rewind stake database to this height.
		var stakeDBHeight int64
		stakeDBHeight, err = db.RewindStakeDB(context.Background(), height, true)
		if err != nil {
			return
		}
		if stakeDBHeight != height {
			err = fmt.Errorf("rewind of StakeDatabase to height %d failed, "+
				"reaching height %d instead", height, stakeDBHeight)
			return
		}
	}

	return
}

// GetBestBlockHeightHash retrieves the DB's best block hash and height.
func (db *WiredDB) GetBestBlockHeightHash() (chainhash.Hash, int64, error) {
	bestBlockSummary := db.GetBestBlockSummary()
	if bestBlockSummary == nil {
		return chainhash.Hash{}, -1, fmt.Errorf("unable to retrieve best block summary")
	}
	height := int64(bestBlockSummary.Height)
	hash, err := chainhash.NewHashFromStr(bestBlockSummary.Hash)
	return *hash, height, err
}

func (db *WiredDB) GetChainParams() *chaincfg.Params {
	return db.params
}
func (db *WiredDB) GetBlockHash(idx int64) (string, error) {
	hash, err := db.RetrieveBlockHash(idx)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Errorf("Unable to get block hash for block number %d: %v", idx, err)
		}
		return "", err
	}
	return hash, nil
}

func (db *WiredDB) GetBlockHeight(hash string) (int64, error) {
	height, err := db.RetrieveBlockHeight(hash)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Errorf("Unable to get block height for hash %s: %v", hash, err)
		}
		return -1, err
	}
	return height, nil
}

func (db *WiredDB) GetHeader(idx int) *dcrjson.GetBlockHeaderVerboseResult {
	return rpcutils.GetBlockHeaderVerbose(db.client, int64(idx))
}

func (db *WiredDB) GetBlockVerbose(idx int, verboseTx bool) *dcrjson.GetBlockVerboseResult {
	return rpcutils.GetBlockVerbose(db.client, int64(idx), verboseTx)
}

func (db *WiredDB) GetBlockVerboseByHash(hash string, verboseTx bool) *dcrjson.GetBlockVerboseResult {
	return rpcutils.GetBlockVerboseByHash(db.client, hash, verboseTx)
}

func (db *WiredDB) GetBlockByHash(hash string) (*wire.MsgBlock, error) {
	blockHash, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		log.Errorf("Invalid block hash %s", hash)
		return nil, err
	}
	return db.client.GetBlock(blockHash)
}

func (db *WiredDB) GetBlockHeaderByHash(hash string) (*wire.BlockHeader, error) {
	blockHash, err := chainhash.NewHashFromStr(hash)
	if err != nil {
		log.Errorf("Invalid block hash %s", hash)
		return nil, err
	}
	return db.client.GetBlockHeader(blockHash)
}

func (db *WiredDB) CoinSupply() (supply *apitypes.CoinSupply) {
	coinSupply, err := db.client.GetCoinSupply()
	if err != nil {
		log.Errorf("RPC failure (GetCoinSupply): %v", err)
		return
	}

	hash, height, err := db.client.GetBestBlock()
	if err != nil {
		log.Errorf("RPC failure (GetBestBlock): %v", err)
		return
	}

	return &apitypes.CoinSupply{
		Height:   height,
		Hash:     hash.String(),
		Mined:    int64(coinSupply),
		Ultimate: txhelpers.UltimateSubsidy(db.params),
	}
}

func (db *WiredDB) BlockSubsidy(height int64, voters uint16) *dcrjson.GetBlockSubsidyResult {
	blockSubsidy, err := db.client.GetBlockSubsidy(height, voters)
	if err != nil {
		return nil
	}
	return blockSubsidy
}

func (db *WiredDB) GetTransactionsForBlock(idx int64) *apitypes.BlockTransactions {
	blockVerbose := rpcutils.GetBlockVerbose(db.client, idx, false)

	return makeBlockTransactions(blockVerbose)
}

func (db *WiredDB) GetTransactionsForBlockByHash(hash string) *apitypes.BlockTransactions {
	blockVerbose := rpcutils.GetBlockVerboseByHash(db.client, hash, false)

	return makeBlockTransactions(blockVerbose)
}

func makeBlockTransactions(blockVerbose *dcrjson.GetBlockVerboseResult) *apitypes.BlockTransactions {
	blockTransactions := new(apitypes.BlockTransactions)

	blockTransactions.Tx = make([]string, len(blockVerbose.Tx))
	copy(blockTransactions.Tx, blockVerbose.Tx)

	blockTransactions.STx = make([]string, len(blockVerbose.STx))
	copy(blockTransactions.STx, blockVerbose.STx)

	return blockTransactions
}

func (db *WiredDB) GetAllTxIn(txid *chainhash.Hash) []*apitypes.TxIn {
	tx, err := db.client.GetRawTransaction(txid)
	if err != nil {
		log.Errorf("Unknown transaction %s", txid)
		return nil
	}

	allTxIn0 := tx.MsgTx().TxIn
	allTxIn := make([]*apitypes.TxIn, len(allTxIn0))
	for i := range allTxIn {
		txIn := &apitypes.TxIn{
			PreviousOutPoint: apitypes.OutPoint{
				Hash:  allTxIn0[i].PreviousOutPoint.Hash.String(),
				Index: allTxIn0[i].PreviousOutPoint.Index,
				Tree:  allTxIn0[i].PreviousOutPoint.Tree,
			},
			Sequence:        allTxIn0[i].Sequence,
			ValueIn:         dcrutil.Amount(allTxIn0[i].ValueIn).ToCoin(),
			BlockHeight:     allTxIn0[i].BlockHeight,
			BlockIndex:      allTxIn0[i].BlockIndex,
			SignatureScript: hex.EncodeToString(allTxIn0[i].SignatureScript),
		}
		allTxIn[i] = txIn
	}

	return allTxIn
}

func (db *WiredDB) GetAllTxOut(txid *chainhash.Hash) []*apitypes.TxOut {
	tx, err := db.client.GetRawTransactionVerbose(txid)
	if err != nil {
		log.Warnf("Unknown transaction %s", txid)
		return nil
	}

	txouts := tx.Vout
	allTxOut := make([]*apitypes.TxOut, 0, len(txouts))
	for i := range txouts {
		// dcrjson.Vout and apitypes.TxOut are the same except for N.
		spk := &tx.Vout[i].ScriptPubKey
		// If the script type is not recognized by apitypes, the ScriptClass
		// types may need to be updated to match dcrd.
		if spk.Type != "invalid" && !apitypes.IsValidScriptClass(spk.Type) {
			log.Warnf(`The ScriptPubKey's type "%s" is not known to dcrdata! ` +
				`Update apitypes or debug dcrd.`)
		}
		allTxOut = append(allTxOut, &apitypes.TxOut{
			Value:   txouts[i].Value,
			Version: txouts[i].Version,
			ScriptPubKeyDecoded: apitypes.ScriptPubKey{
				Asm:       spk.Asm,
				Hex:       spk.Hex,
				ReqSigs:   spk.ReqSigs,
				Type:      spk.Type,
				Addresses: spk.Addresses,
				CommitAmt: spk.CommitAmt,
			},
		})
	}

	return allTxOut
}

// GetRawTransactionWithPrevOutAddresses looks up the previous outpoints for a
// transaction and extracts a slice of addresses encoded by the pkScript for
// each previous outpoint consumed by the transaction.
func (db *WiredDB) GetRawTransactionWithPrevOutAddresses(txid *chainhash.Hash) (*apitypes.Tx, [][]string) {
	tx, _ := db.getRawTransaction(txid)
	if tx == nil {
		return nil, nil
	}

	prevOutAddresses := make([][]string, len(tx.Vin))

	for i := range tx.Vin {
		vin := &tx.Vin[i]
		// Skip inspecting previous outpoint for coinbase transaction, and
		// vin[0] of stakebase transcation.
		if vin.IsCoinBase() || (vin.IsStakeBase() && i == 0) {
			continue
		}
		var err error
		prevOutAddresses[i], err = txhelpers.OutPointAddressesFromString(
			vin.Txid, vin.Vout, vin.Tree, db.client, db.params)
		if err != nil {
			log.Warnf("failed to get outpoint address from txid: %v", err)
		}
	}

	return tx, prevOutAddresses
}

func (db *WiredDB) GetRawTransaction(txid *chainhash.Hash) *apitypes.Tx {
	tx, _ := db.getRawTransaction(txid)
	return tx
}

func (db *WiredDB) GetTransactionHex(txid *chainhash.Hash) string {
	_, hex := db.getRawTransaction(txid)
	return hex
}

func (db *WiredDB) DecodeRawTransaction(txhex string) (*dcrjson.TxRawResult, error) {
	bytes, err := hex.DecodeString(txhex)
	if err != nil {
		log.Errorf("DecodeRawTransaction failed: %v", err)
		return nil, err
	}
	tx, err := db.client.DecodeRawTransaction(bytes)
	if err != nil {
		log.Errorf("DecodeRawTransaction failed: %v", err)
		return nil, err
	}
	return tx, nil
}

func (db *WiredDB) SendRawTransaction(txhex string) (string, error) {
	msg, err := txhelpers.MsgTxFromHex(txhex)
	if err != nil {
		log.Errorf("SendRawTransaction failed: could not decode tx")
		return "", err
	}
	hash, err := db.client.SendRawTransaction(msg, true)
	if err != nil {
		log.Errorf("SendRawTransaction failed: %v", err)
		return "", err
	}
	return hash.String(), err
}

func (db *WiredDB) GetTrimmedTransaction(txid *chainhash.Hash) *apitypes.TrimmedTx {
	tx, _ := db.getRawTransaction(txid)
	if tx == nil {
		return nil
	}
	return &apitypes.TrimmedTx{
		TxID:     tx.TxID,
		Version:  tx.Version,
		Locktime: tx.Locktime,
		Expiry:   tx.Expiry,
		Vin:      tx.Vin,
		Vout:     tx.Vout,
	}
}

func (db *WiredDB) getRawTransaction(txid *chainhash.Hash) (tx *apitypes.Tx, hex string) {
	var err error
	tx, hex, err = rpcutils.APITransaction(db.client, txid)
	if err != nil {
		log.Errorf("APITransaction failed: %v", err)
	}
	return
}

// GetVoteVersionInfo requests stake version info from the dcrd RPC server
func (db *WiredDB) GetVoteVersionInfo(ver uint32) (*dcrjson.GetVoteInfoResult, error) {
	return db.client.GetVoteInfo(ver)
}

// GetStakeVersions requests the output of the getstakeversions RPC, which gets
// stake version information and individual vote version information starting at the
// given block and for count-1 blocks prior.
func (db *WiredDB) GetStakeVersions(txHash string, count int32) (*dcrjson.GetStakeVersionsResult, error) {
	return db.client.GetStakeVersions(txHash, count)
}

// GetStakeVersionsLatest requests the output of the getstakeversions RPC for
// just the current best block.
func (db *WiredDB) GetStakeVersionsLatest() (*dcrjson.StakeVersions, error) {
	txHash, err := db.GetBestBlockHash()
	if err != nil {
		return nil, err
	}
	stkVers, err := db.GetStakeVersions(txHash, 1)
	if err != nil || stkVers == nil || len(stkVers.StakeVersions) == 0 {
		return nil, err
	}
	stkVer := stkVers.StakeVersions[0]
	return &stkVer, nil
}

// GetVoteInfo attempts to decode the vote bits of a SSGen transaction. If the
// transaction is not a valid SSGen, the VoteInfo output will be nil. Depending
// on the stake version with which dcrdata is compiled with (chaincfg.Params),
// the Choices field of VoteInfo may be a nil slice even if the votebits were
// set for a previously-valid agenda.
func (db *WiredDB) GetVoteInfo(txhash *chainhash.Hash) (*apitypes.VoteInfo, error) {
	tx, err := db.client.GetRawTransaction(txhash)
	if err != nil {
		log.Errorf("GetRawTransaction failed for: %v", txhash)
		return nil, nil
	}

	validation, version, bits, choices, err := txhelpers.SSGenVoteChoices(tx.MsgTx(), db.params)
	if err != nil {
		return nil, err
	}
	vinfo := &apitypes.VoteInfo{
		Validation: apitypes.BlockValidation{
			Hash:     validation.Hash.String(),
			Height:   validation.Height,
			Validity: validation.Validity,
		},
		Version: version,
		Bits:    bits,
		Choices: choices,
	}
	return vinfo, nil
}

func (db *WiredDB) GetStakeDiffEstimates() *apitypes.StakeDiff {
	sd := rpcutils.GetStakeDiffEstimates(db.client)

	height := db.MPC.GetHeight()
	winSize := uint32(db.params.StakeDiffWindowSize)
	sd.IdxBlockInWindow = int(height%winSize) + 1
	sd.PriceWindowNum = int(height / winSize)

	return sd
}

func (db *WiredDB) GetFeeInfo(idx int) *dcrjson.FeeInfoBlock {
	stakeInfo, err := db.RetrieveStakeInfoExtended(int64(idx))
	if err != nil {
		log.Errorf("Unable to retrieve stake info: %v", err)
		return nil
	}

	return &stakeInfo.Feeinfo
}

func (db *WiredDB) GetStakeInfoExtendedByHeight(idx int) *apitypes.StakeInfoExtended {
	stakeInfo, err := db.RetrieveStakeInfoExtended(int64(idx))
	if err != nil {
		log.Errorf("Unable to retrieve stake info: %v", err)
		return nil
	}

	return stakeInfo
}

func (db *WiredDB) GetStakeInfoExtendedByHash(blockhash string) *apitypes.StakeInfoExtended {
	stakeInfo, err := db.RetrieveStakeInfoExtendedByHash(blockhash)
	if err != nil {
		log.Errorf("Unable to retrieve stake info: %v", err)
		return nil
	}

	return stakeInfo
}

func (db *WiredDB) GetSummary(idx int) *apitypes.BlockDataBasic {
	blockSummary, err := db.RetrieveBlockSummary(int64(idx))
	if err != nil {
		log.Errorf("Unable to retrieve block summary: %v", err)
		return nil
	}

	return blockSummary
}

func (db *WiredDB) GetSummaryByHash(hash string, withTxTotals bool) *apitypes.BlockDataBasic {
	blockSummary, err := db.RetrieveBlockSummaryByHash(hash)
	if err != nil {
		log.Errorf("Unable to retrieve block summary: %v", err)
		return nil
	}

	if withTxTotals {
		data := db.GetBlockVerboseByHash(hash, true)
		if data == nil {
			log.Error("Unable to get block for block hash " + hash)
			return nil
		}

		var totalFees, totalOut dcrutil.Amount
		for i := range data.RawTx {
			msgTx, err := txhelpers.MsgTxFromHex(data.RawTx[i].Hex)
			if err != nil {
				log.Errorf("Unable to decode transaction: %v", err)
				return nil
			}
			// Do not compute fee for coinbase transaction.
			if !data.RawTx[i].Vin[0].IsCoinBase() {
				fee, _ := txhelpers.TxFeeRate(msgTx)
				totalFees += fee
			}
			totalOut += txhelpers.TotalOutFromMsgTx(msgTx)
		}
		for i := range data.RawSTx {
			msgTx, err := txhelpers.MsgTxFromHex(data.RawSTx[i].Hex)
			if err != nil {
				log.Errorf("Unable to decode transaction: %v", err)
				return nil
			}
			fee, _ := txhelpers.TxFeeRate(msgTx)
			totalFees += fee
			totalOut += txhelpers.TotalOutFromMsgTx(msgTx)
		}

		miningFee := int64(totalFees)
		blockSummary.MiningFee = &miningFee
		totalSent := int64(totalOut)
		blockSummary.TotalSent = &totalSent
	}

	return blockSummary
}

// GetBestBlockSummary retrieves data for the best block in the DB. If there are
// no blocks in the table (yet), a nil pointer is returned.
func (db *WiredDB) GetBestBlockSummary() *apitypes.BlockDataBasic {
	// Attempt to retrieve height of best block in DB.
	dbBlkHeight, err := db.GetBlockSummaryHeight()
	if err != nil {
		log.Errorf("GetBlockSummaryHeight failed: %v", err)
		return nil
	}

	// Empty table is not an error.
	if dbBlkHeight == -1 {
		return nil
	}

	// Retrieve the block data.
	blockSummary, err := db.RetrieveBlockSummary(dbBlkHeight)
	if err != nil {
		log.Errorf("Unable to retrieve block %d summary: %v", dbBlkHeight, err)
		return nil
	}

	return blockSummary
}

func (db *WiredDB) GetBlockSize(idx int) (int32, error) {
	blockSize, err := db.RetrieveBlockSize(int64(idx))
	if err != nil {
		log.Errorf("Unable to retrieve block %d size: %v", idx, err)
		return -1, err
	}
	return blockSize, nil
}

func (db *WiredDB) GetBlockSizeRange(idx0, idx1 int) ([]int32, error) {
	blockSizes, err := db.RetrieveBlockSizeRange(int64(idx0), int64(idx1))
	if err != nil {
		log.Errorf("Unable to retrieve block size range: %v", err)
		return nil, err
	}
	return blockSizes, nil
}

func (db *WiredDB) GetPool(idx int64) ([]string, error) {
	hs, err := db.sDB.PoolDB.Pool(idx)
	if err != nil {
		log.Errorf("Unable to get ticket pool from stakedb: %v", err)
		return nil, err
	}
	hss := make([]string, 0, len(hs))
	for i := range hs {
		hss = append(hss, hs[i].String())
	}
	return hss, nil
}

func (db *WiredDB) GetPoolByHash(hash string) ([]string, error) {
	idx, err := db.GetBlockHeight(hash)
	if err != nil {
		log.Errorf("Unable to retrieve block height for hash %s: %v", hash, err)
		return nil, err
	}
	hs, err := db.sDB.PoolDB.Pool(idx)
	if err != nil {
		log.Errorf("Unable to get ticket pool from stakedb: %v", err)
		return nil, err
	}
	hss := make([]string, 0, len(hs))
	for i := range hs {
		hss = append(hss, hs[i].String())
	}
	return hss, nil
}

// GetBlockSummaryTimeRange returns the blocks created within a specified time
// range min, max time
func (db *WiredDB) GetBlockSummaryTimeRange(min, max int64, limit int) []apitypes.BlockDataBasic {
	blockSummary, err := db.RetrieveBlockSummaryByTimeRange(min, max, limit)
	if err != nil {
		log.Errorf("Unable to retrieve block summary using time %d: %v", min, err)
	}
	return blockSummary
}

func (db *WiredDB) GetPoolInfo(idx int) *apitypes.TicketPoolInfo {
	ticketPoolInfo, err := db.RetrievePoolInfo(int64(idx))
	if err != nil {
		log.Errorf("Unable to retrieve ticket pool info: %v", err)
		return nil
	}
	return ticketPoolInfo
}

func (db *WiredDB) GetPoolInfoByHash(hash string) *apitypes.TicketPoolInfo {
	ticketPoolInfo, err := db.RetrievePoolInfoByHash(hash)
	if err != nil {
		log.Errorf("Unable to retrieve ticket pool info: %v", err)
		return nil
	}
	return ticketPoolInfo
}

func (db *WiredDB) GetPoolInfoRange(idx0, idx1 int) []apitypes.TicketPoolInfo {
	ticketPoolInfos, _, err := db.RetrievePoolInfoRange(int64(idx0), int64(idx1))
	if err != nil {
		log.Errorf("Unable to retrieve ticket pool info range: %v", err)
		return nil
	}
	return ticketPoolInfos
}

func (db *WiredDB) GetPoolValAndSizeRange(idx0, idx1 int) ([]float64, []float64) {
	poolvals, poolsizes, err := db.RetrievePoolValAndSizeRange(int64(idx0), int64(idx1))
	if err != nil {
		log.Errorf("Unable to retrieve ticket value and size range: %v", err)
		return nil, nil
	}
	return poolvals, poolsizes
}

// GetSqliteChartsData fetches the charts data from the sqlite db.
func (db *WiredDB) GetSqliteChartsData() (map[string]*dbtypes.ChartsData, error) {
	poolData, err := db.RetrieveAllPoolValAndSize()
	if err != nil {
		return nil, err
	}

	feeData, err := db.RetrieveBlockFeeInfo()
	if err != nil {
		return nil, err
	}

	var data = map[string]*dbtypes.ChartsData{
		"ticket-pool-size":  {Time: poolData.Time, SizeF: poolData.SizeF},
		"ticket-pool-value": {Time: poolData.Time, ValueF: poolData.ValueF},
		"fee-per-block":     feeData,
	}

	return data, nil
}

func (db *WiredDB) GetSDiff(idx int) float64 {
	sdiff, err := db.RetrieveSDiff(int64(idx))
	if err != nil {
		log.Errorf("Unable to retrieve stake difficulty: %v", err)
		return -1
	}
	return sdiff
}

// RetreiveDifficulty fetches the difficulty value in the last 24hrs or
// immediately after 24hrs.
func (db *WiredDB) RetreiveDifficulty(timestamp int64) float64 {
	sdiff, err := db.RetrieveDiff(timestamp)
	if err != nil {
		log.Errorf("Unable to retrieve difficulty: %v", err)
		return -1
	}
	return sdiff
}

func (db *WiredDB) GetSDiffRange(idx0, idx1 int) []float64 {
	sdiffs, err := db.RetrieveSDiffRange(int64(idx0), int64(idx1))
	if err != nil {
		log.Errorf("Unable to retrieve stake difficulty range: %v", err)
		return nil
	}
	return sdiffs
}

func (db *WiredDB) GetMempoolSSTxSummary() *apitypes.MempoolTicketFeeInfo {
	_, feeInfo := db.MPC.GetFeeInfoExtra()
	return feeInfo
}

func (db *WiredDB) GetMempoolSSTxFeeRates(N int) *apitypes.MempoolTicketFees {
	height, timestamp, totalFees, fees := db.MPC.GetFeeRates(N)
	mpTicketFees := apitypes.MempoolTicketFees{
		Height:   height,
		Time:     timestamp,
		Length:   uint32(len(fees)),
		Total:    uint32(totalFees),
		FeeRates: fees,
	}
	return &mpTicketFees
}

func (db *WiredDB) GetMempoolSSTxDetails(N int) *apitypes.MempoolTicketDetails {
	height, timestamp, totalSSTx, details := db.MPC.GetTicketsDetails(N)
	mpTicketDetails := apitypes.MempoolTicketDetails{
		Height:  height,
		Time:    timestamp,
		Length:  uint32(len(details)),
		Total:   uint32(totalSSTx),
		Tickets: []*apitypes.TicketDetails(details),
	}
	return &mpTicketDetails
}

// GetMempoolPriceCountTime retreives from mempool: the ticket price, the number
// of tickets in mempool, the time of the first ticket.
func (db *WiredDB) GetMempoolPriceCountTime() *apitypes.PriceCountTime {
	return db.MPC.GetTicketPriceCountTime(int(db.params.MaxFreshStakePerBlock))
}

// GetAddressTransactionsWithSkip returns an apitypes.Address Object with at most the
// last count transactions the address was in
func (db *WiredDB) GetAddressTransactionsWithSkip(addr string, count, skip int) *apitypes.Address {
	address, err := dcrutil.DecodeAddress(addr)
	if err != nil {
		log.Infof("Invalid address %s: %v", addr, err)
		return nil
	}
	txs, err := db.client.SearchRawTransactionsVerbose(address, skip, count, false, true, nil)
	if err != nil && err.Error() == "-32603: No Txns available" {
		log.Debugf("GetAddressTransactionsWithSkip: No transactions found for address %s: %v", addr, err)
		return &apitypes.Address{
			Address:      addr,
			Transactions: make([]*apitypes.AddressTxShort, 0), // not nil for JSON formatting
		}
	}
	if err != nil {
		log.Errorf("GetAddressTransactionsWithSkip failed for address %s: %v", addr, err)
		return nil
	}
	tx := make([]*apitypes.AddressTxShort, 0, len(txs))
	for i := range txs {
		tx = append(tx, &apitypes.AddressTxShort{
			TxID:          txs[i].Txid,
			Time:          apitypes.TimeAPI{S: dbtypes.NewTimeDefFromUNIX(txs[i].Time)},
			Value:         txhelpers.TotalVout(txs[i].Vout).ToCoin(),
			Confirmations: int64(txs[i].Confirmations),
			Size:          int32(len(txs[i].Hex) / 2),
		})
	}
	return &apitypes.Address{
		Address:      addr,
		Transactions: tx,
	}

}

// GetAddressTransactions returns an apitypes.Address Object with at most the
// last count transactions the address was in
func (db *WiredDB) GetAddressTransactions(addr string, count int) *apitypes.Address {
	return db.GetAddressTransactionsWithSkip(addr, count, 0)
}

// GetAddressTransactionsRaw returns an array of apitypes.AddressTxRaw objects
// representing the raw result of SearchRawTransactionsverbose
func (db *WiredDB) GetAddressTransactionsRaw(addr string, count int) []*apitypes.AddressTxRaw {
	return db.GetAddressTransactionsRawWithSkip(addr, count, 0)
}

// GetAddressTransactionsRawWithSkip returns an array of apitypes.AddressTxRaw objects
// representing the raw result of SearchRawTransactionsverbose
func (db *WiredDB) GetAddressTransactionsRawWithSkip(addr string, count int, skip int) []*apitypes.AddressTxRaw {
	address, err := dcrutil.DecodeAddress(addr)
	if err != nil {
		log.Infof("Invalid address %s: %v", addr, err)
		return nil
	}
	txs, err := db.client.SearchRawTransactionsVerbose(address, skip, count, true, true, nil)
	if err != nil {
		if strings.Contains(err.Error(), "No Txns available") {
			return make([]*apitypes.AddressTxRaw, 0)
		}
		log.Warnf("GetAddressTransactionsRaw failed for address %s: %v", addr, err)
		return nil
	}
	txarray := make([]*apitypes.AddressTxRaw, 0, len(txs))
	for i := range txs {
		tx := new(apitypes.AddressTxRaw)
		tx.Size = int32(len(txs[i].Hex) / 2)
		tx.TxID = txs[i].Txid
		tx.Version = txs[i].Version
		tx.Locktime = txs[i].LockTime
		tx.Vin = make([]dcrjson.VinPrevOut, len(txs[i].Vin))
		copy(tx.Vin, txs[i].Vin)
		tx.Confirmations = int64(txs[i].Confirmations)
		tx.BlockHash = txs[i].BlockHash
		tx.Blocktime = apitypes.TimeAPI{S: dbtypes.NewTimeDefFromUNIX(txs[i].Blocktime)}
		tx.Time = apitypes.TimeAPI{S: dbtypes.NewTimeDefFromUNIX(txs[i].Time)}
		tx.Vout = make([]apitypes.Vout, len(txs[i].Vout))
		for j := range txs[i].Vout {
			tx.Vout[j].Value = txs[i].Vout[j].Value
			tx.Vout[j].N = txs[i].Vout[j].N
			tx.Vout[j].Version = txs[i].Vout[j].Version
			spk := &tx.Vout[j].ScriptPubKeyDecoded
			spkRaw := &txs[i].Vout[j].ScriptPubKey
			spk.Asm = spkRaw.Asm
			spk.Hex = spkRaw.Hex
			spk.ReqSigs = spkRaw.ReqSigs
			spk.Type = spkRaw.Type
			spk.Addresses = make([]string, len(spkRaw.Addresses))
			for k := range spkRaw.Addresses {
				spk.Addresses[k] = spkRaw.Addresses[k]
			}
			if spkRaw.CommitAmt != nil {
				spk.CommitAmt = new(float64)
				*spk.CommitAmt = *spkRaw.CommitAmt
			}
		}
		txarray = append(txarray, tx)
	}

	return txarray
}

func sumOutsTxRawResult(txs []dcrjson.TxRawResult) (sum float64) {
	for _, tx := range txs {
		for _, vout := range tx.Vout {
			sum += vout.Value
		}
	}
	return
}

func makeExplorerBlockBasic(data *dcrjson.GetBlockVerboseResult, params *chaincfg.Params) *exptypes.BlockBasic {
	index := dbtypes.CalculateWindowIndex(data.Height, params.StakeDiffWindowSize)

	total := sumOutsTxRawResult(data.RawTx) + sumOutsTxRawResult(data.RawSTx)

	numReg := len(data.RawTx)

	block := &exptypes.BlockBasic{
		IndexVal:       index,
		Height:         data.Height,
		Hash:           data.Hash,
		Version:        data.Version,
		Size:           data.Size,
		Valid:          true, // we do not know this, TODO with DB v2
		MainChain:      true,
		Voters:         data.Voters,
		Transactions:   numReg,
		FreshStake:     data.FreshStake,
		Revocations:    uint32(data.Revocations),
		TxCount:        uint32(data.FreshStake+data.Revocations) + uint32(numReg) + uint32(data.Voters),
		BlockTime:      exptypes.NewTimeDefFromUNIX(data.Time),
		FormattedBytes: humanize.Bytes(uint64(data.Size)),
		Total:          total,
	}

	return block
}

func makeExplorerTxBasic(data dcrjson.TxRawResult, msgTx *wire.MsgTx, params *chaincfg.Params) *exptypes.TxBasic {
	tx := new(exptypes.TxBasic)
	tx.TxID = data.Txid
	tx.FormattedSize = humanize.Bytes(uint64(len(data.Hex) / 2))
	tx.Total = txhelpers.TotalVout(data.Vout).ToCoin()
	tx.Fee, tx.FeeRate = txhelpers.TxFeeRate(msgTx)
	for _, i := range data.Vin {
		if i.IsCoinBase() /* not IsStakeBase */ {
			tx.Coinbase = true
			tx.Fee, tx.FeeRate = 0, 0
		}
	}
	if stake.IsSSGen(msgTx) {
		validation, version, bits, choices, err := txhelpers.SSGenVoteChoices(msgTx, params)
		if err != nil {
			log.Debugf("Cannot get vote choices for %s", tx.TxID)
			return tx
		}
		tx.VoteInfo = &exptypes.VoteInfo{
			Validation: exptypes.BlockValidation{
				Hash:     validation.Hash.String(),
				Height:   validation.Height,
				Validity: validation.Validity,
			},
			Version: version,
			Bits:    bits,
			Choices: choices,
		}
	}
	return tx
}

func makeExplorerAddressTx(data *dcrjson.SearchRawTransactionsResult, address string) *dbtypes.AddressTx {
	tx := new(dbtypes.AddressTx)
	tx.TxID = data.Txid
	tx.FormattedSize = humanize.Bytes(uint64(len(data.Hex) / 2))
	tx.Total = txhelpers.TotalVout(data.Vout).ToCoin()
	tx.Time = dbtypes.NewTimeDefFromUNIX(data.Time)
	tx.Confirmations = data.Confirmations

	msgTx, err := txhelpers.MsgTxFromHex(data.Hex)
	if err == nil {
		tx.TxType = txhelpers.DetermineTxTypeString(msgTx)
	} else {
		log.Warn("makeExplorerAddressTx cannot get tx type", err)
	}

	for i := range data.Vin {
		if data.Vin[i].PrevOut != nil && len(data.Vin[i].PrevOut.Addresses) > 0 {
			if data.Vin[i].PrevOut.Addresses[0] == address {
				tx.SentTotal += *data.Vin[i].AmountIn
			}
		}
	}
	for i := range data.Vout {
		if len(data.Vout[i].ScriptPubKey.Addresses) != 0 {
			if data.Vout[i].ScriptPubKey.Addresses[0] == address {
				tx.ReceivedTotal += data.Vout[i].Value
			}
		}
	}
	return tx
}

// GetExplorerBlocks creates an slice of exptypes.BlockBasic beginning at start
// and decreasing in block height to end, not including end.
func (db *WiredDB) GetExplorerBlocks(start int, end int) []*exptypes.BlockBasic {
	if start < end {
		return nil
	}
	summaries := make([]*exptypes.BlockBasic, 0, start-end)
	for i := start; i > end; i-- {
		data := db.GetBlockVerbose(i, true)
		block := new(exptypes.BlockBasic)
		if data != nil {
			block = makeExplorerBlockBasic(data, db.params)
		}
		summaries = append(summaries, block)
	}
	return summaries
}

func (db *WiredDB) GetExplorerFullBlocks(start int, end int) []*exptypes.BlockInfo {
	if start < end {
		return nil
	}
	summaries := make([]*exptypes.BlockInfo, 0, start-end)
	for i := start; i > end; i-- {
		data := db.GetBlockVerbose(i, true)
		block := new(exptypes.BlockInfo)
		if data != nil {
			block = db.GetExplorerBlock(data.Hash)
		}
		summaries = append(summaries, block)
	}
	return summaries
}

func (db *WiredDB) GetExplorerBlock(hash string) *exptypes.BlockInfo {
	data := db.GetBlockVerboseByHash(hash, true)
	if data == nil {
		log.Error("Unable to get block for block hash " + hash)
		return nil
	}

	b := makeExplorerBlockBasic(data, db.params)

	// Explorer Block Info
	block := &exptypes.BlockInfo{
		BlockBasic:            b,
		Confirmations:         data.Confirmations,
		StakeRoot:             data.StakeRoot,
		MerkleRoot:            data.MerkleRoot,
		Nonce:                 data.Nonce,
		VoteBits:              data.VoteBits,
		FinalState:            data.FinalState,
		PoolSize:              data.PoolSize,
		Bits:                  data.Bits,
		SBits:                 data.SBits,
		Difficulty:            data.Difficulty,
		ExtraData:             data.ExtraData,
		StakeVersion:          data.StakeVersion,
		PreviousHash:          data.PreviousHash,
		NextHash:              data.NextHash,
		StakeValidationHeight: db.params.StakeValidationHeight,
		Subsidy:               db.BlockSubsidy(b.Height, b.Voters),
	}

	votes := make([]*exptypes.TrimmedTxInfo, 0, block.Voters)
	revocations := make([]*exptypes.TrimmedTxInfo, 0, block.Revocations)
	tickets := make([]*exptypes.TrimmedTxInfo, 0, block.FreshStake)

	for _, tx := range data.RawSTx {
		msgTx, err := txhelpers.MsgTxFromHex(tx.Hex)
		if err != nil {
			log.Errorf("Unknown transaction %s: %v", tx.Txid, err)
			return nil
		}
		switch stake.DetermineTxType(msgTx) {
		case stake.TxTypeSSGen:
			stx := trimmedTxInfoFromMsgTx(tx, msgTx, db.params)
			// Fees for votes should be zero, but if the transaction was created
			// with unmatched inputs/outputs then the remainder becomes a fee.
			// Account for this possibility by calculating the fee for votes as
			// well.
			if stx.Fee > 0 {
				log.Tracef("Vote with fee! %v, %v DCR", stx.Fee, stx.Fees)
			}
			votes = append(votes, stx)
		case stake.TxTypeSStx:
			stx := trimmedTxInfoFromMsgTx(tx, msgTx, db.params)
			tickets = append(tickets, stx)
		case stake.TxTypeSSRtx:
			stx := trimmedTxInfoFromMsgTx(tx, msgTx, db.params)
			revocations = append(revocations, stx)
		}
	}

	txs := make([]*exptypes.TrimmedTxInfo, 0, block.Transactions)
	for _, tx := range data.RawTx {
		msgTx, err := txhelpers.MsgTxFromHex(tx.Hex)
		if err != nil {
			log.Errorf("Unknown transaction %s: %v", tx.Txid, err)
			return nil
		}

		exptx := trimmedTxInfoFromMsgTx(tx, msgTx, db.params)
		for _, vin := range tx.Vin {
			if vin.IsCoinBase() {
				exptx.Fee, exptx.FeeRate, exptx.Fees = 0.0, 0.0, 0.0
			}
		}
		txs = append(txs, exptx)
	}

	block.Tx = txs
	block.Votes = votes
	block.Revs = revocations
	block.Tickets = tickets

	sortTx := func(txs []*exptypes.TrimmedTxInfo) {
		sort.Slice(txs, func(i, j int) bool {
			return txs[i].Total > txs[j].Total
		})
	}

	sortTx(block.Tx)
	sortTx(block.Votes)
	sortTx(block.Revs)
	sortTx(block.Tickets)

	getTotalFee := func(txs []*exptypes.TrimmedTxInfo) (total dcrutil.Amount) {
		for _, tx := range txs {
			// Coinbase transactions have no fee. The fee should be zero already
			// (as in makeExplorerTxBasic), but intercept coinbase just in case.
			// Note that this does not include stakebase transactions (votes),
			// which can have a fee but are not required to.
			if tx.Coinbase {
				continue
			}
			if tx.Fee < 0 {
				log.Warnf("Negative fees should not happen! %v", tx.Fee)
			}
			total += tx.Fee
		}
		return
	}
	getTotalSent := func(txs []*exptypes.TrimmedTxInfo) (total dcrutil.Amount) {
		for _, tx := range txs {
			amt, err := dcrutil.NewAmount(tx.Total)
			if err != nil {
				continue
			}
			total += amt
		}
		return
	}
	block.TotalSent = (getTotalSent(block.Tx) + getTotalSent(block.Revs) +
		getTotalSent(block.Tickets) + getTotalSent(block.Votes)).ToCoin()
	block.MiningFee = (getTotalFee(block.Tx) + getTotalFee(block.Revs) +
		getTotalFee(block.Tickets) + getTotalFee(block.Votes)).ToCoin()

	return block
}

func trimmedTxInfoFromMsgTx(txraw dcrjson.TxRawResult, msgTx *wire.MsgTx, params *chaincfg.Params) *exptypes.TrimmedTxInfo {
	txBasic := makeExplorerTxBasic(txraw, msgTx, params)

	voteValid := false
	if txBasic.VoteInfo != nil {
		voteValid = txBasic.VoteInfo.Validation.Validity
	}

	tx := &exptypes.TrimmedTxInfo{
		TxBasic:   txBasic,
		Fees:      txBasic.Fee.ToCoin(),
		VinCount:  len(txraw.Vin),
		VoutCount: len(txraw.Vout),
		VoteValid: voteValid,
	}
	return tx
}

func (db *WiredDB) GetExplorerTx(txid string) *exptypes.TxInfo {
	txhash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		log.Errorf("Invalid transaction hash %s", txid)
		return nil
	}
	txraw, err := db.client.GetRawTransactionVerbose(txhash)
	if err != nil {
		log.Warnf("GetRawTransactionVerbose failed for %v: %v", txhash, err)
		return nil
	}
	msgTx, err := txhelpers.MsgTxFromHex(txraw.Hex)
	if err != nil {
		log.Errorf("Cannot create MsgTx for tx %v: %v", txhash, err)
		return nil
	}
	txBasic := makeExplorerTxBasic(*txraw, msgTx, db.params)
	tx := &exptypes.TxInfo{
		TxBasic: txBasic,
	}
	tx.Type = txhelpers.DetermineTxTypeString(msgTx)
	tx.BlockHeight = txraw.BlockHeight
	tx.BlockIndex = txraw.BlockIndex
	tx.BlockHash = txraw.BlockHash
	tx.Confirmations = txraw.Confirmations
	tx.Time = exptypes.NewTimeDefFromUNIX(txraw.Time)

	inputs := make([]exptypes.Vin, 0, len(txraw.Vin))
	for i, vin := range txraw.Vin {
		// The addresses are may only be obtained by decoding the previous
		// output's pkscript.
		var addresses []string
		// The vin amount is now correct in most cases, but get it from the
		// previous output anyway and compare the values for information.
		valueIn, _ := dcrutil.NewAmount(vin.AmountIn)
		// Do not attempt to look up prevout if it is a coinbase or stakebase
		// input, which does not spend a previous output.
		if !(vin.IsCoinBase() || (vin.IsStakeBase() && i == 0)) {
			// Store the vin amount for comparison.
			valueIn0 := valueIn

			addresses, valueIn, err = txhelpers.OutPointAddresses(
				&msgTx.TxIn[i].PreviousOutPoint, db.client, db.params)
			if err != nil {
				log.Warnf("Failed to get outpoint address from txid: %v", err)
				continue
			}
			// See if getrawtransaction had correct vin amounts. It should
			// except for votes on side chain blocks.
			if valueIn != valueIn0 {
				log.Debugf("vin amount in: prevout RPC = %v, vin's amount = %v",
					valueIn, valueIn0)
			}
		}

		// For mempool transactions where the vin block height is not set
		// (height 0 for an input that is not a coinbase or stakebase),
		// determine the height at which the input was generated via RPC.
		if tx.BlockHeight == 0 && vin.BlockHeight == 0 &&
			!txhelpers.IsZeroHashStr(vin.Txid) {
			vinHash, err := chainhash.NewHashFromStr(vin.Txid)
			if err != nil {
				log.Errorf("Failed to translate hash from string: %s", vin.Txid)
			} else {
				prevTx, err := db.client.GetRawTransactionVerbose(vinHash)
				if err == nil {
					vin.BlockHeight = uint32(prevTx.BlockHeight)
				} else {
					log.Errorf("Error getting data for previous outpoint of mempool transaction: %v", err)
				}
			}
		}

		// Assemble and append this vin.
		coinIn := valueIn.ToCoin()
		inputs = append(inputs, exptypes.Vin{
			Vin: &dcrjson.Vin{
				Txid:        vin.Txid,
				Coinbase:    vin.Coinbase,
				Stakebase:   vin.Stakebase,
				Vout:        vin.Vout,
				AmountIn:    coinIn,
				BlockHeight: vin.BlockHeight,
			},
			Addresses:       addresses,
			FormattedAmount: humanize.Commaf(coinIn),
			Index:           uint32(i),
		})
	}
	tx.Vin = inputs

	if tx.Vin[0].IsCoinBase() {
		tx.Type = "Coinbase"
	}
	if tx.Type == "Coinbase" {
		if tx.Confirmations < int64(db.params.CoinbaseMaturity) {
			tx.Mature = "False"
		} else {
			tx.Mature = "True"
		}
		tx.Maturity = int64(db.params.CoinbaseMaturity)

	}
	if tx.IsVote() || tx.IsTicket() {
		if tx.Confirmations > 0 && db.GetBestBlockHeight() >=
			(int64(db.params.TicketMaturity)+tx.BlockHeight) {
			tx.Mature = "True"
		} else {
			tx.Mature = "False"
			tx.TicketInfo.TicketMaturity = int64(db.params.TicketMaturity)
		}
	}
	if tx.IsVote() {
		if tx.Confirmations < int64(db.params.CoinbaseMaturity) {
			tx.VoteFundsLocked = "True"
		} else {
			tx.VoteFundsLocked = "False"
		}
		tx.Maturity = int64(db.params.CoinbaseMaturity) + 1 // Add one to reflect < instead of <=
	}

	CoinbaseMaturityInHours := (db.params.TargetTimePerBlock.Hours() * float64(db.params.CoinbaseMaturity))
	tx.MaturityTimeTill = ((float64(db.params.CoinbaseMaturity) -
		float64(tx.Confirmations)) / float64(db.params.CoinbaseMaturity)) * CoinbaseMaturityInHours

	outputs := make([]exptypes.Vout, 0, len(txraw.Vout))
	for i, vout := range txraw.Vout {
		txout, err := db.client.GetTxOut(txhash, uint32(i), true)
		if err != nil {
			log.Warnf("Failed to determine if tx out is spent for output %d of tx %s", i, txid)
		}
		var opReturn string
		if strings.Contains(vout.ScriptPubKey.Asm, "OP_RETURN") {
			opReturn = vout.ScriptPubKey.Asm
		}
		outputs = append(outputs, exptypes.Vout{
			Addresses:       vout.ScriptPubKey.Addresses,
			Amount:          vout.Value,
			FormattedAmount: humanize.Commaf(vout.Value),
			OP_RETURN:       opReturn,
			Type:            vout.ScriptPubKey.Type,
			Spent:           txout == nil,
			Index:           vout.N,
		})
	}
	tx.Vout = outputs

	// Initialize the spending transaction slice for safety.
	tx.SpendingTxns = make([]exptypes.TxInID, len(outputs))

	return tx
}

func (db *WiredDB) GetExplorerAddress(address string, count, offset int64) (*dbtypes.AddressInfo, txhelpers.AddressType, txhelpers.AddressError) {
	// Validate the address.
	addr, addrType, addrErr := txhelpers.AddressValidation(address, db.params)
	switch addrErr {
	case txhelpers.AddressErrorNoError:
		// All good!
	case txhelpers.AddressErrorZeroAddress:
		// Short circuit the transaction and balance queries if the provided
		// address is the zero pubkey hash address commonly used for zero
		// value sstxchange-tagged outputs.
		return &dbtypes.AddressInfo{
			Address:         address,
			Net:             addr.Net().Name,
			IsDummyAddress:  true,
			Balance:         new(dbtypes.AddressBalance),
			UnconfirmedTxns: new(dbtypes.AddressTransactions),
			Limit:           count,
			Offset:          offset,
			Fullmode:        true,
		}, addrType, nil
	case txhelpers.AddressErrorWrongNet:
		// Set the net name field so a user can be properly directed.
		return &dbtypes.AddressInfo{
			Address: address,
			Net:     addr.Net().Name,
		}, addrType, addrErr
	default:
		return nil, addrType, addrErr
	}

	txs, err := db.client.SearchRawTransactionsVerbose(addr,
		int(offset), int(MaxAddressRows), true, true, nil)
	if err != nil {
		if err.Error() == "-32603: No Txns available" {
			log.Tracef("GetExplorerAddress: No transactions found for address %s: %v", addr, err)
			return &dbtypes.AddressInfo{
				Address:    address,
				Net:        addr.Net().Name,
				MaxTxLimit: MaxAddressRows,
				Limit:      count,
				Offset:     offset,
			}, addrType, nil
		}
		log.Warnf("GetExplorerAddress: SearchRawTransactionsVerbose failed for address %s: %v", addr, err)
		return nil, addrType, txhelpers.AddressErrorUnknown
	}

	addressTxs := make([]*dbtypes.AddressTx, 0, len(txs))
	for i, tx := range txs {
		if int64(i) == count { // count >= len(txs)
			break
		}
		addressTxs = append(addressTxs, makeExplorerAddressTx(tx, address))
	}

	var numUnconfirmed, numReceiving, numSpending int64
	var totalreceived, totalsent dcrutil.Amount

	for _, tx := range txs {
		if tx.Confirmations == 0 {
			numUnconfirmed++
		}
		for _, y := range tx.Vout {
			if len(y.ScriptPubKey.Addresses) != 0 {
				if address == y.ScriptPubKey.Addresses[0] {
					t, _ := dcrutil.NewAmount(y.Value)
					if t > 0 {
						totalreceived += t
					}
					numReceiving++
				}
			}
		}
		for _, u := range tx.Vin {
			if u.PrevOut != nil && len(u.PrevOut.Addresses) != 0 {
				if address == u.PrevOut.Addresses[0] {
					t, _ := dcrutil.NewAmount(*u.AmountIn)
					if t > 0 {
						totalsent += t
					}
					numSpending++
				}
			}
		}
	}

	numTxns, numberMaxOfTx := count, int64(len(txs))
	if numTxns > numberMaxOfTx {
		numTxns = numberMaxOfTx
	}
	balance := &dbtypes.AddressBalance{
		Address:      address,
		NumSpent:     numSpending,
		NumUnspent:   numReceiving,
		TotalSpent:   int64(totalsent),
		TotalUnspent: int64(totalreceived - totalsent),
	}
	addrData := &dbtypes.AddressInfo{
		Address:           address,
		Net:               addr.Net().Name,
		MaxTxLimit:        MaxAddressRows,
		Limit:             count,
		Offset:            offset,
		NumUnconfirmed:    numUnconfirmed,
		Transactions:      addressTxs,
		NumTransactions:   numTxns,
		NumFundingTxns:    numReceiving,
		NumSpendingTxns:   numSpending,
		AmountReceived:    totalreceived,
		AmountSent:        totalsent,
		AmountUnspent:     totalreceived - totalsent,
		Balance:           balance,
		KnownTransactions: numberMaxOfTx,
		KnownFundingTxns:  numReceiving,
		KnownSpendingTxns: numSpending,
	}

	// Sort by date and calculate block height.
	height, err := db.GetHeight()
	if err != nil {
		log.Warnf("GetExplorerAddress: failed to get best block: %v", err)
		return nil, addrType, txhelpers.AddressErrorUnknown
	}
	addrData.PostProcess(uint32(height))

	return addrData, addrType, nil
}

// CountUnconfirmedTransactions returns the number of unconfirmed transactions
// involving the specified address, given a maximum possible unconfirmed.
func (db *WiredDB) CountUnconfirmedTransactions(address string) (int64, error) {
	_, numUnconfirmed, err := db.UnconfirmedTxnsForAddress(address)
	return numUnconfirmed, err
}

// UnconfirmedTxnsForAddress routes through rpcutils with appropriate
// arguments. Returns mempool inputs/outputs associated with the given address.
func (db *WiredDB) UnconfirmedTxnsForAddress(address string) (*txhelpers.AddressOutpoints, int64, error) {
	return rpcutils.UnconfirmedTxnsForAddress(db.client, address, db.params)
}

// GetMempool gets all transactions from the mempool for explorer and adds the
// total out for all the txs and vote info for the votes. The returned slice
// will be nil if the GetRawMempoolVerbose RPC fails. A zero-length non-nil
// slice is returned if there are no transactions in mempool.
func (db *WiredDB) GetMempool() []exptypes.MempoolTx {
	mempooltxs, err := db.client.GetRawMempoolVerbose(dcrjson.GRMAll)
	if err != nil {
		log.Errorf("GetRawMempoolVerbose failed: %v", err)
		return nil
	}

	txs := make([]exptypes.MempoolTx, 0, len(mempooltxs))

	for hashStr, tx := range mempooltxs {
		hash, err := chainhash.NewHashFromStr(hashStr)
		if err != nil {
			continue
		}
		rawtx, hex := db.getRawTransaction(hash)
		total := 0.0
		if rawtx == nil {
			continue
		}
		for _, v := range rawtx.Vout {
			total += v.Value
		}
		msgTx, err := txhelpers.MsgTxFromHex(hex)
		if err != nil {
			continue
		}
		var voteInfo *exptypes.VoteInfo

		if ok := stake.IsSSGen(msgTx); ok {
			validation, version, bits, choices, err := txhelpers.SSGenVoteChoices(msgTx, db.params)
			if err != nil {
				log.Debugf("Cannot get vote choices for %s", hash)

			} else {
				voteInfo = &exptypes.VoteInfo{
					Validation: exptypes.BlockValidation{
						Hash:     validation.Hash.String(),
						Height:   validation.Height,
						Validity: validation.Validity,
					},
					Version:     version,
					Bits:        bits,
					Choices:     choices,
					TicketSpent: msgTx.TxIn[1].PreviousOutPoint.Hash.String(),
				}
			}
		}
		txs = append(txs, exptypes.MempoolTx{
			TxID:     msgTx.TxHash().String(),
			Hash:     hashStr,
			Time:     tx.Time,
			Size:     tx.Size,
			TotalOut: total,
			Type:     txhelpers.DetermineTxTypeString(msgTx),
			VoteInfo: voteInfo,
			Vin:      exptypes.MsgTxMempoolInputs(msgTx),
		})
	}

	return txs
}

// TxHeight gives the block height of the transaction id specified
func (db *WiredDB) TxHeight(txid *chainhash.Hash) (height int64) {
	txraw, err := db.client.GetRawTransactionVerbose(txid)
	if err != nil {
		log.Errorf("GetRawTransactionVerbose failed for: %v", txid)
		return 0
	}
	height = txraw.BlockHeight
	return
}

// Difficulty returns the difficulty.
func (db *WiredDB) Difficulty() (float64, error) {
	diff, err := db.client.GetDifficulty()
	if err != nil {
		log.Error("GetDifficulty failed")
		return diff, err
	}
	return diff, nil
}

// GetTip grabs the highest block stored in the database.
func (db *WiredDB) GetTip() (*exptypes.WebBasicBlock, error) {
	tip, err := db.DB.getTip()
	if err != nil {
		return nil, err
	}
	blockdata := exptypes.WebBasicBlock{
		Height:      tip.Height,
		Size:        tip.Size,
		Hash:        tip.Hash,
		Difficulty:  tip.Difficulty,
		StakeDiff:   tip.StakeDiff,
		Time:        tip.Time.S.UNIX(),
		NumTx:       tip.NumTx,
		PoolSize:    tip.PoolInfo.Size,
		PoolValue:   tip.PoolInfo.Value,
		PoolValAvg:  tip.PoolInfo.ValAvg,
		PoolWinners: tip.PoolInfo.Winners,
	}
	return &blockdata, nil
}
