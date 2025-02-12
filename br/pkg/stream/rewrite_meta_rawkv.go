// Copyright 2022-present PingCAP, Inc.
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

package stream

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/restore/ingestrec"
	"github.com/pingcap/tidb/br/pkg/restore/tiflashrec"
	"github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/meta/model"
	filter "github.com/pingcap/tidb/pkg/util/table-filter"
	"go.uber.org/zap"
)

// Default columnFamily and write columnFamily
const (
	DefaultCF = "default"
	WriteCF   = "write"
)

type UpstreamID = int64
type DownstreamID = int64

// TableReplace specifies table information mapping from up-stream cluster to up-stream cluster.
type TableReplace struct {
	Name         string
	TableID      DownstreamID
	PartitionMap map[UpstreamID]DownstreamID
	IndexMap     map[UpstreamID]DownstreamID
}

// DBReplace specifies database information mapping from up-stream cluster to up-stream cluster.
type DBReplace struct {
	Name     string
	DbID     DownstreamID
	TableMap map[UpstreamID]*TableReplace
}

// SchemasReplace specifies schemas information mapping from up-stream cluster to up-stream cluster.
type SchemasReplace struct {
	DbMap map[UpstreamID]*DBReplace

	delRangeRecorder *brDelRangeExecWrapper
	ingestRecorder   *ingestrec.IngestRecorder
	TiflashRecorder  *tiflashrec.TiFlashRecorder
	RewriteTS        uint64        // used to rewrite commit ts in meta kv.
	TableFilter      filter.Filter // used to filter schema/table

	AfterTableRewritten func(deleted bool, tableInfo *model.TableInfo)
}

// NewTableReplace creates a TableReplace struct.
func NewTableReplace(name string, newID DownstreamID) *TableReplace {
	return &TableReplace{
		Name:         name,
		TableID:      newID,
		PartitionMap: make(map[UpstreamID]DownstreamID),
		IndexMap:     make(map[UpstreamID]DownstreamID),
	}
}

// NewDBReplace creates a DBReplace struct.
func NewDBReplace(name string, newID DownstreamID) *DBReplace {
	return &DBReplace{
		Name:     name,
		DbID:     newID,
		TableMap: make(map[UpstreamID]*TableReplace),
	}
}

// NewSchemasReplace creates a SchemasReplace struct.
func NewSchemasReplace(
	dbMap map[UpstreamID]*DBReplace,
	tiflashRecorder *tiflashrec.TiFlashRecorder,
	restoreTS uint64,
	tableFilter filter.Filter,
	recordDeleteRange func(*PreDelRangeQuery),
) *SchemasReplace {
	globalTableIdMap := make(map[UpstreamID]DownstreamID)
	for _, dr := range dbMap {
		for tblID, tr := range dr.TableMap {
			globalTableIdMap[tblID] = tr.TableID
			for oldpID, newpID := range tr.PartitionMap {
				globalTableIdMap[oldpID] = newpID
			}
		}
	}

	return &SchemasReplace{
		DbMap:            dbMap,
		delRangeRecorder: newDelRangeExecWrapper(globalTableIdMap, recordDeleteRange),
		ingestRecorder:   ingestrec.New(),
		TiflashRecorder:  tiflashRecorder,
		RewriteTS:        restoreTS,
		TableFilter:      tableFilter,
	}
}

func (sr *SchemasReplace) rewriteKeyForDB(key []byte, cf string) ([]byte, error) {
	rawMetaKey, err := ParseTxnMetaKeyFrom(key)
	if err != nil {
		return nil, errors.Trace(err)
	}

	dbID, err := meta.ParseDBKey(rawMetaKey.Field)
	if err != nil {
		return nil, errors.Trace(err)
	}

	dbMap, exist := sr.DbMap[dbID]
	if !exist {
		// db filtered out
		return nil, nil
	}

	rawMetaKey.UpdateField(meta.DBkey(dbMap.DbID))
	if cf == WriteCF {
		rawMetaKey.UpdateTS(sr.RewriteTS)
	}
	return rawMetaKey.EncodeMetaKey(), nil
}

func (sr *SchemasReplace) rewriteDBInfo(value []byte) ([]byte, error) {
	dbInfo := new(model.DBInfo)
	if err := json.Unmarshal(value, dbInfo); err != nil {
		return nil, errors.Trace(err)
	}

	dbMap, exist := sr.DbMap[dbInfo.ID]
	if !exist {
		// db filtered out
		return nil, nil
	}

	dbInfo.ID = dbMap.DbID
	newValue, err := json.Marshal(dbInfo)
	if err != nil {
		return nil, err
	}
	return newValue, nil
}

func (sr *SchemasReplace) rewriteEntryForDB(e *kv.Entry, cf string) (*kv.Entry, error) {
	r, err := sr.rewriteValue(
		e.Value,
		cf,
		func(value []byte) ([]byte, error) {
			return sr.rewriteDBInfo(value)
		},
	)
	if err != nil {
		return nil, errors.Trace(err)
	}

	newValue := r.NewValue
	newKey, err := sr.rewriteKeyForDB(e.Key, cf)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &kv.Entry{Key: newKey, Value: newValue}, nil
}

func (sr *SchemasReplace) rewriteKeyForTable(
	key []byte,
	cf string,
	parseField func([]byte) (tableID int64, err error),
	encodeField func(tableID int64) []byte,
) ([]byte, error) {
	var (
		err   error
		exist bool
	)
	rawMetaKey, err := ParseTxnMetaKeyFrom(key)
	if err != nil {
		return nil, errors.Trace(err)
	}

	dbID, err := meta.ParseDBKey(rawMetaKey.Key)
	if err != nil {
		return nil, errors.Trace(err)
	}
	tableID, err := parseField(rawMetaKey.Field)
	if err != nil {
		log.Warn("parse table key failed", zap.ByteString("field", rawMetaKey.Field))
		return nil, errors.Trace(err)
	}

	dbReplace, exist := sr.DbMap[dbID]
	if !exist {
		// db filtered out
		return nil, nil
	}

	tableReplace, exist := dbReplace.TableMap[tableID]
	if !exist {
		// table filtered out
		return nil, nil
	}

	rawMetaKey.UpdateKey(meta.DBkey(dbReplace.DbID))
	rawMetaKey.UpdateField(encodeField(tableReplace.TableID))
	if cf == WriteCF {
		rawMetaKey.UpdateTS(sr.RewriteTS)
	}
	return rawMetaKey.EncodeMetaKey(), nil
}

func (sr *SchemasReplace) rewriteTableInfo(value []byte, dbID int64) ([]byte, error) {
	var (
		tableInfo    model.TableInfo
		err          error
		exist        bool
		dbReplace    *DBReplace
		tableReplace *TableReplace
	)
	if err := json.Unmarshal(value, &tableInfo); err != nil {
		return nil, errors.Trace(err)
	}

	// construct or find the id map.
	dbReplace, exist = sr.DbMap[dbID]
	if !exist {
		// db filtered out
		return nil, nil
	}

	tableReplace, exist = dbReplace.TableMap[tableInfo.ID]
	if !exist {
		// table filtered out
		return nil, nil
	}

	// update table ID and partition ID.
	tableInfo.ID = tableReplace.TableID
	partitions := tableInfo.GetPartitionInfo()
	if partitions != nil {
		for i, tbl := range partitions.Definitions {
			newID, exist := tableReplace.PartitionMap[tbl.ID]
			if !exist {
				log.Error("expect partition info in table replace but got none", zap.Int64("partitionID", tbl.ID))
				return nil, errors.Annotatef(berrors.ErrInvalidArgument, "failed to find partition id:%v in replace maps", tbl.ID)
			}
			partitions.Definitions[i].ID = newID
		}
	}

	// Force to disable TTL_ENABLE when restore
	if tableInfo.TTLInfo != nil {
		tableInfo.TTLInfo.Enable = false
	}
	if sr.AfterTableRewritten != nil {
		sr.AfterTableRewritten(false, &tableInfo)
	}

	// marshal to json
	newValue, err := json.Marshal(&tableInfo)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return newValue, nil
}

func (sr *SchemasReplace) rewriteEntryForTable(e *kv.Entry, cf string) (*kv.Entry, error) {
	dbID, err := ParseDBIDFromTableKey(e.Key)
	if err != nil {
		return nil, errors.Trace(err)
	}

	result, err := sr.rewriteValue(
		e.Value,
		cf,
		func(value []byte) ([]byte, error) {
			return sr.rewriteTableInfo(value, dbID)
		},
	)
	if err != nil {
		return nil, errors.Trace(err)
	}

	var newTableID int64 = 0
	newKey, err := sr.rewriteKeyForTable(e.Key, cf, meta.ParseTableKey, func(tableID int64) []byte {
		newTableID = tableID
		return meta.TableKey(tableID)
	})
	if err != nil {
		return nil, errors.Trace(err)
	}

	// NOTE: the normal path is in the `SchemaReplace.rewriteTableInfo`
	//       for now, we rewrite key and value separately hence we cannot
	//       get a view of (is_delete, table_id, table_info) at the same time :(.
	//       Maybe we can extract the rewrite part from rewriteTableInfo.
	if result.Deleted && sr.AfterTableRewritten != nil {
		sr.AfterTableRewritten(true, &model.TableInfo{ID: newTableID})
	}

	return &kv.Entry{Key: newKey, Value: result.NewValue}, nil
}

func (sr *SchemasReplace) rewriteEntryForAutoIncrementIDKey(e *kv.Entry, cf string) (*kv.Entry, error) {
	newKey, err := sr.rewriteKeyForTable(
		e.Key,
		cf,
		meta.ParseAutoIncrementIDKey,
		meta.AutoIncrementIDKey,
	)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &kv.Entry{Key: newKey, Value: e.Value}, nil
}

func (sr *SchemasReplace) rewriteEntryForAutoTableIDKey(e *kv.Entry, cf string) (*kv.Entry, error) {
	newKey, err := sr.rewriteKeyForTable(
		e.Key,
		cf,
		meta.ParseAutoTableIDKey,
		meta.AutoTableIDKey,
	)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &kv.Entry{Key: newKey, Value: e.Value}, nil
}

func (sr *SchemasReplace) rewriteEntryForSequenceKey(e *kv.Entry, cf string) (*kv.Entry, error) {
	newKey, err := sr.rewriteKeyForTable(
		e.Key,
		cf,
		meta.ParseSequenceKey,
		meta.SequenceKey,
	)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &kv.Entry{Key: newKey, Value: e.Value}, nil
}

func (sr *SchemasReplace) rewriteEntryForAutoRandomTableIDKey(e *kv.Entry, cf string) (*kv.Entry, error) {
	newKey, err := sr.rewriteKeyForTable(
		e.Key,
		cf,
		meta.ParseAutoRandomTableIDKey,
		meta.AutoRandomTableIDKey,
	)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &kv.Entry{Key: newKey, Value: e.Value}, nil
}

type rewriteResult struct {
	Deleted  bool
	NewValue []byte
}

// rewriteValue rewrite the value if cf is "default", or rewrite the shortValue if cf is "write".
func (sr *SchemasReplace) rewriteValue(value []byte, cf string, rewrite func([]byte) ([]byte, error)) (rewriteResult, error) {
	switch cf {
	case DefaultCF:
		newValue, err := rewrite(value)
		if err != nil {
			return rewriteResult{}, errors.Trace(err)
		}
		return rewriteResult{
			NewValue: newValue,
			Deleted:  false,
		}, nil
	case WriteCF:
		rawWriteCFValue := new(RawWriteCFValue)
		if err := rawWriteCFValue.ParseFrom(value); err != nil {
			return rewriteResult{}, errors.Trace(err)
		}

		if rawWriteCFValue.IsDelete() {
			return rewriteResult{
				NewValue: value,
				Deleted:  true,
			}, nil
		}
		if rawWriteCFValue.IsRollback() {
			return rewriteResult{
				NewValue: value,
				Deleted:  false,
			}, nil
		}
		if !rawWriteCFValue.HasShortValue() {
			return rewriteResult{
				NewValue: value,
			}, nil
		}

		shortValue, err := rewrite(rawWriteCFValue.GetShortValue())
		if err != nil {
			log.Info("failed to rewrite short value",
				zap.ByteString("write-type", []byte{rawWriteCFValue.GetWriteType()}),
				zap.Int("short-value-len", len(rawWriteCFValue.GetShortValue())))
			return rewriteResult{}, errors.Trace(err)
		}

		rawWriteCFValue.UpdateShortValue(shortValue)
		return rewriteResult{NewValue: rawWriteCFValue.EncodeTo()}, nil
	default:
		panic(fmt.Sprintf("not support cf:%s", cf))
	}
}

func (sr *SchemasReplace) GetIngestRecorder() *ingestrec.IngestRecorder {
	return sr.ingestRecorder
}

// RewriteKvEntry uses to rewrite tableID/dbID in entry.key and entry.value
func (sr *SchemasReplace) RewriteKvEntry(e *kv.Entry, cf string) (*kv.Entry, error) {
	// skip mDDLJob
	if !IsMetaDBKey(e.Key) {
		if cf == DefaultCF && IsMetaDDLJobHistoryKey(e.Key) { // mDDLJobHistory
			job := &model.Job{}
			if err := job.Decode(e.Value); err != nil {
				log.Debug("failed to decode the job",
					zap.String("error", err.Error()), zap.String("job", string(e.Value)))
				// The value in write-cf is like "p\XXXX\XXX" need not restore. skip it
				// The value in default-cf that can Decode() need restore.
				return nil, nil
			}

			return nil, sr.restoreFromHistory(job)
		}
		return nil, nil
	}

	rawKey, err := ParseTxnMetaKeyFrom(e.Key)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if meta.IsDBkey(rawKey.Field) {
		return sr.rewriteEntryForDB(e, cf)
	} else if !meta.IsDBkey(rawKey.Key) {
		return nil, nil
	}
	if meta.IsTableKey(rawKey.Field) {
		return sr.rewriteEntryForTable(e, cf)
	} else if meta.IsAutoIncrementIDKey(rawKey.Field) {
		return sr.rewriteEntryForAutoIncrementIDKey(e, cf)
	} else if meta.IsAutoTableIDKey(rawKey.Field) {
		return sr.rewriteEntryForAutoTableIDKey(e, cf)
	} else if meta.IsSequenceKey(rawKey.Field) {
		return sr.rewriteEntryForSequenceKey(e, cf)
	} else if meta.IsAutoRandomTableIDKey(rawKey.Field) {
		return sr.rewriteEntryForAutoRandomTableIDKey(e, cf)
	}
	return nil, nil
}

func (sr *SchemasReplace) tryRecordIngestIndex(job *model.Job) error {
	if job.Type != model.ActionMultiSchemaChange {
		return sr.ingestRecorder.TryAddJob(job, false)
	}

	for i, sub := range job.MultiSchemaInfo.SubJobs {
		proxyJob := sub.ToProxyJob(job, i)
		// ASSERT: the proxyJob can not be MultiSchemaInfo anymore
		if err := sr.ingestRecorder.TryAddJob(&proxyJob, true); err != nil {
			return err
		}
	}
	return nil
}

func (sr *SchemasReplace) restoreFromHistory(job *model.Job) error {
	if ddl.JobNeedGC(job) {
		if err := ddl.AddDelRangeJobInternal(context.TODO(), sr.delRangeRecorder, job); err != nil {
			return err
		}
	}

	return sr.tryRecordIngestIndex(job)
}

type DelRangeParams struct {
	JobID    int64
	ElemID   int64
	StartKey string
	EndKey   string
}

type PreDelRangeQuery struct {
	Sql        string
	ParamsList []DelRangeParams
}

type brDelRangeExecWrapper struct {
	globalTableIdMap map[UpstreamID]DownstreamID

	recordDeleteRange func(*PreDelRangeQuery)

	// temporary values
	query *PreDelRangeQuery
}

func newDelRangeExecWrapper(
	globalTableIdMap map[UpstreamID]DownstreamID,
	recordDeleteRange func(*PreDelRangeQuery),
) *brDelRangeExecWrapper {
	return &brDelRangeExecWrapper{
		globalTableIdMap:  globalTableIdMap,
		recordDeleteRange: recordDeleteRange,

		query: nil,
	}
}

// UpdateTSOForJob just does nothing. BR would generate ts after log restore done.
func (bdr *brDelRangeExecWrapper) UpdateTSOForJob() error {
	return nil
}

func (bdr *brDelRangeExecWrapper) PrepareParamsList(sz int) {
	bdr.query = &PreDelRangeQuery{
		ParamsList: make([]DelRangeParams, 0, sz),
	}
}

func (bdr *brDelRangeExecWrapper) RewriteTableID(tableID int64) (int64, bool) {
	newTableID, exists := bdr.globalTableIdMap[tableID]
	if !exists {
		log.Warn("failed to find the downstream id when rewrite delete range", zap.Int64("old tableID", tableID))
	}
	return newTableID, exists
}

func (bdr *brDelRangeExecWrapper) AppendParamsList(jobID, elemID int64, startKey, endKey string) {
	bdr.query.ParamsList = append(bdr.query.ParamsList, DelRangeParams{jobID, elemID, startKey, endKey})
}

func (bdr *brDelRangeExecWrapper) ConsumeDeleteRange(ctx context.Context, sql string) error {
	bdr.query.Sql = sql
	bdr.recordDeleteRange(bdr.query)
	bdr.query = nil
	return nil
}
