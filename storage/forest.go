package storage

import (
	"fmt"

	"github.com/golang/protobuf/proto"

	lru "github.com/hashicorp/golang-lru"
	dbm "github.com/tendermint/tm-db"
	"github.com/xlab/treeprint"
)

const (
	commitsPrefix = "c"
	treePrefix    = "t"
)

// Access the read path of a forest
type ForestReader interface {
	Reader(prefix []byte) (KVCallbackIterableReader, error)
}

// MutableForest is a collection of versioned lazily-loaded RWTrees organised by prefix. It maintains a global state hash
// by storing CommitIDs in a special commitsTree (you could think of it is a two-layer single tree rather than a forest).
//
// The trees (or sub-trees if you prefer) in the forest are RWTrees which wrap an IAVL MutableTree routing writes to the
// MutableTree and reads to the last saved ImmutableTree. In this way reads act only against committed state and can be
// lock free (this allows us to avoid blocking commits - particularly for long-running iterations).
//
// The trees in the forest are created lazily as required by new writes. There is a cache of most recently used trees
// and trees that may require a save are marked as such. New writes are only available to read after a Save().
//
// Here is an example forest (the output is generated by the Dump() function):
//  .
//  ├── balances
//  │   ├── Caitlin -> 2344
//  │   ├── Cora -> 654456
//  │   ├── Edward -> 34
//  │   └── Lindsay -> 654
//  └── names
//      ├── Caitlin -> female
//      ├── Cora -> female
//      ├── Edward -> male
//      └── Lindsay -> unisex
//
// Here there are two tree indexed by the prefixes 'balances' and 'names'.
//
// To perform reads of the forest we access it in the following way:
//
//   tree, err := forest.Reader("names")
//   gender := tree.Get("Cora")
//
// To perform writes:
//
//   tree, err := forest.Writer("names")
//   tree.Set("Cora", "unspecified")
//
// If there is no tree currently stored at the prefix passed then it will be created when the forest is saved:
//
//  hash, version, err := forest.Save()
//
// where the global version for the forest is returned.

type MutableForest struct {
	// A tree containing a reference for all contained trees in the form of prefix -> CommitID
	commitsTree *RWTree
	// Much of the implementation of MutableForest is contained in ImmutableForest which is embedded here and used
	// mutable via its private API. This embedded instance holds a reference to commitsTree above.
	*ImmutableForest
	// Map of prefix -> tree for trees that may require a save (but only will be if they have actually been updated)
	dirty map[string]*RWTree
	// List of dirty prefixes in deterministic order so we may loop over them on Save() and obtain a consistent commitTree hash
	dirtyPrefixes []string
}

// ImmutableForest contains much of the implementation for MutableForest yet it's external API is immutable
type ImmutableForest struct {
	// Store of tree prefix -> last commitID (version + hash) - serves as a set of all known trees and provides a global hash
	commitsTree KVCallbackIterableReader
	treeDB      dbm.DB
	// Cache for frequently used trees
	treeCache *lru.Cache
	// Cache size is used in multiple places - for the LRU cache and node cache for any trees created - it probably
	// makes sense for them to be roughly the same size
	cacheSize int
	// Determines whether we use LoadVersionForOverwriting on underlying MutableTrees - since ImmutableForest is used
	// by MutableForest in a writing context sometimes we do need to load a version destructively
	overwriting bool
}

type ForestOption func(*ImmutableForest)

var WithOverwriting ForestOption = func(imf *ImmutableForest) { imf.overwriting = true }

func NewMutableForest(db dbm.DB, cacheSize int) (*MutableForest, error) {
	// The tree whose state root hash is the global state hash
	commitsTree, err := NewRWTree(NewPrefixDB(db, commitsPrefix), cacheSize)
	if err != nil {
		return nil, err
	}
	forest, err := NewImmutableForest(commitsTree, NewPrefixDB(db, treePrefix), cacheSize, WithOverwriting)
	if err != nil {
		return nil, err
	}
	return &MutableForest{
		ImmutableForest: forest,
		commitsTree:     commitsTree,
		dirty:           make(map[string]*RWTree),
	}, nil
}

func NewImmutableForest(commitsTree KVCallbackIterableReader, treeDB dbm.DB, cacheSize int,
	options ...ForestOption) (*ImmutableForest, error) {
	cache, err := lru.New(cacheSize)
	if err != nil {
		return nil, fmt.Errorf("NewImmutableForest() could not create cache: %v", err)
	}
	imf := &ImmutableForest{
		commitsTree: commitsTree,
		treeDB:      treeDB,
		treeCache:   cache,
		cacheSize:   cacheSize,
	}
	for _, opt := range options {
		opt(imf)
	}
	return imf, nil
}

// Load mutable forest from database
func (muf *MutableForest) Load(version int64) error {
	return muf.commitsTree.Load(version, true)
}

func (muf *MutableForest) Save() (hash []byte, version int64, _ error) {
	// Save each tree in forest that requires save
	for _, prefix := range muf.dirtyPrefixes {
		tree := muf.dirty[prefix]
		if tree.Updated() {
			err := muf.saveTree([]byte(prefix), tree)
			if err != nil {
				return nil, 0, err
			}
		}
	}
	// empty dirty cache
	muf.dirty = make(map[string]*RWTree, len(muf.dirty))
	muf.dirtyPrefixes = muf.dirtyPrefixes[:0]
	return muf.commitsTree.Save()
}

func (muf *MutableForest) GetImmutable(version int64) (*ImmutableForest, error) {
	commitsTree, err := muf.commitsTree.GetImmutable(version)
	if err != nil {
		return nil, fmt.Errorf("MutableForest.GetImmutable() could not get commits tree for version %d: %v",
			version, err)
	}
	return NewImmutableForest(commitsTree, muf.treeDB, muf.cacheSize)
}

// Calls to writer should be serialised as should writes to the tree
func (muf *MutableForest) Writer(prefix []byte) (*RWTree, error) {
	// Try dirty cache first (if tree is new it may only be in this location)
	prefixString := string(prefix)
	if tree, ok := muf.dirty[prefixString]; ok {
		return tree, nil
	}
	tree, err := muf.tree(prefix)
	if err != nil {
		return nil, err
	}
	// Mark tree as dirty
	muf.dirty[prefixString] = tree
	muf.dirtyPrefixes = append(muf.dirtyPrefixes, prefixString)
	return tree, nil
}

func (muf *MutableForest) IterateRWTree(start, end []byte, ascending bool, fn func(prefix []byte, tree *RWTree) error) error {
	return muf.commitsTree.Iterate(start, end, ascending, func(prefix []byte, _ []byte) error {
		rwt, err := muf.tree(prefix)
		if err != nil {
			return err
		}
		return fn(prefix, rwt)
	})
}

// Delete a tree - if the tree exists will return the CommitID of the latest saved version
func (muf *MutableForest) Delete(prefix []byte) (*CommitID, error) {
	bs, removed := muf.commitsTree.Delete(prefix)
	if !removed {
		return nil, nil
	}
	return unmarshalCommitID(bs)
}

// Get the current global hash for all trees in this forest
func (muf *MutableForest) Hash() []byte {
	return muf.commitsTree.Hash()
}

// Get the current global version for all versions of all trees in this forest
func (muf *MutableForest) Version() int64 {
	return muf.commitsTree.Version()
}

func (muf *MutableForest) saveTree(prefix []byte, tree *RWTree) error {
	hash, version, err := tree.Save()
	if err != nil {
		return fmt.Errorf("MutableForest.saveTree() could not save tree: %v", err)
	}
	return muf.setCommit(prefix, hash, version)
}

func (muf *MutableForest) setCommit(prefix, hash []byte, version int64) error {
	bs, err := marshalCommitID(hash, version)
	if err != nil {
		return fmt.Errorf("MutableForest.setCommit() could not marshal CommitID: %v", err)
	}
	muf.commitsTree.Set([]byte(prefix), bs)
	return nil
}

// ImmutableForest

// Get the tree at prefix for making reads
func (imf *ImmutableForest) Reader(prefix []byte) (KVCallbackIterableReader, error) {
	return imf.tree(prefix)
}

func (imf *ImmutableForest) Iterate(start, end []byte, ascending bool, fn func(prefix []byte, tree KVCallbackIterableReader) error) error {
	return imf.commitsTree.Iterate(start, end, ascending, func(prefix []byte, _ []byte) error {
		rwt, err := imf.tree(prefix)
		if err != nil {
			return err
		}
		return fn(prefix, rwt)
	})
}

func (imf *ImmutableForest) Dump() string {
	dump := treeprint.New()
	AddTreePrintTree("Commits", dump, imf.commitsTree)
	err := imf.Iterate(nil, nil, true, func(prefix []byte, tree KVCallbackIterableReader) error {
		AddTreePrintTree(string(prefix), dump, tree)
		return nil
	})
	if err != nil {
		return fmt.Sprintf("ImmutableForest.Dump(): iteration error: %v", err)
	}
	return dump.String()
}

// Shared implementation - these methods

// Lazy load tree
func (imf *ImmutableForest) tree(prefix []byte) (*RWTree, error) {
	// Try cache
	if value, ok := imf.treeCache.Get(string(prefix)); ok {
		return value.(*RWTree), nil
	}
	// Not in caches but non-negative version - we should be able to load into memory
	return imf.loadOrCreateTree(prefix)
}

func (imf *ImmutableForest) commitID(prefix []byte) (*CommitID, error) {
	bs, _ := imf.commitsTree.Get(prefix)
	if bs == nil {
		return new(CommitID), nil
	}
	commitID, err := unmarshalCommitID(bs)
	if err != nil {
		return nil, fmt.Errorf("could not get commitID for prefix %X: %v", prefix, err)
	}
	return commitID, nil
}

func (imf *ImmutableForest) loadOrCreateTree(prefix []byte) (*RWTree, error) {
	const errHeader = "ImmutableForest.loadOrCreateTree():"
	tree, err := imf.newTree(prefix)
	if err != nil {
		return nil, err
	}
	commitID, err := imf.commitID(prefix)
	if err != nil {
		return nil, fmt.Errorf("%s %v", errHeader, err)
	}
	if commitID.Version == 0 {
		// This is the first time we have been asked to load this tree
		return imf.newTree(prefix)
	}
	err = tree.Load(commitID.Version, imf.overwriting)
	if err != nil {
		return nil, fmt.Errorf("%s could not load tree: %v", errHeader, err)
	}
	return tree, nil
}

// Create a new in-memory IAVL tree
func (imf *ImmutableForest) newTree(prefix []byte) (*RWTree, error) {
	p := string(prefix)
	tree, err := NewRWTree(NewPrefixDB(imf.treeDB, p), imf.cacheSize)
	if err != nil {
		return nil, err
	}
	imf.treeCache.Add(p, tree)
	return tree, nil
}

// CommitID serialisation

func (cid *CommitID) UnmarshalBinary(data []byte) error {
	buf := proto.NewBuffer(data)
	return buf.Unmarshal(cid)
}

func (cid *CommitID) MarshalBinary() ([]byte, error) {
	buf := proto.NewBuffer(nil)
	err := buf.Marshal(cid)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (cid CommitID) String() string {
	return fmt.Sprintf("Commit{Hash: %v, Version: %v}", cid.Hash, cid.Version)
}

func marshalCommitID(hash []byte, version int64) ([]byte, error) {
	commitID := CommitID{
		Version: version,
		Hash:    hash,
	}
	bs, err := commitID.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("MarshalCommitID() could not encode CommitID %v: %v", commitID, err)
	}
	if bs == nil {
		// Normalise zero value to non-nil so we can store it IAVL tree without panic
		return []byte{}, nil
	}
	return bs, nil
}

func unmarshalCommitID(bs []byte) (*CommitID, error) {
	commitID := new(CommitID)
	err := commitID.UnmarshalBinary(bs)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal CommitID: %v", err)
	}
	return commitID, nil
}
