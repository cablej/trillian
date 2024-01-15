// Copyright 2016 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto" //nolint:staticcheck
	"github.com/google/trillian"
	"github.com/google/trillian/merkle/hashers"
	"github.com/google/trillian/merkle/hashers/registry"
	"github.com/google/trillian/merkle/smt"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/storage/cache"
	"github.com/google/trillian/storage/storagepb"
	"github.com/google/trillian/storage/storagepb/convert"
	"github.com/google/trillian/types"

	stree "github.com/google/trillian/storage/tree"
)

const (
	insertMapHeadSQL = `INSERT INTO MapHead(TreeId, MapHeadTimestamp, RootHash, MapRevision, RootSignature, MapperData)
	VALUES(?, ?, ?, ?, ?, ?)`
	selectLatestSignedMapRootSQL = `SELECT MapHeadTimestamp, RootHash, MapRevision, RootSignature, MapperData
		 FROM MapHead WHERE TreeId=?
		 ORDER BY MapHeadTimestamp DESC LIMIT 1`
	selectGetSignedMapRootSQL = `SELECT MapHeadTimestamp, RootHash, MapRevision, RootSignature, MapperData
		 FROM MapHead WHERE TreeId=? AND MapRevision=?`
	insertMapLeafSQL = `INSERT INTO MapLeaf(TreeId, KeyHash, MapRevision, LeafValue) VALUES (?, ?, ?, ?)`
)

var (
	defaultMapStrata = []int{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 176}
	defaultLayout    = stree.NewLayout(defaultMapStrata)
)

type mySQLMapStorage struct {
	*mySQLTreeStorage
	admin storage.AdminStorage
}

// NewMapStorage creates a storage.MapStorage instance for the specified MySQL URL.
// It assumes storage.AdminStorage is backed by the same MySQL database as well.
func NewMapStorage(db *sql.DB) storage.MapStorage {
	return &mySQLMapStorage{
		admin:            NewAdminStorage(db),
		mySQLTreeStorage: newTreeStorage(db),
	}
}

func (m *mySQLMapStorage) CheckDatabaseAccessible(ctx context.Context) error {
	return m.db.PingContext(ctx)
}

func (m *mySQLMapStorage) begin(ctx context.Context, tree *trillian.Tree, readonly bool) (storage.MapTreeTX, error) {
	// TODO: Find a stronger way to ensure that tree has been pulled from storage.
	// This is a cheap safety-belt check to help us use this API consistently.
	if tree.UpdateTime == nil {
		return nil, fmt.Errorf("tree.UpdateTime: %v. tree must be pulled from storage", tree.UpdateTime)
	}
	if got, want := tree.TreeType, trillian.TreeType_MAP; got != want {
		return nil, fmt.Errorf("begin(tree.TreeType: %v), want %v", got, want)
	}
	hasher, err := registry.NewMapHasher(tree.HashStrategy)
	if err != nil {
		return nil, err
	}

	stCache := cache.NewMapSubtreeCache(defaultMapStrata, tree.TreeId, hasher)
	ttx, err := m.beginTreeTx(ctx, tree, hasher.Size(), stCache)
	if err != nil {
		return nil, err
	}
	l, err := m.Layout(tree)
	if err != nil {
		return nil, err
	}
	mtx := &mapTreeTX{
		treeTX:       ttx,
		layout:       l,
		ms:           m,
		hasher:       hasher,
		readRevision: -1,
	}

	if readonly {
		// readRevision will be set later, by the first
		// GetSignedMapRoot/LatestSignedMapRoot operation.
		return mtx, nil
	}

	// A read-write transaction needs to know the current revision
	// so it can write at revision+1.
	root, err := mtx.LatestSignedMapRoot(ctx)
	if err != nil && err != storage.ErrTreeNeedsInit {
		return nil, err
	}
	if err == storage.ErrTreeNeedsInit {
		return mtx, err
	}

	var mr types.MapRootV1
	if err := mr.UnmarshalBinary(root.MapRoot); err != nil {
		return nil, err
	}

	mtx.readRevision = int64(mr.Revision)
	mtx.treeTX.writeRevision = int64(mr.Revision) + 1
	return mtx, nil
}

func (m *mySQLMapStorage) SnapshotForTree(ctx context.Context, tree *trillian.Tree) (storage.ReadOnlyMapTreeTX, error) {
	return m.begin(ctx, tree, true /* readonly */)
}

// Layout returns the layout of the given tree.
func (m *mySQLMapStorage) Layout(*trillian.Tree) (*stree.Layout, error) {
	return defaultLayout, nil
}

func (m *mySQLMapStorage) ReadWriteTransaction(ctx context.Context, tree *trillian.Tree, f storage.MapTXFunc) error {
	tx, err := m.begin(ctx, tree, false /* readonly */)
	if tx != nil {
		defer tx.Close()
	}
	if err != nil && err != storage.ErrTreeNeedsInit {
		return err
	}
	if err := f(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type mapTreeTX struct {
	treeTX
	layout       *stree.Layout
	ms           *mySQLMapStorage
	hasher       hashers.MapHasher
	readRevision int64
}

func (m *mapTreeTX) ReadRevision(ctx context.Context) (int64, error) {
	m.treeTX.mu.Lock()
	defer m.treeTX.mu.Unlock()

	return int64(m.readRevision), nil
}

func (m *mapTreeTX) WriteRevision(ctx context.Context) (int64, error) {
	m.treeTX.mu.Lock()
	defer m.treeTX.mu.Unlock()

	if m.treeTX.writeRevision < 0 {
		return m.treeTX.writeRevision, errors.New("mapTreeTX write revision not populated")
	}
	return m.treeTX.writeRevision, nil
}

func (m *mapTreeTX) Set(ctx context.Context, keyHash []byte, value *trillian.MapLeaf) error {
	m.treeTX.mu.Lock()
	defer m.treeTX.mu.Unlock()

	// TODO(al): consider storing some sort of value which represents the group of keys being set in this Tx.
	//           That way, if this attempt partially fails (i.e. because some subset of the in-the-future Merkle
	//           nodes do get written), we can enforce that future map update attempts are a complete replay of
	//           the failed set.
	flatValue, err := proto.Marshal(value)
	if err != nil {
		return nil
	}

	stmt, err := m.tx.PrepareContext(ctx, insertMapLeafSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	_, err = stmt.ExecContext(ctx, m.treeID, keyHash, m.writeRevision, flatValue)
	return err
}

// Get returns a list of map leaves indicated by indexes.
// If an index is not found, no corresponding entry is returned.
// Each MapLeaf.Index is overwritten with the index the leaf was found at.
func (m *mapTreeTX) Get(ctx context.Context, revision int64, indexes [][]byte) ([]*trillian.MapLeaf, error) {
	m.treeTX.mu.Lock()
	defer m.treeTX.mu.Unlock()

	// If no indexes are requested, return an empty set.
	if len(indexes) == 0 {
		return []*trillian.MapLeaf{}, nil
	}
	const selectMapLeafSQL = `
 SELECT t1.KeyHash, t1.LeafValue
 FROM MapLeaf t1
 INNER JOIN
 (
	SELECT TreeId, KeyHash, MAX(MapRevision) as maxrev
	FROM MapLeaf t0
	WHERE t0.KeyHash IN (` + placeholderSQL + `) AND
	      t0.TreeId = ? AND t0.MapRevision <= ?
	GROUP BY t0.TreeId, t0.KeyHash
 ) t2
 ON t1.TreeId=t2.TreeId
 AND t1.KeyHash=t2.KeyHash
 AND t1.MapRevision=t2.maxrev`

	stmt, err := m.ms.getStmt(ctx, selectMapLeafSQL, len(indexes), "?", "?")
	if err != nil {
		return nil, err
	}
	stx := m.tx.StmtContext(ctx, stmt)
	defer stx.Close()

	args := make([]interface{}, 0, len(indexes)+2)
	for _, index := range indexes {
		args = append(args, index)
	}
	args = append(args, m.treeID)
	args = append(args, revision)

	rows, err := stx.QueryContext(ctx, args...)
	// It's possible there are no values for any of these keys yet
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer rows.Close()

	ret := make([]*trillian.MapLeaf, 0, len(indexes))
	for rows.Next() {
		var mapKeyHash, flatData []byte
		if err := rows.Scan(&mapKeyHash, &flatData); err != nil {
			return nil, err
		}
		mapLeaf, err := unmarshalMapLeaf(flatData, mapKeyHash)
		if err != nil {
			return nil, err
		}
		ret = append(ret, mapLeaf)
	}
	return ret, nil
}

// GetTiles reads the Merkle tree tiles with the given root IDs at the given
// revision. A tile is empty if it is missing from the returned slice.
func (m *mapTreeTX) GetTiles(ctx context.Context, rev int64, ids []stree.NodeID2) ([]smt.Tile, error) {
	// TODO(pavelkalinnikov): Use NodeID2 directly.
	rootIDs := make([]stree.NodeID, 0, len(ids))
	for _, id := range ids {
		rootIDs = append(rootIDs, stree.NewNodeIDFromID2(id))
	}
	subs, err := m.treeTX.getSubtreesWithLock(ctx, rev, rootIDs)
	if err != nil {
		return nil, err
	}
	tiles := make([]smt.Tile, 0, len(subs))
	for _, sub := range subs {
		tile, err := convert.Unmarshal(sub)
		if err != nil {
			return nil, err
		}
		tiles = append(tiles, tile)
	}
	return tiles, nil
}

// SetTiles stores the given tiles at the current write revision.
func (m *mapTreeTX) SetTiles(ctx context.Context, tiles []smt.Tile) error {
	subs := make([]*storagepb.SubtreeProto, 0, len(tiles))
	for _, tile := range tiles {
		height := m.layout.TileHeight(int(tile.ID.BitLen()))
		pb, err := convert.Marshal(tile, uint(height))
		if err != nil {
			return err
		}
		subs = append(subs, pb)
	}
	m.treeTX.addSubtrees(subs)
	return nil
}

func unmarshalMapLeaf(marshaledLeaf, mapKeyHash []byte) (*trillian.MapLeaf, error) {
	if len(marshaledLeaf) == 0 {
		return nil, errors.New("len(marshaledLeaf): 0 want > 0")
	}
	var mapLeaf trillian.MapLeaf
	if err := proto.Unmarshal(marshaledLeaf, &mapLeaf); err != nil {
		return nil, err
	}
	mapLeaf.Index = mapKeyHash
	return &mapLeaf, nil
}

func (m *mapTreeTX) GetSignedMapRoot(ctx context.Context, revision int64) (*trillian.SignedMapRoot, error) {
	m.treeTX.mu.Lock()
	defer m.treeTX.mu.Unlock()

	var timestamp, mapRevision int64
	var rootHash, rootSignatureBytes []byte
	var mapperMetaBytes []byte

	stmt, err := m.tx.PrepareContext(ctx, selectGetSignedMapRootSQL)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	err = stmt.QueryRowContext(ctx, m.treeID, revision).Scan(
		&timestamp, &rootHash, &mapRevision, &rootSignatureBytes, &mapperMetaBytes)
	if err != nil {
		if revision == 0 {
			return nil, storage.ErrTreeNeedsInit
		}
		return nil, err
	}
	m.readRevision = mapRevision
	return m.signedMapRoot(timestamp, mapRevision, rootHash, rootSignatureBytes, mapperMetaBytes)
}

func (m *mapTreeTX) LatestSignedMapRoot(ctx context.Context) (*trillian.SignedMapRoot, error) {
	m.treeTX.mu.Lock()
	defer m.treeTX.mu.Unlock()

	var timestamp, mapRevision int64
	var rootHash, rootSignatureBytes []byte
	var mapperMetaBytes []byte

	stmt, err := m.tx.PrepareContext(ctx, selectLatestSignedMapRootSQL)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	err = stmt.QueryRowContext(ctx, m.treeID).Scan(
		&timestamp, &rootHash, &mapRevision, &rootSignatureBytes, &mapperMetaBytes)

	// It's possible there are no roots for this tree yet
	if err == sql.ErrNoRows {
		return nil, storage.ErrTreeNeedsInit
	} else if err != nil {
		return nil, err
	}
	m.readRevision = mapRevision
	return m.signedMapRoot(timestamp, mapRevision, rootHash, rootSignatureBytes, mapperMetaBytes)
}

func (m *mapTreeTX) signedMapRoot(timestamp, mapRevision int64, rootHash, rootSignature, mapperMeta []byte) (*trillian.SignedMapRoot, error) {
	mapRoot, err := (&types.MapRootV1{
		RootHash:       rootHash,
		TimestampNanos: uint64(timestamp),
		Revision:       uint64(mapRevision),
		Metadata:       mapperMeta,
	}).MarshalBinary()
	if err != nil {
		return nil, err
	}

	return &trillian.SignedMapRoot{
		MapRoot:   mapRoot,
		Signature: rootSignature,
	}, nil
}

func (m *mapTreeTX) StoreSignedMapRoot(ctx context.Context, root *trillian.SignedMapRoot) error {
	m.treeTX.mu.Lock()
	defer m.treeTX.mu.Unlock()

	var r types.MapRootV1
	if err := r.UnmarshalBinary(root.MapRoot); err != nil {
		return err
	}

	stmt, err := m.tx.PrepareContext(ctx, insertMapHeadSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// TODO(al): store transactionLogHead too
	res, err := stmt.ExecContext(ctx, m.treeID, r.TimestampNanos, r.RootHash, r.Revision, root.Signature, r.Metadata)
	if err != nil {
		glog.Warningf("Failed to store signed map root: %s", err)
	}

	return checkResultOkAndRowCountIs(res, err, 1)
}
