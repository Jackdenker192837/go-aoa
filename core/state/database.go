// Copyright 2021 The go-aoa Authors
// This file is part of the go-aoa library.
//
// The the go-aoa library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The the go-aoa library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-aoa library. If not, see <http://www.gnu.org/licenses/>.

package state

import (
	"fmt"
	"sync"

	"github.com/Aurorachain-io/go-aoa/common"
	"github.com/Aurorachain-io/go-aoa/aoadb"
	"github.com/Aurorachain-io/go-aoa/trie"
	"github.com/hashicorp/golang-lru"
)

// Trie cache generation limit after which to evic trie nodes from memory.
var MaxTrieCacheGen = uint16(120)

const (
	// Number of past tries to keep. This value is chosen such that
	// reasonable chain reorg depths will hit an existing trie.
	maxPastTries = 12

	// Number of codehash->size associations to keep.
	codeSizeCacheSize = 100000

	abiKeySuffix = "_abi"
)

// Database wraps access to tries and contract code.
type Database interface {
	// Accessing tries:
	// OpenTrie opens the main account trie.
	// OpenStorageTrie opens the storage trie of an account.
	OpenTrie(root common.Hash) (Trie, error)
	OpenStorageTrie(addrHash, root common.Hash) (Trie, error)
	// Accessing contract code:
	ContractCode(addrHash, codeHash common.Hash) ([]byte, error)
	ContractCodeSize(addrHash, codeHash common.Hash) (int, error)
	// Accessing contract abi
	ContractAbi(addrHash, codeHash common.Hash) (string, error)
	// CopyTrie returns an independent copy of the given trie.
	CopyTrie(Trie) Trie
	// Accessing assetdata
	AssetData(addrHash, assetHash common.Hash) ([]byte, error)
}

// Trie is a eminer-pro Merkle Trie.
type Trie interface {
	TryGet(key []byte) ([]byte, error)
	TryUpdate(key, value []byte) error
	TryDelete(key []byte) error
	CommitTo(trie.DatabaseWriter) (common.Hash, error)
	Hash() common.Hash
	NodeIterator(startKey []byte) trie.NodeIterator
	GetKey([]byte) []byte // TODO(fjl): remove this when SecureTrie is removed
}

// NewDatabase creates a backing store for state. The returned database is safe for
// concurrent use and retains cached trie nodes in memory.
func NewDatabase(db aoadb.Database) Database {
	csc, _ := lru.New(codeSizeCacheSize)
	return &cachingDB{db: db, codeSizeCache: csc}
}

type cachingDB struct {
	db            aoadb.Database
	mu            sync.Mutex
	pastTries     []*trie.SecureTrie
	codeSizeCache *lru.Cache
}

func (db *cachingDB) OpenTrie(root common.Hash) (Trie, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	for i := len(db.pastTries) - 1; i >= 0; i-- {
		if db.pastTries[i].Hash() == root {
			return cachedTrie{db.pastTries[i].Copy(), db}, nil
		}
	}
	tr, err := trie.NewSecure(root, db.db, MaxTrieCacheGen)
	if err != nil {
		return nil, err
	}
	return cachedTrie{tr, db}, nil
}

func (db *cachingDB) pushTrie(t *trie.SecureTrie) {
	db.mu.Lock()
	defer db.mu.Unlock()

	if len(db.pastTries) >= maxPastTries {
		copy(db.pastTries, db.pastTries[1:])
		db.pastTries[len(db.pastTries)-1] = t
	} else {
		db.pastTries = append(db.pastTries, t)
	}
}

func (db *cachingDB) OpenStorageTrie(addrHash, root common.Hash) (Trie, error) {
	return trie.NewSecure(root, db.db, 0)
}

func (db *cachingDB) CopyTrie(t Trie) Trie {
	switch t := t.(type) {
	case cachedTrie:
		return cachedTrie{t.SecureTrie.Copy(), db}
	case *trie.SecureTrie:
		return t.Copy()
	default:
		panic(fmt.Errorf("unknown trie type %T", t))
	}
}

func (db *cachingDB) ContractCode(addrHash, codeHash common.Hash) ([]byte, error) {
	code, err := db.db.Get(codeHash[:])
	if err == nil {
		db.codeSizeCache.Add(codeHash, len(code))
	}
	return code, err
}

func (db *cachingDB) ContractCodeSize(addrHash, codeHash common.Hash) (int, error) {
	if cached, ok := db.codeSizeCache.Get(codeHash); ok {
		return cached.(int), nil
	}
	code, err := db.ContractCode(addrHash, codeHash)
	if err == nil {
		db.codeSizeCache.Add(codeHash, len(code))
	}
	return len(code), err
}

func (db *cachingDB) ContractAbi(addrHash, codeHash common.Hash) (string, error) {
	key := AbiKey(codeHash.Bytes())
	has, err := db.db.Has(key)
	if has {
		abibytes, err := db.db.Get(key)
		if err == nil {
			return string(abibytes), nil
		}
	}
	return "", err
}

func (db *cachingDB) AssetData(addrHash, assetHash common.Hash) ([]byte, error) {
	return db.db.Get(assetHash[:])
}

func AbiKey(codeHash []byte) []byte {
	return append(codeHash, []byte(abiKeySuffix)...)
}

// cachedTrie inserts its trie into a cachingDB on commit.
type cachedTrie struct {
	*trie.SecureTrie
	db *cachingDB
}

func (m cachedTrie) CommitTo(dbw trie.DatabaseWriter) (common.Hash, error) {
	root, err := m.SecureTrie.CommitTo(dbw)
	if err == nil {
		m.db.pushTrie(m.SecureTrie)
	}
	return root, err
}
