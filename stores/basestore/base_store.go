package basestore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	ipfslog "berty.tech/go-ipfs-log"
	logac "berty.tech/go-ipfs-log/accesscontroller"
	"berty.tech/go-ipfs-log/entry"
	"berty.tech/go-ipfs-log/identityprovider"
	"berty.tech/go-ipfs-log/io"
	"berty.tech/go-orbit-db/accesscontroller"
	"berty.tech/go-orbit-db/accesscontroller/simple"
	"berty.tech/go-orbit-db/address"
	"berty.tech/go-orbit-db/events"
	"berty.tech/go-orbit-db/iface"
	"berty.tech/go-orbit-db/stores"
	"berty.tech/go-orbit-db/stores/operation"
	"berty.tech/go-orbit-db/stores/replicator"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	files "github.com/ipfs/go-ipfs-files"
	coreapi "github.com/ipfs/interface-go-ipfs-core"
	"github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// BaseStore The base of other stores
type BaseStore struct {
	events.EventEmitter

	id                string
	identity          *identityprovider.Identity
	address           address.Address
	dbName            string
	ipfs              coreapi.CoreAPI
	cache             datastore.Datastore
	access            accesscontroller.Interface
	oplog             ipfslog.Log
	replicator        replicator.Replicator
	storeType         string
	index             iface.StoreIndex
	replicationStatus replicator.ReplicationInfo
	loader            replicator.Replicator
	onClose           func(address.Address)
	stats             struct {
		snapshot struct {
			bytesLoaded int
		}
		syncRequestsReceived int
	}
	referenceCount int
	replicate      bool
	directory      string
	options        *iface.NewStoreOptions
	cacheDestroy   func() error

	lock sync.RWMutex
}

func (b *BaseStore) DBName() string {
	return b.dbName
}

func (b *BaseStore) IPFS() coreapi.CoreAPI {
	return b.ipfs
}

func (b *BaseStore) Identity() *identityprovider.Identity {
	return b.identity
}

func (b *BaseStore) OpLog() ipfslog.Log {
	b.lock.RLock()
	defer b.lock.RUnlock()

	return b.oplog
}

func (b *BaseStore) AccessController() accesscontroller.Interface {
	return b.access
}

// InitBaseStore Initializes the store base
func (b *BaseStore) InitBaseStore(ctx context.Context, ipfs coreapi.CoreAPI, identity *identityprovider.Identity, addr address.Address, options *iface.NewStoreOptions) error {
	var err error

	if identity == nil {
		return errors.New("identity required")
	}

	b.storeType = "store"
	b.id = addr.String()
	b.identity = identity
	b.address = addr
	b.dbName = addr.GetPath()
	b.ipfs = ipfs
	b.cache = options.Cache
	b.cacheDestroy = options.CacheDestroy
	if options.AccessController != nil {
		b.access = options.AccessController
	} else {
		manifestParams := accesscontroller.NewManifestParams(cid.Cid{}, true, "simple")
		manifestParams.SetAccess("write", []string{identity.ID})
		b.access, err = simple.NewSimpleAccessController(ctx, nil, manifestParams)

		if err != nil {
			return errors.Wrap(err, "unable to create simple access controller")
		}
	}

	b.lock.Lock()
	b.oplog, err = ipfslog.NewLog(ipfs, identity, &ipfslog.LogOptions{
		ID:               b.id,
		AccessController: b.access,
	})
	b.lock.Unlock()

	if err != nil {
		return errors.New("unable to instantiate an IPFS log")
	}

	if options.Index == nil {
		options.Index = NewBaseIndex
	}

	b.index = options.Index(b.identity.PublicKey)
	b.replicationStatus = replicator.NewReplicationInfo()

	b.lock.Lock()
	b.stats.snapshot.bytesLoaded = -1

	b.replicator = replicator.NewReplicator(ctx, b, options.ReplicationConcurrency)
	b.loader = b.replicator

	b.referenceCount = 64
	if options.ReferenceCount != nil {
		b.referenceCount = *options.ReferenceCount
	}

	b.directory = "./orbitdb"
	if options.Directory != "" {
		b.directory = options.Directory
	}

	b.replicate = true
	if options.Replicate != nil {
		b.replicate = *options.Replicate
	}

	b.options = options
	b.lock.Unlock()

	go b.replicator.Subscribe(ctx, func(e events.Event) {
		switch e.(type) {
		case *replicator.EventLoadAdded:
			evt := e.(*replicator.EventLoadAdded)
			b.replicationLoadAdded(evt.Hash)
			b.replicationStatus.IncQueued()

		case *replicator.EventLoadEnd:
			evt := e.(*replicator.EventLoadEnd)
			b.replicationLoadComplete(evt.Logs)

		case *replicator.EventLoadProgress:
			evt := e.(*replicator.EventLoadProgress)

			if b.replicationStatus.GetBuffered() > evt.BufferLength {
				b.recalculateReplicationProgress(b.replicationStatus.GetProgress() + evt.BufferLength)
			} else {
				b.lock.RLock()
				oplog := b.oplog
				b.lock.RUnlock()
				b.recalculateReplicationProgress(oplog.Values().Len() + evt.BufferLength)
			}

			b.replicationStatus.SetBuffered(evt.BufferLength)
			b.recalculateReplicationMax(b.replicationStatus.GetProgress())
			// logger.debug(`<replicate.progress>`)
			b.Emit(stores.NewEventReplicateProgress(b.Address(), evt.Hash, evt.Latest, b.replicationStatus))
		}
	})

	return nil
}

func (b *BaseStore) replicationLoadAdded(e cid.Cid) {
	// TODO
	//b.replicationStatus.IncQueued()
	//b.recalculateReplicationMax(e.Clock.Time)
	//logger().Debug("<replicate>")
	//b.Emit(stores.NewEventReplicate(b.address, e))
}

func (b *BaseStore) Close() error {
	if b.onClose != nil {
		logger().Debug("\nCLOSING. OnClose\n")
		b.onClose(b.address)
	}

	// Replicator teardown logic
	b.replicator.Stop()

	// Reset replication statistics
	b.replicationStatus.Reset()

	b.lock.Lock()
	// Reset database statistics
	b.stats.snapshot.bytesLoaded = -1
	b.stats.syncRequestsReceived = 0
	b.lock.Unlock()

	b.Emit(stores.NewEventClosed(b.address))

	b.UnsubscribeAll()

	err := b.cache.Close()
	if err != nil {
		return errors.Wrap(err, "unable to close cache")
	}

	return nil
}

func (b *BaseStore) Address() address.Address {
	return b.address
}

func (b *BaseStore) Index() iface.StoreIndex {
	return b.index
}

func (b *BaseStore) Type() string {
	return b.storeType
}

func (b *BaseStore) ReplicationStatus() replicator.ReplicationInfo {
	return b.replicationStatus
}

func (b *BaseStore) Drop() error {
	var err error
	if err = b.Close(); err != nil {
		return errors.Wrap(err, "unable to close store")
	}

	err = b.cacheDestroy()
	if err != nil {
		return errors.Wrap(err, "unable to destroy cache")
	}

	// TODO: Destroy cache? b.cache.Delete()

	// Reset
	b.index = b.options.Index(b.identity.PublicKey)
	b.lock.Lock()
	b.oplog, err = ipfslog.NewLog(b.ipfs, b.identity, &ipfslog.LogOptions{
		ID:               b.id,
		AccessController: b.access,
	})
	b.lock.Unlock()

	if err != nil {
		return errors.Wrap(err, "unable to create log")
	}

	b.cache = b.options.Cache

	return nil
}

func (b *BaseStore) Load(ctx context.Context, amount int) error {
	if amount <= 0 && b.options.MaxHistory != nil {
		amount = *b.options.MaxHistory
	}

	var localHeads, remoteHeads []*entry.Entry
	localHeadsBytes, err := b.cache.Get(datastore.NewKey("_localHeads"))
	if err != nil {
		return errors.Wrap(err, "unable to get local heads from cache")
	}

	err = json.Unmarshal(localHeadsBytes, &localHeads)
	if err != nil {
		return errors.Wrap(err, "unable to unmarshal cached local heads")
	}

	remoteHeadsBytes, err := b.cache.Get(datastore.NewKey("_remoteHeads"))
	if err != nil && err != datastore.ErrNotFound {
		return errors.Wrap(err, "unable to get data from cache")
	}

	if remoteHeadsBytes != nil {
		err = json.Unmarshal(remoteHeadsBytes, &remoteHeads)
		if err != nil {
			return errors.Wrap(err, "unable to unmarshal cached remote heads")
		}
	}

	heads := append(localHeads, remoteHeads...)

	if len(heads) > 0 {
		headsForEvent := make([]ipfslog.Entry, len(heads))
		for i := range heads {
			headsForEvent[i] = heads[i]
		}

		b.Emit(stores.NewEventLoad(b.address, headsForEvent))
	}

	for _, h := range heads {
		var l ipfslog.Log

		// TODO: parallelize things
		b.recalculateReplicationMax(h.GetClock().GetTime())
		b.lock.RLock()
		oplog := b.oplog
		b.lock.RUnlock()

		l, err := ipfslog.NewFromEntryHash(ctx, b.ipfs, b.identity, h.GetHash(), &ipfslog.LogOptions{
			ID:               oplog.GetID(),
			AccessController: b.access,
		}, &ipfslog.FetchOptions{
			Length:  &amount,
			Exclude: oplog.Values().Slice(),
			// TODO: ProgressChan:  this._onLoadProgress.bind(this),
		})

		if err != nil {
			return errors.Wrap(err, "unable to create log from entry hash")
		}

		l, err = oplog.Join(l, amount)
		if err != nil {
			return errors.Wrap(err, "unable to join log")
		}

		b.lock.Lock()
		b.oplog = l
		b.lock.Unlock()
	}

	// Update the index
	if len(heads) > 0 {
		if err := b.updateIndex(); err != nil {
			return errors.Wrap(err, "unable to update index")
		}
	}

	b.lock.RLock()
	oplog := b.oplog
	b.lock.RUnlock()

	b.Emit(stores.NewEventReady(b.address, oplog.Heads().Slice()))
	return nil
}

func (b *BaseStore) Sync(ctx context.Context, heads []ipfslog.Entry) error {
	b.lock.Lock()
	b.stats.syncRequestsReceived++
	b.lock.Unlock()

	if len(heads) == 0 {
		return nil
	}

	var savedEntriesCIDs []cid.Cid

	for _, h := range heads {
		if h == nil {
			logger().Debug("warning: Given input entry was 'null'.")
			continue
		}

		if h.GetNext() == nil {
			h.SetNext([]cid.Cid{})
		}

		identityProvider := b.identity.Provider
		if identityProvider == nil {
			return errors.New("identity-provider is required, cannot verify entry")
		}

		b.lock.RLock()
		oplog := b.oplog
		b.lock.RUnlock()

		canAppend := b.access.CanAppend(h, identityProvider, &CanAppendContext{log: oplog})
		if canAppend != nil {
			logger().Debug("warning: Given input entry is not allowed in this log and was discarded (no write access).")
			continue
		}

		hash, err := io.WriteCBOR(ctx, b.ipfs, h.ToCborEntry())
		if err != nil {
			return errors.Wrap(err, "unable to write entry on dag")
		}

		if hash.String() != h.GetHash().String() {
			return errors.New("WARNING! Head hash didn't match the contents")
		}

		savedEntriesCIDs = append(savedEntriesCIDs, hash)
	}

	b.replicator.Load(ctx, savedEntriesCIDs)

	return nil
}

func (b *BaseStore) LoadMoreFrom(ctx context.Context, amount uint, cids []cid.Cid) {
	b.replicator.Load(ctx, cids)
	// TODO: can this return an error?
}

type storeSnapshot struct {
	ID    string         `json:"id,omitempty"`
	Heads []*entry.Entry `json:"heads,omitempty"`
	Size  int            `json:"size,omitempty"`
	Type  string         `json:"type,omitempty"`
}

func (b *BaseStore) SaveSnapshot(ctx context.Context) (cid.Cid, error) {
	// @glouvigny: I'd rather use protobuf here but I decided to keep the
	// JS behavior for the sake of compatibility across implementations
	// TODO: avoid using `*entry.Entry`?

	unfinished := b.replicator.GetQueue()

	b.lock.RLock()
	oplog := b.oplog
	b.lock.RUnlock()

	untypedEntries := oplog.Heads().Slice()
	entries := make([]*entry.Entry, len(untypedEntries))
	for i := range untypedEntries {
		castedEntry, ok := untypedEntries[i].(*entry.Entry)
		if !ok {
			return cid.Cid{}, errors.New("unable to downcast entry")
		}

		entries[i] = castedEntry
	}

	header, err := json.Marshal(&storeSnapshot{
		ID:    oplog.GetID(),
		Heads: entries,
		Size:  oplog.Values().Len(),
		Type:  b.storeType,
	})

	if err != nil {
		return cid.Cid{}, errors.Wrap(err, "unable to serialize snapshot")
	}

	headerSize := len(header)

	size := make([]byte, 2)
	binary.BigEndian.PutUint16(size, uint16(headerSize))
	rs := append(size, header...)

	b.lock.RLock()
	oplog = b.oplog
	b.lock.RUnlock()

	for _, e := range oplog.Values().Slice() {
		entryJSON, err := json.Marshal(e)

		if err != nil {
			return cid.Cid{}, errors.Wrap(err, "unable to serialize entry as JSON")
		}

		size := make([]byte, 2)
		binary.BigEndian.PutUint16(size, uint16(len(entryJSON)))

		rs = append(rs, size...)
		rs = append(rs, entryJSON...)
	}

	rs = append(rs, 0)

	rsFileNode := files.NewBytesFile(rs)

	snapshotPath, err := b.ipfs.Unixfs().Add(ctx, rsFileNode)
	if err != nil {
		return cid.Cid{}, errors.Wrap(err, "unable to save log data on store")
	}

	err = b.cache.Put(datastore.NewKey("snapshot"), []byte(snapshotPath.Cid().String()))
	if err != nil {
		return cid.Cid{}, errors.Wrap(err, "unable to add snapshot data to cache")
	}

	unfinishedJSON, err := json.Marshal(unfinished)
	if err != nil {
		return cid.Cid{}, errors.Wrap(err, "unable to marshal unfinished cids")
	}

	err = b.cache.Put(datastore.NewKey("queue"), unfinishedJSON)
	if err != nil {
		return cid.Cid{}, errors.Wrap(err, "unable to add unfinished data to cache")
	}

	logger().Debug(fmt.Sprintf(`Saved snapshot: %s, queue length: %d`, snapshotPath.String(), len(unfinished)))

	return snapshotPath.Cid(), nil
}

func (b *BaseStore) LoadFromSnapshot(ctx context.Context) error {
	b.Emit(stores.NewEventLoad(b.address, nil))

	queueJSON, err := b.cache.Get(datastore.NewKey("queue"))
	if err != nil && err != datastore.ErrNotFound {
		return errors.Wrap(err, "unable to get value from cache")
	}

	if err != datastore.ErrNotFound {
		_ = queueJSON
		var queue []cid.Cid

		var entries []ipfslog.Entry

		if err := json.Unmarshal(queueJSON, &queue); err != nil {
			return errors.Wrap(err, "unable to deserialize queued CIDs")
		}

		for _, h := range queue {
			entries = append(entries, &entry.Entry{Hash: h})
		}

		if err := b.Sync(ctx, entries); err != nil {
			return errors.Wrap(err, "unable to sync queued CIDs")
		}
	}

	snapshot, err := b.cache.Get(datastore.NewKey("snapshot"))
	if err == datastore.ErrNotFound {
		return errors.Wrap(err, "not found")
	}

	if err != nil {
		return errors.Wrap(err, "unable to get value from cache")
	}

	logger().Debug("loading snapshot from path", zap.String("snapshot", string(snapshot)))

	resNode, err := b.ipfs.Unixfs().Get(ctx, path.New(string(snapshot)))
	if err != nil {
		return errors.Wrap(err, "unable to get snapshot from ipfs")
	}

	res, ok := resNode.(files.File)
	if !ok {
		return errors.New("unable to cast fetched data as a file")
	}

	headerLengthRaw := make([]byte, 2)
	if _, err := res.Read(headerLengthRaw); err != nil {
		return errors.Wrap(err, "unable to read from stream")
	}

	headerLength := binary.BigEndian.Uint16(headerLengthRaw)
	header := &storeSnapshot{}
	headerRaw := make([]byte, headerLength)
	if _, err := res.Read(headerRaw); err != nil {
		return errors.Wrap(err, "unable to read from stream")
	}

	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return errors.Wrap(err, "unable to decode header from ipfs data")
	}

	var entries []ipfslog.Entry
	maxClock := 0

	for i := 0; i < header.Size; i++ {
		entryLengthRaw := make([]byte, 2)
		if _, err := res.Read(entryLengthRaw); err != nil {
			return errors.Wrap(err, "unable to read from stream")
		}

		entryLength := binary.BigEndian.Uint16(entryLengthRaw)
		e := &entry.Entry{}
		entryRaw := make([]byte, entryLength)

		if _, err := res.Read(entryRaw); err != nil {
			return errors.Wrap(err, "unable to read from stream")
		}

		logger().Debug(fmt.Sprintf("Entry raw: %s", string(entryRaw)))

		if err = json.Unmarshal(entryRaw, e); err != nil {
			return errors.Wrap(err, "unable to unmarshal entry from ipfs data")
		}

		entries = append(entries, e)
		if maxClock < e.Clock.GetTime() {
			maxClock = e.Clock.GetTime()
		}
	}

	b.recalculateReplicationMax(maxClock)

	var headsCids []cid.Cid
	for _, h := range header.Heads {
		headsCids = append(headsCids, h.GetHash())
	}

	log, err := ipfslog.NewFromJSON(ctx, b.ipfs, b.identity, &ipfslog.JSONLog{
		Heads: headsCids,
		ID:    header.ID,
	}, &ipfslog.LogOptions{
		Entries:          entry.NewOrderedMapFromEntries(entries),
		ID:               header.ID,
		AccessController: b.access,
	}, &entry.FetchOptions{
		Length:  intPtr(-1),
		Timeout: time.Second,
	})

	if err != nil {
		return errors.Wrap(err, "unable to load log")
	}

	b.lock.RLock()
	oplog := b.oplog
	b.lock.RUnlock()

	if _, err = oplog.Join(log, -1); err != nil {
		return errors.Wrap(err, "unable to join log")
	}

	if err := b.updateIndex(); err != nil {
		return errors.Wrap(err, "unable to update index")
	}

	return nil
}

func intPtr(i int) *int {
	return &i
}

func (b *BaseStore) AddOperation(ctx context.Context, op operation.Operation, onProgressCallback chan<- ipfslog.Entry) (ipfslog.Entry, error) {
	data, err := op.Marshal()
	if err != nil {
		return nil, errors.Wrap(err, "unable to marshal operation")
	}

	b.lock.RLock()
	oplog := b.oplog
	b.lock.RUnlock()

	e, err := oplog.Append(ctx, data, b.referenceCount)
	if err != nil {
		return nil, errors.Wrap(err, "unable to append data on log")
	}
	b.recalculateReplicationStatus(b.replicationStatus.GetProgress()+1, e.GetClock().GetTime())

	marshaledEntry, err := json.Marshal([]ipfslog.Entry{e})
	if err != nil {
		return nil, errors.Wrap(err, "unable to marshal entry")
	}

	err = b.cache.Put(datastore.NewKey("_localHeads"), marshaledEntry)
	if err != nil {
		return nil, errors.Wrap(err, "unable to add data to cache")
	}

	if err := b.updateIndex(); err != nil {
		return nil, errors.Wrap(err, "unable to update index")
	}

	b.Emit(stores.NewEventWrite(b.address, e, oplog.Heads().Slice()))

	if onProgressCallback != nil {
		onProgressCallback <- e
	}

	return e, nil
}

func (b *BaseStore) recalculateReplicationProgress(max int) {
	b.lock.RLock()
	oplog := b.oplog
	b.lock.RUnlock()

	if valuesLen := oplog.Values().Len(); b.replicationStatus.GetProgress() < valuesLen {
		b.replicationStatus.SetProgress(valuesLen)

	} else if b.replicationStatus.GetProgress() < max {
		b.replicationStatus.SetProgress(max)
	}

	b.recalculateReplicationMax(b.replicationStatus.GetProgress())
}

func (b *BaseStore) recalculateReplicationMax(max int) {
	b.lock.RLock()
	oplog := b.oplog
	b.lock.RUnlock()

	if valuesLen := oplog.Values().Len(); b.replicationStatus.GetMax() < valuesLen {
		b.replicationStatus.SetMax(valuesLen)

	} else if b.replicationStatus.GetMax() < max {
		b.replicationStatus.SetMax(max)
	}
}

func (b *BaseStore) recalculateReplicationStatus(maxProgress, maxTotal int) {
	b.recalculateReplicationProgress(maxProgress)
	b.recalculateReplicationMax(maxTotal)
}

func (b *BaseStore) updateIndex() error {
	b.lock.RLock()
	oplog := b.oplog
	b.lock.RUnlock()

	b.recalculateReplicationMax(0)
	if err := b.index.UpdateIndex(oplog, []ipfslog.Entry{}); err != nil {
		return errors.Wrap(err, "unable to update index")
	}
	b.recalculateReplicationProgress(0)

	return nil
}

func (b *BaseStore) replicationLoadComplete(logs []ipfslog.Log) {
	b.lock.RLock()
	oplog := b.oplog
	b.lock.RUnlock()

	logger().Debug("replication load complete")
	for _, log := range logs {
		_, err := oplog.Join(log, -1)
		if err != nil {
			logger().Error("unable to join logs", zap.Error(err))
			return
		}
	}
	b.replicationStatus.DecreaseQueued(len(logs))
	b.replicationStatus.SetBuffered(b.replicator.GetBufferLen())
	err := b.updateIndex()
	if err != nil {
		logger().Error("unable to update index", zap.Error(err))
		return
	}

	// only store heads that has been verified and merges
	heads := oplog.Heads()

	headsBytes, err := json.Marshal(heads.Slice())
	if err != nil {
		logger().Error("unable to serialize heads cache", zap.Error(err))
		return
	}

	err = b.cache.Put(datastore.NewKey("_remoteHeads"), headsBytes)
	if err != nil {
		logger().Error("unable to update heads cache", zap.Error(err))
		return
	}

	logger().Debug(fmt.Sprintf("Saved heads %d", heads.Len()))

	// logger.debug(`<replicated>`)
	b.Emit(stores.NewEventReplicated(b.address, len(logs)))
}

type CanAppendContext struct {
	log ipfslog.Log
}

func (c *CanAppendContext) GetLogEntries() []logac.LogEntry {
	logEntries := c.log.GetEntries().Slice()

	var entries = make([]logac.LogEntry, len(logEntries))
	for i := range logEntries {
		entries[i] = logEntries[i]
	}

	return entries
}

var _ iface.Store = &BaseStore{}
