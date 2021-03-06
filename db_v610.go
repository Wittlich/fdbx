package fdbx

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/google/uuid"
)

const (
	flagGZip  = uint8(1 << 6)
	flagChunk = uint8(1 << 7)
)

func newV610db(c *v610Conn, tx fdb.Transaction) (*v610db, error) {
	return &v610db{conn: c, tx: tx}, nil
}

type v610db struct {
	conn *v610Conn
	tx   fdb.Transaction
}

// ********************** Public **********************

func (db *v610db) Get(typeID uint16, id []byte) ([]byte, error) {
	return db.tx.Get(fdbKey(db.conn.db, typeID, id)).Get()
}

func (db *v610db) Set(typeID uint16, id, value []byte) error {
	db.tx.Set(fdbKey(db.conn.db, typeID, id), value)
	return nil
}

func (db *v610db) Del(typeID uint16, id []byte) error {
	db.tx.Clear(fdbKey(db.conn.db, typeID, id))
	return nil
}

func (db *v610db) Save(onExists RecordHandler, recs ...Record) error {
	return saveRecords(db.conn.db, db.tx, onExists, recs...)
}

func (db *v610db) Load(onNotFound RecordHandler, recs ...Record) (err error) {
	return loadRecords(db.conn.db, db.tx, onNotFound, recs...)
}

func (db *v610db) Drop(onNotExists RecordHandler, recs ...Record) (err error) {
	keys := make(map[int]fdb.Key, len(recs))
	futures := make([]fdb.FutureByteSlice, len(recs))

	for i := range recs {
		keys[i] = recKey(db.conn.db, recs[i])
		futures[i] = db.tx.Get(keys[i])
	}

	for i := range futures {
		if err = dropRecord(db.conn.db, db.tx, recs[i], futures[i], onNotExists); err != nil {
			return
		}

		db.tx.Clear(keys[i])
	}

	return nil
}

func (db *v610db) Index(h IndexHandler, rid string, drop bool) (err error) {
	var idx *v610Indexer

	if idx, err = newV610Indexer(); err != nil {
		return
	}

	if err = h(idx); err != nil {
		return
	}

	return idx.commit(db.conn.db, db.tx, drop, rid)
}

func (db *v610db) ClearIndex(h IndexHandler) (err error) {
	var idx *v610Indexer

	if idx, err = newV610Indexer(); err != nil {
		return
	}

	if err = h(idx); err != nil {
		return
	}

	return idx.clear(db.conn.db, db.tx)
}

func (db *v610db) Clear(typeID uint16) error {
	return clearType(db.conn.db, typeID, db.tx)
}

func (db *v610db) Select(rtp RecordType, opts ...Option) ([]Record, error) {
	return selectRecords(db.conn.db, db.tx, &rtp, opts...)
}

func (db *v610db) SelectIDs(indexTypeID uint16, opts ...Option) ([]string, error) {
	return selectIDs(db.conn.db, indexTypeID, db.tx, opts...)
}

// *********** private ***********

func selectOpts(opts []Option) (opt *options, err error) {
	opt = new(options)

	for i := range opts {
		if err = opts[i](opt); err != nil {
			return
		}
	}

	if opt.from == nil {
		opt.from = []byte{0x00}
	}

	if opt.to == nil {
		opt.to = []byte{0xFF}
	}
	opt.to = append(opt.to, tail...)

	return opt, nil
}

func selectIDs(
	dbID, typeID uint16,
	rtx fdb.ReadTransaction,
	opts ...Option,
) (ids []string, err error) {
	var opt *options

	if opt, err = selectOpts(opts); err != nil {
		return
	}

	rng := fdb.KeyRange{Begin: fdbKey(dbID, typeID, opt.from), End: fdbKey(dbID, typeID, opt.to)}
	rngOpt := fdb.RangeOptions{Mode: fdb.StreamingModeSerial, Limit: opt.limit, Reverse: opt.reverse != nil}

	ids, _, err = getRangeIDs(rtx, rng, rngOpt)
	return
}

func selectRecords(
	dbID uint16,
	rtx fdb.ReadTransaction,
	rtp *RecordType,
	opts ...Option,
) (list []Record, err error) {
	var opt *options

	if opt, err = selectOpts(opts); err != nil {
		return
	}

	rng := fdb.KeyRange{Begin: fdbKey(dbID, rtp.ID, opt.from), End: fdbKey(dbID, rtp.ID, opt.to)}
	rngOpt := fdb.RangeOptions{Mode: fdb.StreamingModeSerial, Limit: opt.limit, Reverse: opt.reverse != nil}

	list, _, err = getRange(dbID, rtx, rng, rngOpt, rtp, opt.filter, false)
	return
}

func loadRecords(dbID uint16, rtx fdb.ReadTransaction, onNotFound RecordHandler, recs ...Record) (err error) {
	// query all futures to leverage wait time
	futures := make([]fdb.FutureByteSlice, len(recs))
	for i := range recs {
		futures[i] = rtx.Get(recKey(dbID, recs[i]))
	}

	for i := range futures {
		if err = loadRecord(dbID, rtx, recs[i], futures[i], onNotFound); err != nil {
			return
		}
	}

	return nil
}

func loadRecord(
	dbID uint16,
	rtx fdb.ReadTransaction,
	rec Record,
	fb fdb.FutureByteSlice,
	onNotFound RecordHandler,
) (err error) {
	var buf []byte

	if buf, err = fb.Get(); err != nil {
		return
	}

	if len(buf) == 0 {
		if onNotFound != nil {
			return onNotFound(rec)
		}
		return ErrRecordNotFound.WithStack()
	}

	if _, buf, err = unpackValue(dbID, rtx, buf); err != nil {
		return
	}

	return rec.FdbxUnmarshal(buf)
}

func saveRecords(dbID uint16, tx fdb.Transaction, onExists RecordHandler, recs ...Record) (err error) {
	var key fdb.Key

	fbs := make([]fdb.FutureByteSlice, len(recs))
	keys := make([]fdb.Key, len(recs))

	for i := range recs {
		key = recKey(dbID, recs[i])
		fbs[i] = tx.Get(key)
		keys[i] = key
	}

	for i := range recs {
		if err = saveRecord(dbID, tx, fbs[i], keys[i], recs[i], onExists); err != nil {
			return
		}
	}

	return nil
}

func saveRecord(
	dbID uint16,
	tx fdb.Transaction,
	fb fdb.FutureByteSlice,
	key fdb.Key,
	rec Record,
	onExists RecordHandler,
) (err error) {
	var buf []byte

	if buf, err = fb.Get(); err != nil {
		return
	}

	if len(buf) > 0 {
		if onExists != nil {
			if err = onExists(rec); err != nil {
				return
			}
		}

		if _, buf, err = unpackValue(dbID, tx, buf); err != nil {
			return
		}

		if err = setIndexes(dbID, tx, rec, buf, true); err != nil {
			return
		}
	}

	if buf, err = rec.FdbxMarshal(); err != nil {
		return
	}

	if err = setIndexes(dbID, tx, rec, buf, false); err != nil {
		return
	}

	if buf, err = packValue(dbID, tx, buf); err != nil {
		return
	}

	tx.Set(key, buf)
	return nil
}

func dropRecord(
	dbID uint16,
	tx fdb.Transaction,
	rec Record,
	fb fdb.FutureByteSlice,
	onNotExists RecordHandler,
) (err error) {
	var buf, blobID []byte

	if buf, err = fb.Get(); err != nil {
		return
	}

	if len(buf) == 0 {
		if onNotExists != nil {
			return onNotExists(rec)
		}
		return nil
	}

	if blobID, buf, err = unpackValue(dbID, tx, buf); err != nil {
		return
	}

	if blobID != nil {
		if err = dropBlob(dbID, tx, blobID); err != nil {
			return
		}
	}

	return setIndexes(dbID, tx, rec, buf, true)
}

func setIndexes(dbID uint16, tx fdb.Transaction, rec Record, buf []byte, drop bool) (err error) {
	var rcp Record
	var idx *v610Indexer

	rid := rec.FdbxID()

	if idx, err = newV610Indexer(); err != nil {
		return
	}

	if drop {
		if rcp, err = rec.FdbxType().New(rid); err != nil {
			return
		}

		if err = rcp.FdbxUnmarshal(buf); err != nil {
			return
		}
	} else {
		rcp = rec
	}

	if err = rcp.FdbxIndex(idx); err != nil {
		return
	}

	return idx.commit(dbID, tx, drop, rid)
}

func packValue(dbID uint16, tx fdb.Transaction, value []byte) (_ []byte, err error) {
	var flags uint8

	// so long, try to reduce
	if len(value) > GZipSize {
		if value, err = gzipValue(&flags, value); err != nil {
			return
		}
	}

	// sooooooo long, we must split and save as blob
	if len(value) > ChunkSize {
		if value, err = saveBlob(dbID, tx, &flags, value); err != nil {
			return
		}
	}

	return append([]byte{flags}, value...), nil
}

func unpackValue(dbID uint16, rtx fdb.ReadTransaction, value []byte) (blobID, buffer []byte, err error) {
	flags := value[0]
	buffer = value[1:]

	// blob data
	if flags&flagChunk > 0 {
		blobID = buffer

		if buffer, err = loadBlob(dbID, rtx, buffer); err != nil {
			return
		}
	}

	// gzip data
	if flags&flagGZip > 0 {
		if buffer, err = gunzipValue(buffer); err != nil {
			return
		}
	}

	return blobID, buffer, nil
}

func loadBlob(dbID uint16, rtx fdb.ReadTransaction, value []byte) (blob []byte, err error) {
	var kv fdb.KeyValue

	res := rtx.GetRange(fdb.KeyRange{
		Begin: fdbKey(dbID, ChunkTypeID, value),
		End:   fdbKey(dbID, ChunkTypeID, value, []byte{0xFF}),
	}, fdb.RangeOptions{Mode: fdb.StreamingModeSerial}).Iterator()

	for res.Advance() {
		if kv, err = res.Get(); err != nil {
			return
		}
		blob = append(blob, kv.Value...)
	}
	return blob, nil
}

func saveBlob(dbID uint16, tx fdb.Transaction, flags *uint8, blob []byte) (value []byte, err error) {
	var i uint16
	var last bool
	var part []byte
	var index [2]byte

	*flags |= flagChunk
	blobID := uuid.New()

	// TODO: only up to 10M (transaction size)
	// split into multiple goroutines for speed
	for !last {
		// check tail
		if len(blob) <= ChunkSize {
			last = true
			part = blob
		} else {
			part = blob[:ChunkSize]
			blob = blob[ChunkSize:]
		}

		// save part
		binary.BigEndian.PutUint16(index[:], i)
		tx.Set(fdbKey(dbID, ChunkTypeID, blobID[:], index[:]), part)
		i++
	}

	return blobID[:], nil
}

func dropBlob(dbID uint16, tx fdb.Transaction, value []byte) error {
	tx.ClearRange(fdb.KeyRange{
		Begin: fdbKey(dbID, ChunkTypeID, value),
		End:   fdbKey(dbID, ChunkTypeID, value, []byte{0xFF}),
	})
	return nil
}

func gzipValue(flags *uint8, value []byte) ([]byte, error) {
	*flags |= flagGZip

	// TODO: sync.Pool
	buf := new(bytes.Buffer)

	if err := gzipStream(buf, bytes.NewReader(value)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func gunzipValue(value []byte) ([]byte, error) {
	// TODO: sync.Pool
	buf := new(bytes.Buffer)

	if err := gunzipStream(buf, bytes.NewReader(value)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func gzipStream(w io.Writer, r io.Reader) (err error) {
	gw := gzip.NewWriter(w)

	defer func() {
		e := gw.Close()
		if err == nil {
			err = e
		}
	}()

	if _, err = io.Copy(gw, r); err != nil {
		return ErrMemFail.WithReason(err)
	}

	return nil
}

func gunzipStream(w io.Writer, r io.Reader) (err error) {
	var gr *gzip.Reader

	if gr, err = gzip.NewReader(r); err != nil {
		return ErrInvalidGZ.WithReason(err)
	}

	defer func() {
		e := gr.Close()
		if err == nil {
			err = e
		}
	}()

	if _, err = io.Copy(w, gr); err != nil {
		return ErrMemFail.WithReason(err)
	}

	return nil
}

func getRowID(key fdb.Key) string {
	klen := len(key) - 1
	return B2S(key[klen-int(key[klen]) : klen])
}

func getRangeIDs(
	rtx fdb.ReadTransaction,
	rng fdb.Range,
	opt fdb.RangeOptions,
) (ids []string, lastKey fdb.Key, err error) {
	rows := rtx.GetRange(rng, opt).GetSliceOrPanic()
	ids = make([]string, 0, len(rows))

	for i := range rows {

		if opt.Reverse {
			// first key is the last key for reverse
			if i == 0 {
				lastKey = rows[i].Key
			}
		} else {
			lastKey = rows[i].Key
		}

		ids = append(ids, getRowID(rows[i].Key))
	}

	return ids, lastKey, nil
}

func getRange(
	dbID uint16,
	rtx fdb.ReadTransaction,
	rng fdb.KeyRange,
	opt fdb.RangeOptions,
	rtp *RecordType,
	chk Predicat,
	rev bool,
) (list []Record, lastKey fdb.Key, err error) {
	var blrec Record
	var rcbuf []byte
	var batch []fdb.KeyValue
	var futsb []fdb.FutureByteSlice
	var blrng fdb.KeyRange
	var blkey *bytes.Buffer

	first := true
	bsize := 1000
	limit := opt.Limit
	opt.Mode = fdb.StreamingModeSerial

	if opt.Reverse {
		blkey = bytes.NewBuffer([]byte(rng.End.FDBKey()))
	} else {
		blkey = bytes.NewBuffer([]byte(rng.Begin.FDBKey()))
	}
	blkey.Grow(len(tail))

	// batch size shouldn't be more then limit
	if limit < bsize {
		bsize = limit
	}

	// better split on batches when custom filter
	if chk != nil {
		opt.Limit = bsize
	} else {
		bsize = opt.Limit
	}

	// load records in batches
	for limit == 0 || len(list) < limit {

		if opt.Reverse {
			blrng = fdb.KeyRange{Begin: rng.Begin, End: fdb.Key(blkey.Bytes())}
		} else {
			blrng = fdb.KeyRange{Begin: fdb.Key(blkey.Bytes()), End: rng.End}
		}

		// zero length means last batch
		if batch = rtx.GetRange(blrng, opt).GetSliceOrPanic(); len(batch) == 0 {
			break
		}

		// batch data loading if only ids
		if len(batch[0].Value) == 0 {

			// make futures batch
			if len(futsb) < len(batch) {
				futsb = make([]fdb.FutureByteSlice, len(batch))
			} else {
				futsb = futsb[:len(batch)]
			}

			// get record type id
			if blrec, err = rtp.New(getRowID(batch[0].Key)); err != nil {
				return
			}

			// query all futures to leverage wait time
			for i := range batch {
				futsb[i] = rtx.Get(recTypeKey(dbID, blrec.FdbxType().ID, getRowID(batch[i].Key)))
			}

			// wait all values
			for i := range batch {
				if batch[i].Value, err = futsb[i].Get(); err != nil {
					return
				}
			}
		}

		// record filtering
		for i := range batch {
			if limit > 0 && len(list) >= limit {
				break
			}

			blkey.Reset()
			blkey.Grow(len(batch[i].Key) + len(tail))
			blkey.Write([]byte(batch[i].Key))
			if !opt.Reverse {
				blkey.Write(tail)
			}

			if rev {
				if first {
					if opt.Reverse {
						lastKey = fdb.Key(append(batch[0].Key, tail...))
					} else {
						lastKey = batch[0].Key
					}
					first = false
				}
			} else {
				lastKey = fdb.Key(blkey.Bytes())
			}

			if blrec, err = rtp.New(getRowID(batch[i].Key)); err != nil {
				return
			}

			if _, rcbuf, err = unpackValue(dbID, rtx, batch[i].Value); err != nil {
				return
			}

			if err = blrec.FdbxUnmarshal(rcbuf); err != nil {
				return
			}

			add := true

			if chk != nil {
				if add, err = chk(blrec); err != nil {
					return
				}
			}

			if add {
				list = append(list, blrec)
			}
		}

	}

	return list, lastKey, nil
}

func clearType(dbID, typeID uint16, tx fdb.Transaction) error {
	tx.ClearRange(fdb.KeyRange{
		Begin: fdbKey(dbID, typeID),
		End:   fdbKey(dbID, typeID, tail),
	})
	return nil
}
