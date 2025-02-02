// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package codec

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/parser/types"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/pkg/config"
	cerrors "github.com/pingcap/tiflow/pkg/errors"
	canal "github.com/pingcap/tiflow/proto/canal"
	"go.uber.org/zap"
)

const tidbWaterMarkType = "TIDB_WATERMARK"

// CanalFlatEventBatchEncoder encodes Canal flat messages in JSON format
type CanalFlatEventBatchEncoder struct {
	builder    *canalEntryBuilder
	messageBuf []canalFlatMessageInterface
	// When it is true, canal-json would generate TiDB extension information
	// which, at the moment, only includes `tidbWaterMarkType` and `_tidb` fields.
	enableTiDBExtension bool
}

// NewCanalFlatEventBatchEncoder creates a new CanalFlatEventBatchEncoder
func NewCanalFlatEventBatchEncoder() EventBatchEncoder {
	return &CanalFlatEventBatchEncoder{
		builder:             NewCanalEntryBuilder(),
		messageBuf:          make([]canalFlatMessageInterface, 0),
		enableTiDBExtension: false,
	}
}

type canalFlatEventBatchEncoderBuilder struct {
	opts map[string]string
}

// Build a `CanalFlatEventBatchEncoder`
func (b *canalFlatEventBatchEncoderBuilder) Build(_ context.Context) (EventBatchEncoder, error) {
	encoder := NewCanalFlatEventBatchEncoder()
	if err := encoder.SetParams(b.opts); err != nil {
		return nil, cerrors.WrapError(cerrors.ErrKafkaInvalidConfig, err)
	}

	return encoder, nil
}

func newCanalFlatEventBatchEncoderBuilder(opts map[string]string) EncoderBuilder {
	return &canalFlatEventBatchEncoderBuilder{opts: opts}
}

// The TiCDC Canal-JSON implementation extend the official format with a TiDB extension field.
// canalFlatMessageInterface is used to support this without affect the original format.
type canalFlatMessageInterface interface {
	getTikvTs() uint64
	getSchema() *string
	getTable() *string
	getCommitTs() uint64
	getQuery() string
	getOld() map[string]interface{}
	getData() map[string]interface{}
	getMySQLType() map[string]string
	getJavaSQLType() map[string]int32
}

// adapted from https://github.com/alibaba/canal/blob/b54bea5e3337c9597c427a53071d214ff04628d1/protocol/src/main/java/com/alibaba/otter/canal/protocol/FlatMessage.java#L1
type canalFlatMessage struct {
	// ignored by consumers
	ID        int64    `json:"id"`
	Schema    string   `json:"database"`
	Table     string   `json:"table"`
	PKNames   []string `json:"pkNames"`
	IsDDL     bool     `json:"isDdl"`
	EventType string   `json:"type"`
	// officially the timestamp of the event-time of the message, in milliseconds since Epoch.
	ExecutionTime int64 `json:"es"`
	// officially the timestamp of building the MQ message, in milliseconds since Epoch.
	BuildTime int64 `json:"ts"`
	// SQL that generated the change event, DDL or Query
	Query string `json:"sql"`
	// only works for INSERT / UPDATE / DELETE events, records each column's java representation type.
	SQLType map[string]int32 `json:"sqlType"`
	// only works for INSERT / UPDATE / DELETE events, records each column's mysql representation type.
	MySQLType map[string]string `json:"mysqlType"`
	// A Datum should be a string or nil
	Data []map[string]interface{} `json:"data"`
	Old  []map[string]interface{} `json:"old"`
	// Used internally by CanalFlatEventBatchEncoder
	tikvTs uint64
}

func (c *canalFlatMessage) getTikvTs() uint64 {
	return c.tikvTs
}

func (c *canalFlatMessage) getSchema() *string {
	return &c.Schema
}

func (c *canalFlatMessage) getTable() *string {
	return &c.Table
}

// for canalFlatMessage, we lost the commitTs.
func (c *canalFlatMessage) getCommitTs() uint64 {
	return 0
}

func (c *canalFlatMessage) getQuery() string {
	return c.Query
}

func (c *canalFlatMessage) getOld() map[string]interface{} {
	if c.Old == nil {
		return nil
	}
	return c.Old[0]
}

func (c *canalFlatMessage) getData() map[string]interface{} {
	if c.Data == nil {
		return nil
	}
	return c.Data[0]
}

func (c *canalFlatMessage) getMySQLType() map[string]string {
	return c.MySQLType
}

func (c *canalFlatMessage) getJavaSQLType() map[string]int32 {
	return c.SQLType
}

type tidbExtension struct {
	CommitTs    uint64 `json:"commitTs,omitempty"`
	WatermarkTs uint64 `json:"watermarkTs,omitempty"`
}

type canalFlatMessageWithTiDBExtension struct {
	*canalFlatMessage
	// Extensions is a TiCDC custom field that different from official Canal-JSON format.
	// It would be useful to store something for special usage.
	// At the moment, only store the `tso` of each event,
	// which is useful if the message consumer needs to restore the original transactions.
	Extensions *tidbExtension `json:"_tidb"`
}

func (c *canalFlatMessageWithTiDBExtension) getCommitTs() uint64 {
	return c.Extensions.CommitTs
}

func (c *CanalFlatEventBatchEncoder) newFlatMessageForDML(e *model.RowChangedEvent) (canalFlatMessageInterface, error) {
	eventType := convertRowEventType(e)
	header := c.builder.buildHeader(e.CommitTs, e.Table.Schema, e.Table.Table, eventType, 1)
	rowData, err := c.builder.buildRowData(e)
	if err != nil {
		return nil, cerrors.WrapError(cerrors.ErrCanalEncodeFailed, err)
	}

	pkCols := e.PrimaryKeyColumns()
	pkNames := make([]string, len(pkCols))
	for i := range pkNames {
		pkNames[i] = pkCols[i].Name
	}

	var nonTrivialRow []*canal.Column
	if e.IsDelete() {
		nonTrivialRow = rowData.BeforeColumns
	} else {
		nonTrivialRow = rowData.AfterColumns
	}

	sqlType := make(map[string]int32, len(nonTrivialRow))
	mysqlType := make(map[string]string, len(nonTrivialRow))
	for i := range nonTrivialRow {
		sqlType[nonTrivialRow[i].Name] = nonTrivialRow[i].SqlType
		mysqlType[nonTrivialRow[i].Name] = nonTrivialRow[i].MysqlType
	}

	var (
		data    map[string]interface{}
		oldData map[string]interface{}
	)

	if len(rowData.BeforeColumns) > 0 {
		oldData = make(map[string]interface{}, len(rowData.BeforeColumns))
		for i := range rowData.BeforeColumns {
			if !rowData.BeforeColumns[i].GetIsNull() {
				oldData[rowData.BeforeColumns[i].Name] = rowData.BeforeColumns[i].Value
			} else {
				oldData[rowData.BeforeColumns[i].Name] = nil
			}
		}
	}

	if len(rowData.AfterColumns) > 0 {
		data = make(map[string]interface{}, len(rowData.AfterColumns))
		for i := range rowData.AfterColumns {
			if !rowData.AfterColumns[i].GetIsNull() {
				data[rowData.AfterColumns[i].Name] = rowData.AfterColumns[i].Value
			} else {
				data[rowData.AfterColumns[i].Name] = nil
			}
		}
	}

	flatMessage := &canalFlatMessage{
		ID:            0, // ignored by both Canal Adapter and Flink
		Schema:        header.SchemaName,
		Table:         header.TableName,
		PKNames:       pkNames,
		IsDDL:         false,
		EventType:     header.GetEventType().String(),
		ExecutionTime: header.ExecuteTime,
		BuildTime:     time.Now().UnixNano() / 1e6, // ignored by both Canal Adapter and Flink
		Query:         "",
		SQLType:       sqlType,
		MySQLType:     mysqlType,
		Data:          make([]map[string]interface{}, 0),
		Old:           nil,
		tikvTs:        e.CommitTs,
	}

	if e.IsDelete() {
		flatMessage.Data = append(flatMessage.Data, oldData)
	} else if e.IsInsert() {
		flatMessage.Data = append(flatMessage.Data, data)
	} else if e.IsUpdate() {
		flatMessage.Old = []map[string]interface{}{oldData}
		flatMessage.Data = append(flatMessage.Data, data)
	} else {
		log.Panic("unreachable event type", zap.Any("event", e))
	}

	if !c.enableTiDBExtension {
		return flatMessage, nil
	}

	return &canalFlatMessageWithTiDBExtension{
		canalFlatMessage: flatMessage,
		Extensions:       &tidbExtension{CommitTs: e.CommitTs},
	}, nil
}

func (c *CanalFlatEventBatchEncoder) newFlatMessageForDDL(e *model.DDLEvent) canalFlatMessageInterface {
	header := c.builder.buildHeader(e.CommitTs, e.TableInfo.Schema, e.TableInfo.Table, convertDdlEventType(e), 1)
	flatMessage := &canalFlatMessage{
		ID:            0, // ignored by both Canal Adapter and Flink
		Schema:        header.SchemaName,
		Table:         header.TableName,
		IsDDL:         true,
		EventType:     header.GetEventType().String(),
		ExecutionTime: header.ExecuteTime,
		BuildTime:     time.Now().UnixNano() / 1e6, // timestamp
		Query:         e.Query,
		tikvTs:        e.CommitTs,
	}

	if !c.enableTiDBExtension {
		return flatMessage
	}

	return &canalFlatMessageWithTiDBExtension{
		canalFlatMessage: flatMessage,
		Extensions:       &tidbExtension{CommitTs: e.CommitTs},
	}
}

func (c *CanalFlatEventBatchEncoder) newFlatMessage4CheckpointEvent(ts uint64) *canalFlatMessageWithTiDBExtension {
	return &canalFlatMessageWithTiDBExtension{
		canalFlatMessage: &canalFlatMessage{
			ID:            0,
			IsDDL:         false,
			EventType:     tidbWaterMarkType,
			ExecutionTime: convertToCanalTs(ts),
			BuildTime:     time.Now().UnixNano() / int64(time.Millisecond), // converts to milliseconds
		},
		Extensions: &tidbExtension{WatermarkTs: ts},
	}
}

// EncodeCheckpointEvent implements the EventBatchEncoder interface
func (c *CanalFlatEventBatchEncoder) EncodeCheckpointEvent(ts uint64) (*MQMessage, error) {
	if !c.enableTiDBExtension {
		return nil, nil
	}

	msg := c.newFlatMessage4CheckpointEvent(ts)
	value, err := json.Marshal(msg)
	if err != nil {
		return nil, cerrors.WrapError(cerrors.ErrCanalEncodeFailed, err)
	}
	return newResolvedMQMessage(config.ProtocolCanalJSON, nil, value, ts), nil
}

// AppendRowChangedEvent implements the interface EventBatchEncoder
func (c *CanalFlatEventBatchEncoder) AppendRowChangedEvent(e *model.RowChangedEvent) error {
	message, err := c.newFlatMessageForDML(e)
	if err != nil {
		return errors.Trace(err)
	}
	c.messageBuf = append(c.messageBuf, message)
	return nil
}

// EncodeDDLEvent encodes DDL events
func (c *CanalFlatEventBatchEncoder) EncodeDDLEvent(e *model.DDLEvent) (*MQMessage, error) {
	message := c.newFlatMessageForDDL(e)
	value, err := json.Marshal(message)
	if err != nil {
		return nil, cerrors.WrapError(cerrors.ErrCanalEncodeFailed, err)
	}
	return newDDLMQMessage(config.ProtocolCanalJSON, nil, value, e), nil
}

// Build implements the EventBatchEncoder interface
func (c *CanalFlatEventBatchEncoder) Build() []*MQMessage {
	if len(c.messageBuf) == 0 {
		return nil
	}
	ret := make([]*MQMessage, len(c.messageBuf))
	for i, msg := range c.messageBuf {
		value, err := json.Marshal(msg)
		if err != nil {
			log.Panic("CanalFlatEventBatchEncoder", zap.Error(err))
			return nil
		}
		m := NewMQMessage(config.ProtocolCanalJSON, nil, value, msg.getTikvTs(), model.MqMessageTypeRow, msg.getSchema(), msg.getTable())
		m.IncRowsCount()
		ret[i] = m
	}
	c.messageBuf = make([]canalFlatMessageInterface, 0)
	return ret
}

// Size implements the EventBatchEncoder interface
func (c *CanalFlatEventBatchEncoder) Size() int {
	return -1
}

// SetParams sets the encoding parameters for the canal flat protocol.
func (c *CanalFlatEventBatchEncoder) SetParams(params map[string]string) error {
	if s, ok := params["enable-tidb-extension"]; ok {
		a, err := strconv.ParseBool(s)
		if err != nil {
			return cerrors.WrapError(cerrors.ErrSinkInvalidConfig, err)
		}
		c.enableTiDBExtension = a
	}
	return nil
}

// CanalFlatEventBatchDecoder decodes the byte into the original message.
type CanalFlatEventBatchDecoder struct {
	data                []byte
	msg                 *MQMessage
	enableTiDBExtension bool
}

func newCanalFlatEventBatchDecoder(data []byte, enableTiDBExtension bool) EventBatchDecoder {
	return &CanalFlatEventBatchDecoder{
		data:                data,
		msg:                 nil,
		enableTiDBExtension: enableTiDBExtension,
	}
}

// HasNext implements the EventBatchDecoder interface
func (b *CanalFlatEventBatchDecoder) HasNext() (model.MqMessageType, bool, error) {
	if len(b.data) == 0 {
		return model.MqMessageTypeUnknown, false, nil
	}
	msg := &MQMessage{}
	if err := json.Unmarshal(b.data, msg); err != nil {
		return model.MqMessageTypeUnknown, false, err
	}
	b.msg = msg
	b.data = nil
	if b.msg.Type == model.MqMessageTypeUnknown {
		return model.MqMessageTypeUnknown, false, nil
	}
	return b.msg.Type, true, nil
}

// NextRowChangedEvent implements the EventBatchDecoder interface
// `HasNext` should be called before this.
func (b *CanalFlatEventBatchDecoder) NextRowChangedEvent() (*model.RowChangedEvent, error) {
	if b.msg == nil || b.msg.Type != model.MqMessageTypeRow {
		return nil, cerrors.ErrCanalDecodeFailed.GenWithStack("not found row changed event message")
	}

	var data canalFlatMessageInterface = &canalFlatMessage{}
	if b.enableTiDBExtension {
		data = &canalFlatMessageWithTiDBExtension{canalFlatMessage: &canalFlatMessage{}, Extensions: &tidbExtension{}}
	}

	if err := json.Unmarshal(b.msg.Value, data); err != nil {
		return nil, errors.Trace(err)
	}
	b.msg = nil
	return canalFlatMessage2RowChangedEvent(data)
}

// NextDDLEvent implements the EventBatchDecoder interface
// `HasNext` should be called before this.
func (b *CanalFlatEventBatchDecoder) NextDDLEvent() (*model.DDLEvent, error) {
	if b.msg == nil || b.msg.Type != model.MqMessageTypeDDL {
		return nil, cerrors.ErrCanalDecodeFailed.GenWithStack("not found ddl event message")
	}

	var data canalFlatMessageInterface = &canalFlatMessage{}
	if b.enableTiDBExtension {
		data = &canalFlatMessageWithTiDBExtension{canalFlatMessage: &canalFlatMessage{}, Extensions: &tidbExtension{}}
	}

	if err := json.Unmarshal(b.msg.Value, data); err != nil {
		return nil, errors.Trace(err)
	}
	b.msg = nil
	return canalFlatMessage2DDLEvent(data), nil
}

// NextResolvedEvent implements the EventBatchDecoder interface
// `HasNext` should be called before this.
func (b *CanalFlatEventBatchDecoder) NextResolvedEvent() (uint64, error) {
	if b.msg == nil || b.msg.Type != model.MqMessageTypeResolved {
		return 0, cerrors.ErrCanalDecodeFailed.GenWithStack("not found resolved event message")
	}

	message := &canalFlatMessageWithTiDBExtension{
		canalFlatMessage: &canalFlatMessage{},
	}
	if err := json.Unmarshal(b.msg.Value, message); err != nil {
		return 0, errors.Trace(err)
	}
	b.msg = nil
	return message.Extensions.WatermarkTs, nil
}

func canalFlatMessage2RowChangedEvent(flatMessage canalFlatMessageInterface) (*model.RowChangedEvent, error) {
	result := new(model.RowChangedEvent)
	result.CommitTs = flatMessage.getCommitTs()
	result.Table = &model.TableName{
		Schema: *flatMessage.getSchema(),
		Table:  *flatMessage.getTable(),
	}

	var err error
	result.Columns, err = canalFlatJSONColumnMap2SinkColumns(flatMessage.getData(), flatMessage.getMySQLType(), flatMessage.getJavaSQLType())
	if err != nil {
		return nil, err
	}
	result.PreColumns, err = canalFlatJSONColumnMap2SinkColumns(flatMessage.getOld(), flatMessage.getMySQLType(), flatMessage.getJavaSQLType())
	if err != nil {
		return nil, err
	}

	return result, nil
}

func canalFlatJSONColumnMap2SinkColumns(cols map[string]interface{}, mysqlType map[string]string, javaSQLType map[string]int32) ([]*model.Column, error) {
	result := make([]*model.Column, 0, len(cols))
	for name, value := range cols {
		javaType, ok := javaSQLType[name]
		if !ok {
			// this should not happen, else we have to check encoding for javaSQLType.
			return nil, cerrors.ErrCanalDecodeFailed.GenWithStack(
				"java sql type does not found, column: %+v, mysqlType: %+v", name, javaSQLType)
		}
		mysqlTypeStr, ok := mysqlType[name]
		if !ok {
			// this should not happen, else we have to check encoding for mysqlType.
			return nil, cerrors.ErrCanalDecodeFailed.GenWithStack(
				"mysql type does not found, column: %+v, mysqlType: %+v", name, mysqlType)
		}
		mysqlTypeStr = trimUnsignedFromMySQLType(mysqlTypeStr)
		mysqlType := types.StrToType(mysqlTypeStr)
		col := newColumn(value, mysqlType).decodeCanalJSONColumn(name, JavaSQLType(javaType))
		result = append(result, col)
	}
	if len(result) == 0 {
		return nil, nil
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.Compare(result[i].Name, result[j].Name) > 0
	})
	return result, nil
}

func canalFlatMessage2DDLEvent(flatDDL canalFlatMessageInterface) *model.DDLEvent {
	result := new(model.DDLEvent)
	// we lost the startTs from kafka message
	result.CommitTs = flatDDL.getCommitTs()

	result.TableInfo = new(model.SimpleTableInfo)
	result.TableInfo.Schema = *flatDDL.getSchema()
	result.TableInfo.Table = *flatDDL.getTable()

	// we lost DDL type from canal flat json format, only got the DDL SQL.
	result.Query = flatDDL.getQuery()

	return result
}
