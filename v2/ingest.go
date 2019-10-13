package pdk

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"

	"github.com/pilosa/go-pilosa"
	"github.com/pilosa/go-pilosa/gpexp"
	"github.com/pilosa/pilosa/logger"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// TODO Jaeger
// TODO profiling endpoint
// TODO Prometheus

// Main holds all config for general ingest
type Main struct {
	PilosaHosts      []string `help:"Comma separated list of host:port pairs for Pilosa."`
	BatchSize        int      `help:"Number of records to read before indexing all of them at once. Generally, larger means better throughput and more memory usage. 1,048,576 might be a good number."`
	Index            string   `help:"Name of Pilosa index."`
	LogPath          string   `help:"Log file to write to. Empty means stderr. TODO implement."`
	PrimaryKeyFields []string `help:"Data field(s) which make up the primary key for a record. These will be concatenated and translated to a Pilosa ID. If empty, record key translation will not be used."`
	IDField          string   `help:"Field which contains the integer column ID. May not be used in conjunction with primary-key-fields. If both are empty, auto-generated IDs will be used."`
	MaxMsgs          int      `help:"Number of messages to consume from Kafka before stopping. Useful for testing when you don't want to run indefinitely."`
	Concurrency      int      `help:"Number of concurrent kafka readers and indexing routines to launch. MaxMsgs will be read *from each*."`
	PackBools        string   `help:"If non-empty, boolean fields will be packed into two set fields—one with this name, and one with <name>-exists."`
	Verbose          bool     `help:"Enable verbose logging."`
	// TODO implement the auto-generated IDs... hopefully using Pilosa to manage it.

	NewSource func() (Source, error) `flag:"-"`

	client *pilosa.Client
	schema *pilosa.Schema
	index  *pilosa.Index

	log logger.Logger
}

func (m *Main) PilosaClient() *pilosa.Client {
	return m.client
}

func NewMain() *Main {
	return &Main{
		PilosaHosts: []string{"localhost:10101"},
		BatchSize:   1, // definitely increase this to achieve any amount of performance
		Index:       "defaultindex",
		Concurrency: 1,
		PackBools:   "bools",
	}
}

func (m *Main) Run() (err error) {
	err = m.setup()
	if err != nil {
		return errors.Wrap(err, "setting up")
	}
	eg := errgroup.Group{}
	for c := 0; c < m.Concurrency; c++ {
		c := c
		eg.Go(func() error {
			return m.runIngester(c)
		})
	}

	return eg.Wait()
}

func (m *Main) setup() (err error) {
	if err := m.validate(); err != nil {
		return errors.Wrap(err, "validating configuration")
	}

	logOut := os.Stdout
	if m.LogPath != "" {
		f, err := os.OpenFile(m.LogPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			return errors.Wrap(err, "opening log file")
		}
		logOut = f
	}

	if m.Verbose {
		m.log = logger.NewVerboseLogger(logOut)
	} else {
		m.log = logger.NewStandardLogger(logOut)
	}

	m.client, err = pilosa.NewClient(m.PilosaHosts)
	if err != nil {
		return errors.Wrap(err, "getting pilosa client")
	}
	m.schema, err = m.client.Schema()
	if err != nil {
		return errors.Wrap(err, "getting schema")
	}
	keyTranslation := len(m.PrimaryKeyFields) > 0
	m.index = m.schema.Index(m.Index, pilosa.OptIndexKeys(keyTranslation))
	if m.PackBools != "" {
		m.index.Field(m.PackBools, pilosa.OptFieldTypeSet(pilosa.CacheTypeRanked, 50000), pilosa.OptFieldKeys(true))
		m.index.Field(m.PackBools+"-exists", pilosa.OptFieldTypeSet(pilosa.CacheTypeRanked, 50000), pilosa.OptFieldKeys(true))
	}
	err = m.client.SyncSchema(m.schema)
	if err != nil {
		return errors.Wrap(err, "syncing schema")
	}

	return nil
}

func (m *Main) runIngester(c int) error {
	source, err := m.NewSource()
	if err != nil {
		return errors.Wrap(err, "getting source")
	}
	var batch gpexp.RecordBatch
	var recordizers []Recordizer
	var prevRec Record
	var row *gpexp.Row
	rec, err := source.Record()
	for ; err == ErrSchemaChange || err == nil; rec, err = source.Record() {
		if err == ErrSchemaChange {
			// finish previous batch if this is not the first
			if batch != nil {
				err = batch.Import()
				if err != nil {
					return errors.Wrap(err, "importing")
				}
				err = prevRec.Commit()
				if err != nil {
					return errors.Wrap(err, "committing")
				}
			}
			schema := source.Schema()
			m.log.Printf("new schema: %+v", schema)
			recordizers, batch, row, err = m.batchFromSchema(schema)
			if err != nil {
				return errors.Wrap(err, "batchFromSchema")
			}
		}
		for i := range row.Values {
			row.Values[i] = nil
		}
		data := rec.Data()
		m.log.Debugf("record: %+v", data)
		for _, rdz := range recordizers {
			err = rdz(data, row)
			if err != nil {
				return errors.Wrap(err, "recordizing")
			}
		}
		err = batch.Add(*row)
		if err == gpexp.ErrBatchNowFull {
			err = batch.Import()
			if err != nil {
				return errors.Wrap(err, "importing batch")
			}
			err = rec.Commit()
			if err != nil {
				return errors.Wrap(err, "commiting record")
			}
		} else if err != nil {
			return errors.Wrap(err, "adding to batch")
		}
		prevRec = rec
	}
	if err == io.EOF {
		err = nil
	}
	return errors.Wrap(err, "getting record")
}

type Recordizer func(rawRec []interface{}, rec *gpexp.Row) error

func (m *Main) batchFromSchema(schema []Field) ([]Recordizer, gpexp.RecordBatch, *gpexp.Row, error) {
	// from the schema, and the configuration stored on Main, we need
	// to create a []pilosa.Field and a []Recordizer processing
	// functions which take a []interface{} which conforms to the
	// schema, and converts it to a record which conforms to the
	// []pilosa.Field.
	//
	// The relevant config options on Main are:
	// 1. PrimaryKeyFields and IDField
	// 2. PackBools
	// 3. BatchSize (gets passed directly to the batch)
	//
	// For PrimaryKeyFields and IDField there is some complexity. There are 3 top level options. 1, the other, or neither (auto-generated IDs).
	//
	// 1. PrimarKeyFields - the main question here is whether in
	// addition to combining these and translating them to column ID,
	// do we index them separately? I think the answer by default
	// should be yes.
	// 2. IDField — this is pretty easy. Use the integer value as the column ID. Do not index it separately by default.
	// 3. Autogenerate IDs. Ideally using a RangeAllocator per concurrent goroutine. OK, let's assume that if we set row.ID to nil, the auto generation can happen inside the Batch.
	recordizers := make([]Recordizer, 0)

	var rz Recordizer
	skips := make(map[int]struct{})
	var err error

	// primary key stuff
	if len(m.PrimaryKeyFields) != 0 {
		rz, skips, err = getPrimaryKeyRecordizer(schema, m.PrimaryKeyFields)
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "getting primary key recordizer")
		}
	} else if m.IDField != "" {
		for fieldIndex, field := range schema {
			if field.Name() == m.IDField {
				if _, ok := field.(IDField); !ok {
					if _, ok := field.(IntField); !ok {
						return nil, nil, nil, errors.Errorf("specified column id field %s is not an IDField or an IntField %T", m.IDField, field)
					}
				}
				fieldIndex := fieldIndex
				rz = func(rawRec []interface{}, rec *gpexp.Row) (err error) {
					id, err := field.PilosafyVal(rawRec[fieldIndex])
					if err != nil {
						return errors.Wrapf(err, "converting %+v to ID", rawRec[fieldIndex])
					}
					if uid, ok := id.(uint64); ok {
						rec.ID = uid
					} else if iid, ok := id.(int64); ok {
						rec.ID = uint64(iid)
					} else {
						return errors.Errorf("can't convert %v of %[1]T to uint64 for use as ID", id)
					}
					return nil
				}
				skips[fieldIndex] = struct{}{}
				break
			}
		}
		if rz == nil {
			return nil, nil, nil, errors.Errorf("ID field %s not found", m.IDField)
		}
	} else {
		return nil, nil, nil, errors.New("autogen IDs is currently unimplemented; specify an IDField or primary key fields")
	}
	recordizers = append(recordizers, rz)

	// set up bool fields
	var boolField, boolFieldExists *pilosa.Field
	if m.PackBools != "" {
		boolField = m.index.Field(m.PackBools, pilosa.OptFieldTypeBool())
		boolFieldExists = m.index.Field(m.PackBools+"-exists", pilosa.OptFieldTypeBool())
	}
	fields := make([]*pilosa.Field, 0, len(schema))
	for i, pdkField := range schema {
		// need to redefine these inside the loop since we're
		// capturing them in closures
		i := i
		pdkField := pdkField
		// see if we previously decided to skip this field of the raw
		// record.
		if _, ok := skips[i]; ok {
			continue
		}

		// handle records where pilosa already has the field
		_, isBool := pdkField.(BoolField)
		if (m.PackBools == "" || !isBool) && m.index.HasField(pdkField.Name()) {
			// TODO validate that Pilosa's existing field matches the
			// type and options of the PDK field.
			fields = append(fields, m.index.Field(pdkField.Name()))
			valIdx := len(fields) - 1
			// TODO may need to have more sophisticated recordizer by type at some point
			recordizers = append(recordizers, func(rawRec []interface{}, rec *gpexp.Row) (err error) {
				rec.Values[valIdx], err = pdkField.PilosafyVal(rawRec[i])
				return errors.Wrapf(err, "pilosafying field %d:%+v, val:%+v", i, pdkField, rawRec[i])
			})
			continue
		}

		// now handle this field if it was not already found in pilosa
		switch fld := pdkField.(type) {
		case StringField, IDField, StringArrayField:
			opts := []pilosa.FieldOption{}
			if hasMutex(fld) {
				opts = append(opts, pilosa.OptFieldTypeMutex(pilosa.CacheTypeRanked, 50000))
			} else {
				opts = append(opts, pilosa.OptFieldTypeSet(pilosa.CacheTypeRanked, 50000))
			}
			_, ok1 := fld.(StringArrayField)
			if _, ok2 := fld.(StringField); ok1 || ok2 {
				opts = append(opts, pilosa.OptFieldKeys(true))
			}
			fields = append(fields, m.index.Field(fld.Name(), opts...))
			valIdx := len(fields) - 1
			recordizers = append(recordizers, func(rawRec []interface{}, rec *gpexp.Row) (err error) {
				rec.Values[valIdx], err = pdkField.PilosafyVal(rawRec[i])
				return errors.Wrapf(err, "pilosafying field %d:%+v, val:%+v", i, pdkField, rawRec[i])
			})
		case BoolField:
			if m.PackBools == "" {
				fields = append(fields, m.index.Field(fld.Name(), pilosa.OptFieldTypeBool()))
				valIdx := len(fields) - 1
				recordizers = append(recordizers, func(rawRec []interface{}, rec *gpexp.Row) (err error) {
					rec.Values[valIdx] = rawRec[i]
					return nil
				})
			} else {
				fields = append(fields, boolField, boolFieldExists)
				fieldIdx := len(fields) - 2
				recordizers = append(recordizers, func(rawRec []interface{}, rec *gpexp.Row) (err error) {
					b, ok := rawRec[i].(bool)
					if b {
						rec.Values[fieldIdx] = pdkField.Name()
					}
					if ok {
						rec.Values[fieldIdx+1] = pdkField.Name()
					}
					return nil
				})
				continue
			}
		case IntField:
			if fld.Min != nil {
				min := *fld.Min
				if fld.Max != nil {
					fields = append(fields, m.index.Field(fld.Name(), pilosa.OptFieldTypeInt(min, *fld.Max)))
				} else {
					fields = append(fields, m.index.Field(fld.Name(), pilosa.OptFieldTypeInt(min)))
				}
			} else {
				fields = append(fields, m.index.Field(fld.Name(), pilosa.OptFieldTypeInt()))
			}
			valIdx := len(fields) - 1
			recordizers = append(recordizers, func(rawRec []interface{}, rec *gpexp.Row) (err error) {
				rec.Values[valIdx], err = pdkField.PilosafyVal(rawRec[i])
				return errors.Wrapf(err, "pilosafying field %d:%+v, val:%+v", i, pdkField, rawRec[i])
			})
		case DecimalField:
			fields = append(fields, m.index.Field(fld.Name(), pilosa.OptFieldTypeInt()))
			valIdx := len(fields) - 1
			recordizers = append(recordizers, func(rawRec []interface{}, rec *gpexp.Row) (err error) {
				rec.Values[valIdx], err = pdkField.PilosafyVal(rawRec[i])
				return errors.Wrapf(err, "pilosafying field %d:%+v, val:%+v", i, pdkField, rawRec[i])
			})
		default:
			return nil, nil, nil, errors.Errorf("unknown schema field type %T %[1]v", pdkField)
		}
	}
	err = m.client.SyncSchema(m.schema)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "syncing schema")
	}
	batch, err := gpexp.NewBatch(m.client, m.BatchSize, m.index, fields)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "creating batch")
	}
	row := &gpexp.Row{
		Values: make([]interface{}, len(fields)),
	}
	return recordizers, batch, row, nil
}

func hasMutex(fld Field) bool {
	if sfld, ok := fld.(StringField); ok {
		return sfld.Mutex
	}
	if sfld, ok := fld.(IDField); ok {
		return sfld.Mutex
	}
	return false
}

// getPrimaryKeyRecordizer returns a Recordizer function which
// extracts the primary key fields from a record, combines them, and
// sets the ID on the record. If pkFields is a single field, and that
// field is of type string, we'll return it in skipFields, because we
// won't want to index it separately.
func getPrimaryKeyRecordizer(schema []Field, pkFields []string) (recordizer Recordizer, skipFields map[int]struct{}, err error) {
	if len(schema) == 0 {
		return nil, nil, errors.New("can't call getPrimaryKeyRecordizer with empty schema")
	}
	if len(pkFields) == 0 {
		return nil, nil, errors.New("can't call getPrimaryKeyRecordizer with empty pkFields")
	}
	fieldIndices := make([]int, 0, len(pkFields))
	for pkIndex, pk := range pkFields {
		for fieldIndex, field := range schema {
			if pk == field.Name() {
				switch field.(type) {
				case StringArrayField:
					return nil, nil, errors.Errorf("field %s cannot be a primary key field because it is a StringArray field.", pk)
				}
				fieldIndices = append(fieldIndices, fieldIndex)
				break
			}
		}
		if len(fieldIndices) != pkIndex+1 {
			return nil, nil, errors.Errorf("no field with primary key field name %s found. fields: %+v", pk, schema)
		}
	}
	if len(pkFields) == 1 {
		if _, ok := schema[fieldIndices[0]].(StringField); ok {
			skipFields = make(map[int]struct{}, 1)
			skipFields[fieldIndices[0]] = struct{}{}
		}
	}
	recordizer = func(rawRec []interface{}, rec *gpexp.Row) (err error) {
		idbytes, ok := rec.ID.([]byte)
		if ok {
			idbytes = idbytes[:0]
		} else {
			idbytes = make([]byte, 0)
		}
		buf := bytes.NewBuffer(idbytes) // TODO does the buffer escape to heap?

		// TODO... will want to change this encoding logic to length-prefix the different fields or something.
		for _, fieldIdx := range fieldIndices {
			val := rawRec[fieldIdx]
			switch vt := val.(type) {
			case string:
				buf.WriteString(vt) // err is always nil
			case []byte:
				buf.Write(vt) // err is always nil
			default:
				err = binary.Write(buf, binary.BigEndian, val)
				if err != nil {
					return errors.Wrapf(err, "writing %+v of type %[1]T", val)
				}
			}
		}
		rec.ID = buf.Bytes()
		return nil
	}
	return recordizer, skipFields, nil
}

func (m *Main) validate() error {
	if len(m.PrimaryKeyFields) != 0 && m.IDField != "" {
		return errors.New("cannot set both primary key fields and id-field")
	}
	if m.NewSource == nil {
		return errors.New("must set a NewSource function on PDK ingester")
	}
	return nil
}
