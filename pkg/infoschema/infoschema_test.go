// Copyright 2015 PingCAP, Inc.
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

package infoschema_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/ddl/placement"
	"github.com/pingcap/tidb/pkg/domain"
	"github.com/pingcap/tidb/pkg/infoschema"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/meta/autoid"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/store/mockstore"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/testkit"
	"github.com/pingcap/tidb/pkg/testkit/testutil"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/stretchr/testify/require"
)

type mockAutoIDRequirement struct {
	store  kv.Storage
	client *autoid.ClientDiscover
}

func (mr *mockAutoIDRequirement) Store() kv.Storage {
	return mr.store
}

func (mr *mockAutoIDRequirement) AutoIDClient() *autoid.ClientDiscover {
	return mr.client
}

func createAutoIDRequirement(t testing.TB, opts ...mockstore.MockTiKVStoreOption) autoid.Requirement {
	store, err := mockstore.NewMockStore(opts...)
	require.NoError(t, err)
	return &mockAutoIDRequirement{
		store:  store,
		client: nil,
	}
}

func TestBasic(t *testing.T) {
	re := createAutoIDRequirement(t)
	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	dbName := model.NewCIStr("Test")
	tbName := model.NewCIStr("T")
	colName := model.NewCIStr("A")
	idxName := model.NewCIStr("idx")
	noexist := model.NewCIStr("noexist")

	colID, err := genGlobalID(re.Store())
	require.NoError(t, err)
	colInfo := &model.ColumnInfo{
		ID:        colID,
		Name:      colName,
		Offset:    0,
		FieldType: *types.NewFieldType(mysql.TypeLonglong),
		State:     model.StatePublic,
	}

	idxInfo := &model.IndexInfo{
		Name:  idxName,
		Table: tbName,
		Columns: []*model.IndexColumn{
			{
				Name:   colName,
				Offset: 0,
				Length: 10,
			},
		},
		Unique:  true,
		Primary: true,
		State:   model.StatePublic,
	}

	tbID, err := genGlobalID(re.Store())
	require.NoError(t, err)
	tblInfo := &model.TableInfo{
		ID:      tbID,
		Name:    tbName,
		Columns: []*model.ColumnInfo{colInfo},
		Indices: []*model.IndexInfo{idxInfo},
		State:   model.StatePublic,
	}

	dbID, err := genGlobalID(re.Store())
	require.NoError(t, err)
	dbInfo := &model.DBInfo{
		ID:     dbID,
		Name:   dbName,
		Tables: []*model.TableInfo{tblInfo},
		State:  model.StatePublic,
	}
	tblInfo.DBID = dbInfo.ID

	dbInfos := []*model.DBInfo{dbInfo}
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
	err = kv.RunInNewTxn(ctx, re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).CreateDatabase(dbInfo)
		require.NoError(t, err)
		return errors.Trace(err)
	})
	require.NoError(t, err)

	builder, err := infoschema.NewBuilder(re, nil, nil).InitWithDBInfos(dbInfos, nil, nil, 1)
	require.NoError(t, err)

	txn, err := re.Store().Begin()
	require.NoError(t, err)
	checkApplyCreateNonExistsSchemaDoesNotPanic(t, txn, builder)
	checkApplyCreateNonExistsTableDoesNotPanic(t, txn, builder, dbID)
	err = txn.Rollback()
	require.NoError(t, err)

	is := builder.Build()

	schemaNames := infoschema.AllSchemaNames(is)
	require.Len(t, schemaNames, 3)
	require.True(t, testutil.CompareUnorderedStringSlice(schemaNames, []string{util.InformationSchemaName.O, util.MetricSchemaName.O, "Test"}))

	schemas := is.AllSchemas()
	require.Len(t, schemas, 3)

	require.True(t, is.SchemaExists(dbName))
	require.False(t, is.SchemaExists(noexist))

	schema, ok := is.SchemaByID(dbID)
	require.True(t, ok)
	require.NotNil(t, schema)

	schema, ok = is.SchemaByID(tbID)
	require.False(t, ok)
	require.Nil(t, schema)

	schema, ok = is.SchemaByName(dbName)
	require.True(t, ok)
	require.NotNil(t, schema)

	schema, ok = is.SchemaByName(noexist)
	require.False(t, ok)
	require.Nil(t, schema)

	schema, ok = infoschema.SchemaByTable(is, tblInfo)
	require.True(t, ok)
	require.NotNil(t, schema)

	noexistTblInfo := &model.TableInfo{ID: 12345, Name: tblInfo.Name}
	schema, ok = infoschema.SchemaByTable(is, noexistTblInfo)
	require.False(t, ok)
	require.Nil(t, schema)

	require.True(t, is.TableExists(dbName, tbName))
	require.False(t, is.TableExists(dbName, noexist))
	require.False(t, infoschema.TableIsView(is, dbName, tbName))
	require.False(t, infoschema.TableIsSequence(is, dbName, tbName))

	tb, ok := is.TableByID(tbID)
	require.True(t, ok)
	require.NotNil(t, tb)

	tb, ok = is.TableByID(dbID)
	require.False(t, ok)
	require.Nil(t, tb)

	tb, err = is.TableByName(dbName, tbName)
	require.NoError(t, err)
	require.NotNil(t, tb)

	_, err = is.TableByName(dbName, noexist)
	require.Error(t, err)

	tbs := is.SchemaTables(dbName)
	require.Len(t, tbs, 1)

	tbs = is.SchemaTables(noexist)
	require.Len(t, tbs, 0)

	// Make sure partitions table exists
	tb, err = is.TableByName(model.NewCIStr("information_schema"), model.NewCIStr("partitions"))
	require.NoError(t, err)
	require.NotNil(t, tb)

	err = kv.RunInNewTxn(ctx, re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).CreateTableOrView(dbID, dbName.L, tblInfo)
		require.NoError(t, err)
		return errors.Trace(err)
	})
	require.NoError(t, err)
	txn, err = re.Store().Begin()
	require.NoError(t, err)
	_, err = builder.ApplyDiff(meta.NewMeta(txn), &model.SchemaDiff{Type: model.ActionRenameTable, SchemaID: dbID, TableID: tbID, OldSchemaID: dbID})
	require.NoError(t, err)
	err = txn.Rollback()
	require.NoError(t, err)
	is = builder.Build()
	schema, ok = is.SchemaByID(dbID)
	require.True(t, ok)
	require.Equal(t, 1, len(schema.Tables))
}

func TestMockInfoSchema(t *testing.T) {
	tblID := int64(1234)
	tblName := model.NewCIStr("tbl_m")
	tableInfo := &model.TableInfo{
		ID:    tblID,
		Name:  tblName,
		State: model.StatePublic,
	}
	colInfo := &model.ColumnInfo{
		State:     model.StatePublic,
		Offset:    0,
		Name:      model.NewCIStr("h"),
		FieldType: *types.NewFieldType(mysql.TypeLong),
		ID:        1,
	}
	tableInfo.Columns = []*model.ColumnInfo{colInfo}
	is := infoschema.MockInfoSchema([]*model.TableInfo{tableInfo})
	tbl, ok := is.TableByID(tblID)
	require.True(t, ok)
	require.Equal(t, tblName, tbl.Meta().Name)
	require.Equal(t, colInfo, tbl.Cols()[0].ColumnInfo)
}

func checkApplyCreateNonExistsSchemaDoesNotPanic(t *testing.T, txn kv.Transaction, builder *infoschema.Builder) {
	m := meta.NewMeta(txn)
	_, err := builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionCreateSchema, SchemaID: 999})
	require.True(t, infoschema.ErrDatabaseNotExists.Equal(err))
}

func checkApplyCreateNonExistsTableDoesNotPanic(t *testing.T, txn kv.Transaction, builder *infoschema.Builder, dbID int64) {
	m := meta.NewMeta(txn)
	_, err := builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionCreateTable, SchemaID: dbID, TableID: 999})
	require.True(t, infoschema.ErrTableNotExists.Equal(err))
}

// TestInfoTables makes sure that all tables of information_schema could be found in infoschema handle.
func TestInfoTables(t *testing.T) {
	re := createAutoIDRequirement(t)

	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	builder, err := infoschema.NewBuilder(re, nil, nil).InitWithDBInfos(nil, nil, nil, 0)
	require.NoError(t, err)
	is := builder.Build()

	infoTables := []string{
		"SCHEMATA",
		"TABLES",
		"COLUMNS",
		"STATISTICS",
		"CHARACTER_SETS",
		"COLLATIONS",
		"FILES",
		"PROFILING",
		"PARTITIONS",
		"KEY_COLUMN_USAGE",
		"REFERENTIAL_CONSTRAINTS",
		"SESSION_VARIABLES",
		"PLUGINS",
		"TABLE_CONSTRAINTS",
		"TRIGGERS",
		"USER_PRIVILEGES",
		"ENGINES",
		"VIEWS",
		"ROUTINES",
		"SCHEMA_PRIVILEGES",
		"COLUMN_PRIVILEGES",
		"TABLE_PRIVILEGES",
		"PARAMETERS",
		"EVENTS",
		"GLOBAL_STATUS",
		"GLOBAL_VARIABLES",
		"SESSION_STATUS",
		"OPTIMIZER_TRACE",
		"TABLESPACES",
		"COLLATION_CHARACTER_SET_APPLICABILITY",
		"PROCESSLIST",
		"TIDB_TRX",
		"DEADLOCKS",
		"PLACEMENT_POLICIES",
		"TRX_SUMMARY",
		"RESOURCE_GROUPS",
	}
	for _, tbl := range infoTables {
		tb, err1 := is.TableByName(util.InformationSchemaName, model.NewCIStr(tbl))
		require.Nil(t, err1)
		require.NotNil(t, tb)
	}
}

func genGlobalID(store kv.Storage) (int64, error) {
	var globalID int64
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
	err := kv.RunInNewTxn(ctx, store, true, func(ctx context.Context, txn kv.Transaction) error {
		var err error
		globalID, err = meta.NewMeta(txn).GenGlobalID()
		return errors.Trace(err)
	})
	return globalID + 100, errors.Trace(err)
}

func TestBuildSchemaWithGlobalTemporaryTable(t *testing.T) {
	re := createAutoIDRequirement(t)
	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	dbInfo := &model.DBInfo{
		ID:     1,
		Name:   model.NewCIStr("test"),
		Tables: []*model.TableInfo{},
		State:  model.StatePublic,
	}
	dbInfos := []*model.DBInfo{dbInfo}
	builder, err := infoschema.NewBuilder(re, nil, nil).InitWithDBInfos(dbInfos, nil, nil, 1)
	require.NoError(t, err)
	is := builder.Build()
	require.False(t, is.HasTemporaryTable())
	db, ok := is.SchemaByName(model.NewCIStr("test"))
	require.True(t, ok)
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
	err = kv.RunInNewTxn(ctx, re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).CreateDatabase(dbInfo)
		require.NoError(t, err)
		return errors.Trace(err)
	})
	require.NoError(t, err)

	doChange := func(changes ...func(m *meta.Meta, builder *infoschema.Builder)) infoschema.InfoSchema {
		ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
		curIs := is
		err := kv.RunInNewTxn(ctx, re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
			m := meta.NewMeta(txn)
			for _, change := range changes {
				builder, err := infoschema.NewBuilder(re, nil, nil).InitWithOldInfoSchema(curIs)
				require.NoError(t, err)
				change(m, builder)
				curIs = builder.Build()
			}
			return nil
		})
		require.NoError(t, err)
		return curIs
	}

	createGlobalTemporaryTableChange := func(tblID int64) func(m *meta.Meta, builder *infoschema.Builder) {
		return func(m *meta.Meta, builder *infoschema.Builder) {
			err := m.CreateTableOrView(db.ID, db.Name.L, &model.TableInfo{
				ID:            tblID,
				TempTableType: model.TempTableGlobal,
				State:         model.StatePublic,
			})
			require.NoError(t, err)
			_, err = builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionCreateTable, SchemaID: db.ID, TableID: tblID})
			require.NoError(t, err)
		}
	}

	createNormalTableChange := func(tblID int64) func(m *meta.Meta, builder *infoschema.Builder) {
		return func(m *meta.Meta, builder *infoschema.Builder) {
			err := m.CreateTableOrView(db.ID, db.Name.L, &model.TableInfo{
				ID:    tblID,
				State: model.StatePublic,
			})
			require.NoError(t, err)
			_, err = builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionCreateTable, SchemaID: db.ID, TableID: tblID})
			require.NoError(t, err)
		}
	}

	dropTableChange := func(tblID int64) func(m *meta.Meta, builder *infoschema.Builder) {
		return func(m *meta.Meta, builder *infoschema.Builder) {
			err := m.DropTableOrView(db.ID, db.Name.L, tblID, "")
			require.NoError(t, err)
			_, err = builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionDropTable, SchemaID: db.ID, TableID: tblID})
			require.NoError(t, err)
		}
	}

	truncateGlobalTemporaryTableChange := func(tblID, newTblID int64) func(m *meta.Meta, builder *infoschema.Builder) {
		return func(m *meta.Meta, builder *infoschema.Builder) {
			err := m.DropTableOrView(db.ID, db.Name.L, tblID, "")
			require.NoError(t, err)

			err = m.CreateTableOrView(db.ID, db.Name.L, &model.TableInfo{
				ID:            newTblID,
				TempTableType: model.TempTableGlobal,
				State:         model.StatePublic,
			})
			require.NoError(t, err)
			_, err = builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionTruncateTable, SchemaID: db.ID, OldTableID: tblID, TableID: newTblID})
			require.NoError(t, err)
		}
	}

	alterTableChange := func(tblID int64) func(m *meta.Meta, builder *infoschema.Builder) {
		return func(m *meta.Meta, builder *infoschema.Builder) {
			_, err := builder.ApplyDiff(m, &model.SchemaDiff{Type: model.ActionAddColumn, SchemaID: db.ID, TableID: tblID})
			require.NoError(t, err)
		}
	}

	// create table
	tbID, err := genGlobalID(re.Store())
	require.NoError(t, err)
	newIS := doChange(
		createGlobalTemporaryTableChange(tbID),
	)
	require.True(t, newIS.HasTemporaryTable())

	// full load
	newDB, ok := newIS.SchemaByName(model.NewCIStr("test"))
	require.True(t, ok)
	builder, err = infoschema.NewBuilder(re, nil, nil).InitWithDBInfos([]*model.DBInfo{newDB}, newIS.AllPlacementPolicies(), newIS.AllResourceGroups(), newIS.SchemaMetaVersion())
	require.NoError(t, err)
	require.True(t, builder.Build().HasTemporaryTable())

	// create and then drop
	tbID, err = genGlobalID(re.Store())
	require.NoError(t, err)
	require.False(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		dropTableChange(tbID),
	).HasTemporaryTable())

	// create and then alter
	tbID, err = genGlobalID(re.Store())
	require.NoError(t, err)
	require.True(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		alterTableChange(tbID),
	).HasTemporaryTable())

	// create and truncate
	tbID, err = genGlobalID(re.Store())
	require.NoError(t, err)
	newTbID, err := genGlobalID(re.Store())
	require.NoError(t, err)
	require.True(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		truncateGlobalTemporaryTableChange(tbID, newTbID),
	).HasTemporaryTable())

	// create two and drop one
	tbID, err = genGlobalID(re.Store())
	require.NoError(t, err)
	tbID2, err := genGlobalID(re.Store())
	require.NoError(t, err)
	require.True(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		createGlobalTemporaryTableChange(tbID2),
		dropTableChange(tbID),
	).HasTemporaryTable())

	// create temporary and then create normal
	tbID, err = genGlobalID(re.Store())
	require.NoError(t, err)
	tbID2, err = genGlobalID(re.Store())
	require.NoError(t, err)
	require.True(t, doChange(
		createGlobalTemporaryTableChange(tbID),
		createNormalTableChange(tbID2),
	).HasTemporaryTable())
}

func TestBuildBundle(t *testing.T) {
	store, dom := testkit.CreateMockStoreAndDomain(t)

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("drop table if exists t1, t2")
	tk.MustExec("drop placement policy if exists p1")
	tk.MustExec("drop placement policy if exists p2")
	tk.MustExec("create placement policy p1 followers=1")
	tk.MustExec("create placement policy p2 followers=2")
	tk.MustExec(`create table t1(a int primary key) placement policy p1 partition by range(a) (
		partition p1 values less than (10) placement policy p2,
		partition p2 values less than (20)
	)`)
	tk.MustExec("create table t2(a int)")
	defer func() {
		tk.MustExec("drop table if exists t1, t2")
		tk.MustExec("drop placement policy if exists p1")
		tk.MustExec("drop placement policy if exists p2")
	}()

	is := domain.GetDomain(tk.Session()).InfoSchema()
	db, ok := is.SchemaByName(model.NewCIStr("test"))
	require.True(t, ok)

	tbl1, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	require.NoError(t, err)

	tbl2, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t2"))
	require.NoError(t, err)

	var p1 model.PartitionDefinition
	for _, par := range tbl1.Meta().Partition.Definitions {
		if par.Name.L == "p1" {
			p1 = par
			break
		}
	}
	require.NotNil(t, p1)

	var tb1Bundle, p1Bundle *placement.Bundle

	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
	require.NoError(t, kv.RunInNewTxn(ctx, store, false, func(ctx context.Context, txn kv.Transaction) (err error) {
		m := meta.NewMeta(txn)
		tb1Bundle, err = placement.NewTableBundle(m, tbl1.Meta())
		require.NoError(t, err)
		require.NotNil(t, tb1Bundle)

		p1Bundle, err = placement.NewPartitionBundle(m, p1)
		require.NoError(t, err)
		require.NotNil(t, p1Bundle)
		return
	}))

	assertBundle := func(checkIS infoschema.InfoSchema, id int64, expected *placement.Bundle) {
		actual, ok := checkIS.PlacementBundleByPhysicalTableID(id)
		if expected == nil {
			require.False(t, ok)
			return
		}

		expectedJSON, err := json.Marshal(expected)
		require.NoError(t, err)
		actualJSON, err := json.Marshal(actual)
		require.NoError(t, err)
		require.Equal(t, string(expectedJSON), string(actualJSON))
	}

	assertBundle(is, tbl1.Meta().ID, tb1Bundle)
	assertBundle(is, tbl2.Meta().ID, nil)
	assertBundle(is, p1.ID, p1Bundle)

	builder, err := infoschema.NewBuilder(dom, nil, nil).InitWithDBInfos([]*model.DBInfo{db}, is.AllPlacementPolicies(), is.AllResourceGroups(), is.SchemaMetaVersion())
	require.NoError(t, err)
	is2 := builder.Build()
	assertBundle(is2, tbl1.Meta().ID, tb1Bundle)
	assertBundle(is2, tbl2.Meta().ID, nil)
	assertBundle(is2, p1.ID, p1Bundle)
}

func TestLocalTemporaryTables(t *testing.T) {
	re := createAutoIDRequirement(t)
	var err error
	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	createNewSchemaInfo := func(schemaName string) *model.DBInfo {
		schemaID, err := genGlobalID(re.Store())
		require.NoError(t, err)
		return &model.DBInfo{
			ID:    schemaID,
			Name:  model.NewCIStr(schemaName),
			State: model.StatePublic,
		}
	}

	createNewTable := func(schemaID int64, tbName string) table.Table {
		colID, err := genGlobalID(re.Store())
		require.NoError(t, err)

		colInfo := &model.ColumnInfo{
			ID:        colID,
			Name:      model.NewCIStr("col1"),
			Offset:    0,
			FieldType: *types.NewFieldType(mysql.TypeLonglong),
			State:     model.StatePublic,
		}

		tbID, err := genGlobalID(re.Store())
		require.NoError(t, err)

		tblInfo := &model.TableInfo{
			ID:      tbID,
			Name:    model.NewCIStr(tbName),
			Columns: []*model.ColumnInfo{colInfo},
			Indices: []*model.IndexInfo{},
			State:   model.StatePublic,
			DBID:    schemaID,
		}

		allocs := autoid.NewAllocatorsFromTblInfo(re, schemaID, tblInfo)
		tbl, err := table.TableFromMeta(allocs, tblInfo)
		require.NoError(t, err)

		return tbl
	}

	assertTableByName := func(sc *infoschema.SessionTables, schemaName, tableName string, schema *model.DBInfo, tb table.Table) {
		got, ok := sc.TableByName(model.NewCIStr(schemaName), model.NewCIStr(tableName))
		if tb == nil {
			require.Nil(t, schema)
			require.False(t, ok)
			require.Nil(t, got)
		} else {
			require.NotNil(t, schema)
			require.True(t, ok)
			require.Equal(t, tb, got)
		}
	}

	assertTableExists := func(sc *infoschema.SessionTables, schemaName, tableName string, exists bool) {
		got := sc.TableExists(model.NewCIStr(schemaName), model.NewCIStr(tableName))
		require.Equal(t, exists, got)
	}

	assertTableByID := func(sc *infoschema.SessionTables, tbID int64, schema *model.DBInfo, tb table.Table) {
		got, ok := sc.TableByID(tbID)
		if tb == nil {
			require.Nil(t, schema)
			require.False(t, ok)
			require.Nil(t, got)
		} else {
			require.NotNil(t, schema)
			require.True(t, ok)
			require.Equal(t, tb, got)
		}
	}

	assertSchemaByTable := func(sc *infoschema.SessionTables, db *model.DBInfo, tb *model.TableInfo) {
		got, ok := sc.SchemaByID(tb.DBID)
		if db == nil {
			require.Nil(t, got)
			require.False(t, ok)
		} else {
			require.NotNil(t, got)
			require.Equal(t, db.Name.L, got.Name.L)
			require.True(t, ok)
		}
	}

	sc := infoschema.NewSessionTables()
	db1 := createNewSchemaInfo("db1")
	tb11 := createNewTable(db1.ID, "tb1")
	tb12 := createNewTable(db1.ID, "Tb2")
	tb13 := createNewTable(db1.ID, "tb3")

	// db1b has the same name with db1
	db1b := createNewSchemaInfo("db1b")
	tb15 := createNewTable(db1b.ID, "tb5")
	tb16 := createNewTable(db1b.ID, "tb6")
	tb17 := createNewTable(db1b.ID, "tb7")

	db2 := createNewSchemaInfo("db2")
	tb21 := createNewTable(db2.ID, "tb1")
	tb22 := createNewTable(db2.ID, "TB2")
	tb24 := createNewTable(db2.ID, "tb4")

	prepareTables := []struct {
		db *model.DBInfo
		tb table.Table
	}{
		{db1, tb11}, {db1, tb12}, {db1, tb13},
		{db1b, tb15}, {db1b, tb16}, {db1b, tb17},
		{db2, tb21}, {db2, tb22}, {db2, tb24},
	}

	for _, p := range prepareTables {
		err = sc.AddTable(p.db, p.tb)
		require.NoError(t, err)
	}

	// test exist tables
	for _, p := range prepareTables {
		dbName := p.db.Name
		tbName := p.tb.Meta().Name

		assertTableByName(sc, dbName.O, tbName.O, p.db, p.tb)
		assertTableByName(sc, dbName.L, tbName.L, p.db, p.tb)
		assertTableByName(
			sc,
			strings.ToUpper(dbName.L[:1])+dbName.L[1:],
			strings.ToUpper(tbName.L[:1])+tbName.L[1:],
			p.db, p.tb,
		)

		assertTableExists(sc, dbName.O, tbName.O, true)
		assertTableExists(sc, dbName.L, tbName.L, true)
		assertTableExists(
			sc,
			strings.ToUpper(dbName.L[:1])+dbName.L[1:],
			strings.ToUpper(tbName.L[:1])+tbName.L[1:],
			true,
		)

		assertTableByID(sc, p.tb.Meta().ID, p.db, p.tb)
		assertSchemaByTable(sc, p.db, p.tb.Meta())
	}

	// test add dup table
	err = sc.AddTable(db1, tb11)
	require.True(t, infoschema.ErrTableExists.Equal(err))
	err = sc.AddTable(db1b, tb15)
	require.True(t, infoschema.ErrTableExists.Equal(err))
	err = sc.AddTable(db1b, tb11)
	require.True(t, infoschema.ErrTableExists.Equal(err))
	db1c := createNewSchemaInfo("db1")
	err = sc.AddTable(db1c, createNewTable(db1c.ID, "tb1"))
	require.True(t, infoschema.ErrTableExists.Equal(err))
	err = sc.AddTable(db1b, tb11)
	require.True(t, infoschema.ErrTableExists.Equal(err))
	tb11.Meta().DBID = 0 // SchemaByTable will get incorrect result if not reset here.

	// failed add has no effect
	assertTableByName(sc, db1.Name.L, tb11.Meta().Name.L, db1, tb11)

	// delete some tables
	require.True(t, sc.RemoveTable(model.NewCIStr("db1"), model.NewCIStr("tb1")))
	require.True(t, sc.RemoveTable(model.NewCIStr("Db2"), model.NewCIStr("tB2")))
	tb22.Meta().DBID = 0 // SchemaByTable will get incorrect result if not reset here.
	require.False(t, sc.RemoveTable(model.NewCIStr("db1"), model.NewCIStr("tbx")))
	require.False(t, sc.RemoveTable(model.NewCIStr("dbx"), model.NewCIStr("tbx")))

	// test non exist tables by name
	for _, c := range []struct{ dbName, tbName string }{
		{"db1", "tb1"}, {"db1", "tb4"}, {"db1", "tbx"},
		{"db2", "tb2"}, {"db2", "tb3"}, {"db2", "tbx"},
		{"dbx", "tb1"},
	} {
		assertTableByName(sc, c.dbName, c.tbName, nil, nil)
		assertTableExists(sc, c.dbName, c.tbName, false)
	}

	// test non exist tables by id
	nonExistID, err := genGlobalID(re.Store())
	require.NoError(t, err)

	for _, id := range []int64{nonExistID, tb11.Meta().ID, tb22.Meta().ID} {
		assertTableByID(sc, id, nil, nil)
	}

	// test non exist table schemaByTable
	assertSchemaByTable(sc, nil, tb11.Meta())
	assertSchemaByTable(sc, nil, tb22.Meta())

	// test SessionExtendedInfoSchema
	dbTest := createNewSchemaInfo("test")
	tmpTbTestA := createNewTable(dbTest.ID, "tba")
	normalTbTestA := createNewTable(dbTest.ID, "tba")
	normalTbTestB := createNewTable(dbTest.ID, "tbb")
	normalTbTestC := createNewTable(db1.ID, "tbc")

	is := &infoschema.SessionExtendedInfoSchema{
		InfoSchema:           infoschema.MockInfoSchema([]*model.TableInfo{normalTbTestA.Meta(), normalTbTestB.Meta()}),
		LocalTemporaryTables: sc,
	}

	err = sc.AddTable(dbTest, tmpTbTestA)
	require.NoError(t, err)

	// test TableByName
	tbl, err := is.TableByName(dbTest.Name, normalTbTestA.Meta().Name)
	require.NoError(t, err)
	require.Equal(t, tmpTbTestA, tbl)
	tbl, err = is.TableByName(dbTest.Name, normalTbTestB.Meta().Name)
	require.NoError(t, err)
	require.Equal(t, normalTbTestB.Meta(), tbl.Meta())
	tbl, err = is.TableByName(db1.Name, tb11.Meta().Name)
	require.True(t, infoschema.ErrTableNotExists.Equal(err))
	require.Nil(t, tbl)
	tbl, err = is.TableByName(db1.Name, tb12.Meta().Name)
	require.NoError(t, err)
	require.Equal(t, tb12, tbl)

	// test TableByID
	tbl, ok := is.TableByID(normalTbTestA.Meta().ID)
	require.True(t, ok)
	require.Equal(t, normalTbTestA.Meta(), tbl.Meta())
	tbl, ok = is.TableByID(normalTbTestB.Meta().ID)
	require.True(t, ok)
	require.Equal(t, normalTbTestB.Meta(), tbl.Meta())
	tbl, ok = is.TableByID(tmpTbTestA.Meta().ID)
	require.True(t, ok)
	require.Equal(t, tmpTbTestA, tbl)
	tbl, ok = is.TableByID(tb12.Meta().ID)
	require.True(t, ok)
	require.Equal(t, tb12, tbl)

	// test SchemaByTable
	info, ok := is.SchemaByID(normalTbTestA.Meta().DBID)
	require.True(t, ok)
	require.Equal(t, dbTest.Name.L, info.Name.L)
	info, ok = is.SchemaByID(normalTbTestB.Meta().DBID)
	require.True(t, ok)
	require.Equal(t, dbTest.Name.L, info.Name.L)
	info, ok = is.SchemaByID(tmpTbTestA.Meta().DBID)
	require.True(t, ok)
	require.Equal(t, dbTest.Name.L, info.Name.L)
	// SchemaByTable also returns DBInfo when the schema is not in the infoSchema but the table is an existing tmp table.
	info, ok = is.SchemaByID(tb12.Meta().DBID)
	require.True(t, ok)
	require.Equal(t, db1.Name.L, info.Name.L)
	// SchemaByTable returns nil when the schema is not in the infoSchema and the table is an non-existing normal table.
	normalTbTestC.Meta().DBID = 0 // normalTbTestC is not added to any db, reset the DBID to avoid misuse
	info, ok = is.SchemaByID(normalTbTestC.Meta().DBID)
	require.False(t, ok)
	require.Nil(t, info)
	// SchemaByTable returns nil when the schema is not in the infoSchema and the table is an non-existing tmp table.
	info, ok = is.SchemaByID(tb22.Meta().DBID)
	require.False(t, ok)
	require.Nil(t, info)
}

// TestInfoSchemaCreateTableLike tests the table's column ID and index ID for memory database.
func TestInfoSchemaCreateTableLike(t *testing.T) {
	store := testkit.CreateMockStore(t)

	tk := testkit.NewTestKit(t, store)
	tk.MustExec("use test")
	tk.MustExec("create table vi like information_schema.variables_info;")
	tk.MustExec("alter table vi modify min_value varchar(32);")
	tk.MustExec("create table u like metrics_schema.up;")
	tk.MustExec("alter table u modify job int;")
	tk.MustExec("create table so like performance_schema.setup_objects;")
	tk.MustExec("alter table so modify object_name int;")

	tk.MustExec("create table t1 like information_schema.variables_info;")
	tk.MustExec("alter table t1 add column c varchar(32);")
	is := domain.GetDomain(tk.Session()).InfoSchema()
	tbl, err := is.TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	require.NoError(t, err)
	tblInfo := tbl.Meta()
	require.Equal(t, tblInfo.Columns[8].Name.O, "c")
	require.Equal(t, tblInfo.Columns[8].ID, int64(9))
	tk.MustExec("alter table t1 add index idx(c);")
	is = domain.GetDomain(tk.Session()).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test"), model.NewCIStr("t1"))
	require.NoError(t, err)
	tblInfo = tbl.Meta()
	require.Equal(t, tblInfo.Indices[0].Name.O, "idx")
	require.Equal(t, tblInfo.Indices[0].ID, int64(1))

	// metrics_schema
	tk.MustExec("create table t2 like metrics_schema.up;")
	tk.MustExec("alter table t2 add column c varchar(32);")
	is = domain.GetDomain(tk.Session()).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test"), model.NewCIStr("t2"))
	require.NoError(t, err)
	tblInfo = tbl.Meta()
	require.Equal(t, tblInfo.Columns[4].Name.O, "c")
	require.Equal(t, tblInfo.Columns[4].ID, int64(5))
	tk.MustExec("alter table t2 add index idx(c);")
	is = domain.GetDomain(tk.Session()).InfoSchema()
	tbl, err = is.TableByName(model.NewCIStr("test"), model.NewCIStr("t2"))
	require.NoError(t, err)
	tblInfo = tbl.Meta()
	require.Equal(t, tblInfo.Indices[0].Name.O, "idx")
	require.Equal(t, tblInfo.Indices[0].ID, int64(1))
}

func TestEnableInfoSchemaV2(t *testing.T) {
	store := testkit.CreateMockStore(t)
	tk := testkit.NewTestKit(t, store)
	// Test the @@tidb_enable_infoschema_v2 variable.
	tk.MustQuery("select @@tidb_schema_cache_size").Check(testkit.Rows("0"))
	tk.MustQuery("select @@global.tidb_schema_cache_size").Check(testkit.Rows("0"))
	require.Equal(t, variable.SchemaCacheSize.Load(), int64(0))

	// Modify it.
	tk.MustExec("set @@global.tidb_schema_cache_size = 1024")
	tk.MustQuery("select @@global.tidb_schema_cache_size").Check(testkit.Rows("1024"))
	tk.MustQuery("select @@tidb_schema_cache_size").Check(testkit.Rows("1024"))
	require.Equal(t, variable.SchemaCacheSize.Load(), int64(1024))

	tk.MustExec("use test")
	tk.MustExec("create table v2 (id int)")

	// Check the InfoSchema used is V2.
	is := domain.GetDomain(tk.Session()).InfoSchema()
	require.True(t, infoschema.IsV2(is))

	// Execute some basic operations under infoschema v2.
	tk.MustQuery("show tables").Check(testkit.Rows("v2"))
	tk.MustExec("drop table v2")
	tk.MustExec("create table v1 (id int)")

	// Change infoschema back to v1 and check again.
	tk.MustExec("set @@global.tidb_schema_cache_size = 0")
	tk.MustQuery("select @@global.tidb_schema_cache_size").Check(testkit.Rows("0"))
	require.Equal(t, variable.SchemaCacheSize.Load(), int64(0))

	tk.MustExec("drop table v1")
	is = domain.GetDomain(tk.Session()).InfoSchema()
	require.False(t, infoschema.IsV2(is))
}

type infoschemaTestContext struct {
	// only test one db.
	dbInfo *model.DBInfo
	t      *testing.T
	re     autoid.Requirement
	ctx    context.Context
	is     infoschema.InfoSchema
}

func (tc *infoschemaTestContext) createSchema() {
	dbID, err := genGlobalID(tc.re.Store())
	require.NoError(tc.t, err)
	dbInfo := &model.DBInfo{
		ID:     dbID,
		Name:   model.NewCIStr("test"),
		Tables: []*model.TableInfo{},
		State:  model.StatePublic,
	}
	ctx := kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL)
	err = kv.RunInNewTxn(ctx, tc.re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).CreateDatabase(dbInfo)
		require.NoError(tc.t, err)
		return errors.Trace(err)
	})
	require.NoError(tc.t, err)
	tc.dbInfo = dbInfo

	// init infoschema
	builder, err := infoschema.NewBuilder(tc.re, nil, nil).InitWithDBInfos([]*model.DBInfo{tc.dbInfo}, nil, nil, 1)
	require.NoError(tc.t, err)
	tc.is = builder.Build()
}

func (tc *infoschemaTestContext) runCreateSchema() {
	// create schema
	tc.createSchema()

	tc.applyDiffAddCheck(&model.SchemaDiff{Type: model.ActionCreateSchema, SchemaID: tc.dbInfo.ID}, func(tc *infoschemaTestContext) {
		dbInfo, ok := tc.is.SchemaByID(tc.dbInfo.ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, dbInfo.Name, tc.dbInfo.Name)
	})
}

func (tc *infoschemaTestContext) dropSchema() {
	err := kv.RunInNewTxn(tc.ctx, tc.re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).DropDatabase(tc.dbInfo.ID, tc.dbInfo.Name.O)
		require.NoError(tc.t, err)
		return errors.Trace(err)
	})
	require.NoError(tc.t, err)
}

func (tc *infoschemaTestContext) runDropSchema() {
	// create schema
	tc.runCreateSchema()

	// drop schema
	tc.dropSchema()
	tc.applyDiffAddCheck(&model.SchemaDiff{Type: model.ActionDropSchema, SchemaID: tc.dbInfo.ID}, func(tc *infoschemaTestContext) {
		_, ok := tc.is.SchemaByID(tc.dbInfo.ID)
		require.False(tc.t, ok)
	})
}

func (tc *infoschemaTestContext) createTable(tblName string) int64 {
	colName := model.NewCIStr("a")

	colID, err := genGlobalID(tc.re.Store())
	require.NoError(tc.t, err)
	colInfo := &model.ColumnInfo{
		ID:        colID,
		Name:      colName,
		Offset:    0,
		FieldType: *types.NewFieldType(mysql.TypeLonglong),
		State:     model.StatePublic,
	}

	tblID, err := genGlobalID(tc.re.Store())
	require.NoError(tc.t, err)
	tblInfo := &model.TableInfo{
		ID:      tblID,
		Name:    model.NewCIStr(tblName),
		Columns: []*model.ColumnInfo{colInfo},
		State:   model.StatePublic,
	}

	err = kv.RunInNewTxn(tc.ctx, tc.re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).CreateTableOrView(tc.dbInfo.ID, tc.dbInfo.Name.O, tblInfo)
		require.NoError(tc.t, err)
		return errors.Trace(err)
	})
	require.NoError(tc.t, err)
	return tblID
}

func (tc *infoschemaTestContext) runCreateTable(tblName string) int64 {
	if tc.dbInfo == nil {
		tc.runCreateSchema()
	}
	// create table
	tblID := tc.createTable(tblName)

	tc.applyDiffAddCheck(&model.SchemaDiff{Type: model.ActionCreateTable, SchemaID: tc.dbInfo.ID, TableID: tblID}, func(tc *infoschemaTestContext) {
		tbl, ok := tc.is.TableByID(tblID)
		require.True(tc.t, ok)
		require.Equal(tc.t, tbl.Meta().Name.O, tblName)
	})
	return tblID
}

func (tc *infoschemaTestContext) dropTable(tblName string, tblID int64) {
	err := kv.RunInNewTxn(tc.ctx, tc.re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).DropTableOrView(tc.dbInfo.ID, tc.dbInfo.Name.O, tblID, tblName)
		require.NoError(tc.t, err)
		return errors.Trace(err)
	})
	require.NoError(tc.t, err)
}

func (tc *infoschemaTestContext) runDropTable(tblName string) {
	// createTable
	tblID := tc.runCreateTable(tblName)

	// dropTable
	tc.dropTable(tblName, tblID)
	tc.applyDiffAddCheck(&model.SchemaDiff{Type: model.ActionDropTable, SchemaID: tc.dbInfo.ID, TableID: tblID}, func(tc *infoschemaTestContext) {
		tbl, ok := tc.is.TableByID(tblID)
		require.False(tc.t, ok)
		require.Nil(tc.t, tbl)
	})
}

func (tc *infoschemaTestContext) runModifyTable(tblName string, tp model.ActionType) {
	switch tp {
	case model.ActionAddColumn:
		tc.runAddColumn(tblName)
	case model.ActionModifyColumn:
		tc.runModifyColumn(tblName)
	default:
		return
	}
}

func (tc *infoschemaTestContext) runAddColumn(tblName string) {
	tbl, err := tc.is.TableByName(tc.dbInfo.Name, model.NewCIStr(tblName))
	require.NoError(tc.t, err)

	tc.addColumn(tbl.Meta())
	tc.applyDiffAddCheck(&model.SchemaDiff{Type: model.ActionAddColumn, SchemaID: tc.dbInfo.ID, TableID: tbl.Meta().ID}, func(tc *infoschemaTestContext) {
		tbl, ok := tc.is.TableByID(tbl.Meta().ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, 2, len(tbl.Cols()))
	})
}

func (tc *infoschemaTestContext) addColumn(tblInfo *model.TableInfo) {
	colName := model.NewCIStr("b")
	colID, err := genGlobalID(tc.re.Store())
	require.NoError(tc.t, err)
	colInfo := &model.ColumnInfo{
		ID:        colID,
		Name:      colName,
		Offset:    1,
		FieldType: *types.NewFieldType(mysql.TypeLonglong),
		State:     model.StatePublic,
	}

	tblInfo.Columns = append(tblInfo.Columns, colInfo)
	err = kv.RunInNewTxn(tc.ctx, tc.re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).UpdateTable(tc.dbInfo.ID, tblInfo)
		require.NoError(tc.t, err)
		return errors.Trace(err)
	})
	require.NoError(tc.t, err)
}

func (tc *infoschemaTestContext) runModifyColumn(tblName string) {
	tbl, err := tc.is.TableByName(tc.dbInfo.Name, model.NewCIStr(tblName))
	require.NoError(tc.t, err)

	tc.modifyColumn(tbl.Meta())
	tc.applyDiffAddCheck(&model.SchemaDiff{Type: model.ActionModifyColumn, SchemaID: tc.dbInfo.ID, TableID: tbl.Meta().ID}, func(tc *infoschemaTestContext) {
		tbl, ok := tc.is.TableByID(tbl.Meta().ID)
		require.True(tc.t, ok)
		require.Equal(tc.t, "test", tbl.Cols()[0].Comment)
	})
}

func (tc *infoschemaTestContext) modifyColumn(tblInfo *model.TableInfo) {
	columnInfo := tblInfo.Columns
	columnInfo[0].Comment = "test"

	err := kv.RunInNewTxn(tc.ctx, tc.re.Store(), true, func(ctx context.Context, txn kv.Transaction) error {
		err := meta.NewMeta(txn).UpdateTable(tc.dbInfo.ID, tblInfo)
		require.NoError(tc.t, err)
		return errors.Trace(err)
	})
	require.NoError(tc.t, err)
}

func (tc *infoschemaTestContext) applyDiffAddCheck(diff *model.SchemaDiff, checkFn func(tc *infoschemaTestContext)) {
	txn, err := tc.re.Store().Begin()
	require.NoError(tc.t, err)

	builder, err := infoschema.NewBuilder(tc.re, nil, nil).InitWithOldInfoSchema(tc.is)
	require.NoError(tc.t, err)
	// applyDiff
	_, err = builder.ApplyDiff(meta.NewMeta(txn), diff)
	require.NoError(tc.t, err)
	tc.is = builder.Build()
	checkFn(tc)
}

func (tc *infoschemaTestContext) clear() {
	tc.dbInfo = nil
	tc.is = nil
}

func TestApplyDiff(t *testing.T) {
	re := createAutoIDRequirement(t)
	defer func() {
		err := re.Store().Close()
		require.NoError(t, err)
	}()

	tc := &infoschemaTestContext{
		t:   t,
		re:  re,
		ctx: kv.WithInternalSourceType(context.Background(), kv.InternalTxnDDL),
	}

	tc.runCreateSchema()
	tc.clear()
	tc.runDropSchema()
	tc.clear()
	tc.runCreateTable("test")
	tc.clear()
	tc.runDropTable("test")
	tc.clear()

	tc.runCreateTable("test")
	tc.runModifyTable("test", model.ActionAddColumn)
	tc.runModifyTable("test", model.ActionModifyColumn)
	// TODO check all actions..
}
