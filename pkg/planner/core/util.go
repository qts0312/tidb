// Copyright 2017 PingCAP, Inc.
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

package core

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pingcap/tidb/pkg/expression"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/planner/core/base"
	"github.com/pingcap/tidb/pkg/planner/core/operator/baseimpl"
	"github.com/pingcap/tidb/pkg/planner/core/operator/logicalop"
	"github.com/pingcap/tidb/pkg/planner/core/operator/physicalop"
	"github.com/pingcap/tidb/pkg/planner/util"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/codec"
	"github.com/pingcap/tidb/pkg/util/set"
	"github.com/pingcap/tidb/pkg/util/size"
)

// AggregateFuncExtractor visits Expr tree.
// It collects AggregateFuncExpr from AST Node.
type AggregateFuncExtractor struct {
	// skipAggMap stores correlated aggregate functions which have been built in outer query,
	// so extractor in sub-query will skip these aggregate functions.
	skipAggMap map[*ast.AggregateFuncExpr]*expression.CorrelatedColumn
	// AggFuncs is the collected AggregateFuncExprs.
	AggFuncs []*ast.AggregateFuncExpr
}

// Enter implements Visitor interface.
func (*AggregateFuncExtractor) Enter(n ast.Node) (ast.Node, bool) {
	switch n.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		return n, true
	}
	return n, false
}

// Leave implements Visitor interface.
func (a *AggregateFuncExtractor) Leave(n ast.Node) (ast.Node, bool) {
	//nolint: revive
	switch v := n.(type) {
	case *ast.AggregateFuncExpr:
		if _, ok := a.skipAggMap[v]; !ok {
			a.AggFuncs = append(a.AggFuncs, v)
		}
	}
	return n, true
}

// WindowFuncExtractor visits Expr tree.
// It converts ColumnNameExpr to WindowFuncExpr and collects WindowFuncExpr.
type WindowFuncExtractor struct {
	// WindowFuncs is the collected WindowFuncExprs.
	windowFuncs []*ast.WindowFuncExpr
}

// Enter implements Visitor interface.
func (*WindowFuncExtractor) Enter(n ast.Node) (ast.Node, bool) {
	switch n.(type) {
	case *ast.SelectStmt, *ast.SetOprStmt:
		return n, true
	}
	return n, false
}

// Leave implements Visitor interface.
func (a *WindowFuncExtractor) Leave(n ast.Node) (ast.Node, bool) {
	//nolint: revive
	switch v := n.(type) {
	case *ast.WindowFuncExpr:
		a.windowFuncs = append(a.windowFuncs, v)
	}
	return n, true
}

// physicalSchemaProducer stores the schema for the physical plans who can produce schema directly.
type physicalSchemaProducer struct {
	schema *expression.Schema
	physicalop.BasePhysicalPlan
}

func (s *physicalSchemaProducer) cloneForPlanCacheWithSelf(newCtx base.PlanContext, newSelf base.PhysicalPlan) (*physicalSchemaProducer, bool) {
	cloned := new(physicalSchemaProducer)
	cloned.schema = s.schema
	base, ok := s.BasePhysicalPlan.CloneForPlanCacheWithSelf(newCtx, newSelf)
	if !ok {
		return nil, false
	}
	cloned.BasePhysicalPlan = *base
	return cloned, true
}

func (s *physicalSchemaProducer) cloneWithSelf(newCtx base.PlanContext, newSelf base.PhysicalPlan) (*physicalSchemaProducer, error) {
	base, err := s.BasePhysicalPlan.CloneWithSelf(newCtx, newSelf)
	if err != nil {
		return nil, err
	}
	return &physicalSchemaProducer{
		BasePhysicalPlan: *base,
		schema:           s.Schema().Clone(),
	}, nil
}

// Schema implements the Plan.Schema interface.
func (s *physicalSchemaProducer) Schema() *expression.Schema {
	if s.schema == nil {
		if len(s.Children()) == 1 {
			// default implementation for plans has only one child: proprgate child schema.
			// multi-children plans are likely to have particular implementation.
			s.schema = s.Children()[0].Schema().Clone()
		} else {
			s.schema = expression.NewSchema()
		}
	}
	return s.schema
}

// SetSchema implements the Plan.SetSchema interface.
func (s *physicalSchemaProducer) SetSchema(schema *expression.Schema) {
	s.schema = schema
}

// MemoryUsage return the memory usage of physicalSchemaProducer
func (s *physicalSchemaProducer) MemoryUsage() (sum int64) {
	if s == nil {
		return
	}

	sum = s.BasePhysicalPlan.MemoryUsage() + size.SizeOfPointer
	return
}

// baseSchemaProducer stores the schema for the base plans who can produce schema directly.
type baseSchemaProducer struct {
	schema *expression.Schema
	names  types.NameSlice `plan-cache-clone:"shallow"`
	baseimpl.Plan
}

func (s *baseSchemaProducer) cloneForPlanCache(newCtx base.PlanContext) *baseSchemaProducer {
	cloned := new(baseSchemaProducer)
	cloned.Plan = *s.Plan.CloneWithNewCtx(newCtx)
	cloned.schema = s.schema
	cloned.names = s.names
	return cloned
}

// OutputNames returns the outputting names of each column.
func (s *baseSchemaProducer) OutputNames() types.NameSlice {
	return s.names
}

func (s *baseSchemaProducer) SetOutputNames(names types.NameSlice) {
	s.names = names
}

// Schema implements the Plan.Schema interface.
func (s *baseSchemaProducer) Schema() *expression.Schema {
	if s.schema == nil {
		s.schema = expression.NewSchema()
	}
	return s.schema
}

// SetSchema implements the Plan.SetSchema interface.
func (s *baseSchemaProducer) SetSchema(schema *expression.Schema) {
	s.schema = schema
}

func (s *baseSchemaProducer) setSchemaAndNames(schema *expression.Schema, names types.NameSlice) {
	s.schema = schema
	s.names = names
}

// MemoryUsage return the memory usage of baseSchemaProducer
func (s *baseSchemaProducer) MemoryUsage() (sum int64) {
	if s == nil {
		return
	}

	sum = size.SizeOfPointer + size.SizeOfSlice + int64(cap(s.names))*size.SizeOfPointer + s.Plan.MemoryUsage()
	if s.schema != nil {
		sum += s.schema.MemoryUsage()
	}
	for _, name := range s.names {
		sum += name.MemoryUsage()
	}
	return
}

// BuildPhysicalJoinSchema builds the schema of PhysicalJoin from it's children's schema.
func BuildPhysicalJoinSchema(joinType logicalop.JoinType, join base.PhysicalPlan) *expression.Schema {
	leftSchema := join.Children()[0].Schema()
	switch joinType {
	case logicalop.SemiJoin, logicalop.AntiSemiJoin:
		return leftSchema.Clone()
	case logicalop.LeftOuterSemiJoin, logicalop.AntiLeftOuterSemiJoin:
		newSchema := leftSchema.Clone()
		newSchema.Append(join.Schema().Columns[join.Schema().Len()-1])
		return newSchema
	}
	newSchema := expression.MergeSchema(leftSchema, join.Children()[1].Schema())
	if joinType == logicalop.LeftOuterJoin {
		util.ResetNotNullFlag(newSchema, leftSchema.Len(), newSchema.Len())
	} else if joinType == logicalop.RightOuterJoin {
		util.ResetNotNullFlag(newSchema, 0, leftSchema.Len())
	}
	return newSchema
}

// GetStatsInfo gets the statistics info from a physical plan tree.
func GetStatsInfo(i any) map[string]uint64 {
	if i == nil {
		// it's a workaround for https://github.com/pingcap/tidb/issues/17419
		// To entirely fix this, uncomment the assertion in TestPreparedIssue17419
		return nil
	}
	p := i.(base.Plan)
	var physicalPlan base.PhysicalPlan
	switch x := p.(type) {
	case *Insert:
		physicalPlan = x.SelectPlan
	case *Update:
		physicalPlan = x.SelectPlan
	case *Delete:
		physicalPlan = x.SelectPlan
	case base.PhysicalPlan:
		physicalPlan = x
	}

	if physicalPlan == nil {
		return nil
	}

	statsInfos := make(map[string]uint64)
	statsInfos = CollectPlanStatsVersion(physicalPlan, statsInfos)
	return statsInfos
}

// extractStringFromStringSet helps extract string info from set.StringSet.
func extractStringFromStringSet(set set.StringSet) string {
	if len(set) < 1 {
		return ""
	}
	l := make([]string, 0, len(set))
	for k := range set {
		l = append(l, fmt.Sprintf(`"%s"`, k))
	}
	slices.Sort(l)
	return strings.Join(l, ",")
}

// extractStringFromStringSlice helps extract string info from []string.
func extractStringFromStringSlice(ss []string) string {
	if len(ss) < 1 {
		return ""
	}
	slices.Sort(ss)
	return strings.Join(ss, ",")
}

// extractStringFromUint64Slice helps extract string info from uint64 slice.
func extractStringFromUint64Slice(slice []uint64) string {
	if len(slice) < 1 {
		return ""
	}
	l := make([]string, 0, len(slice))
	for _, k := range slice {
		l = append(l, fmt.Sprintf(`%d`, k))
	}
	slices.Sort(l)
	return strings.Join(l, ",")
}

// extractStringFromBoolSlice helps extract string info from bool slice.
func extractStringFromBoolSlice(slice []bool) string {
	if len(slice) < 1 {
		return ""
	}
	l := make([]string, 0, len(slice))
	for _, k := range slice {
		l = append(l, fmt.Sprintf(`%t`, k))
	}
	slices.Sort(l)
	return strings.Join(l, ",")
}

func tableHasDirtyContent(ctx base.PlanContext, tableInfo *model.TableInfo) bool {
	pi := tableInfo.GetPartitionInfo()
	if pi == nil {
		return ctx.HasDirtyContent(tableInfo.ID)
	}
	// Currently, we add UnionScan on every partition even though only one partition's data is changed.
	// This is limited by current implementation of Partition Prune. It'll be updated once we modify that part.
	for _, partition := range pi.Definitions {
		if ctx.HasDirtyContent(partition.ID) {
			return true
		}
	}
	return false
}

func clonePhysicalPlan(sctx base.PlanContext, plans []base.PhysicalPlan) ([]base.PhysicalPlan, error) {
	cloned := make([]base.PhysicalPlan, 0, len(plans))
	for _, p := range plans {
		c, err := p.Clone(sctx)
		if err != nil {
			return nil, err
		}
		cloned = append(cloned, c)
	}
	return cloned, nil
}

// EncodeUniqueIndexKey encodes a unique index key.
func EncodeUniqueIndexKey(ctx sessionctx.Context, tblInfo *model.TableInfo, idxInfo *model.IndexInfo, idxVals []types.Datum, tID int64) (_ []byte, err error) {
	encodedIdxVals, err := EncodeUniqueIndexValuesForKey(ctx, tblInfo, idxInfo, idxVals)
	if err != nil {
		return nil, err
	}
	return tablecodec.EncodeIndexSeekKey(tID, idxInfo.ID, encodedIdxVals), nil
}

// EncodeUniqueIndexValuesForKey encodes unique index values for a key.
func EncodeUniqueIndexValuesForKey(ctx sessionctx.Context, tblInfo *model.TableInfo, idxInfo *model.IndexInfo, idxVals []types.Datum) (_ []byte, err error) {
	sc := ctx.GetSessionVars().StmtCtx
	for i := range idxVals {
		colInfo := tblInfo.Columns[idxInfo.Columns[i].Offset]
		// table.CastValue will append 0x0 if the string value's length is smaller than the BINARY column's length.
		// So we don't use CastValue for string value for now.
		// TODO: The first if branch should have been removed, because the functionality of set the collation of the datum
		// have been moved to util/ranger (normal path) and getNameValuePairs/getPointGetValue (fast path). But this change
		// will be cherry-picked to a hotfix, so we choose to be a bit conservative and keep this for now.
		if colInfo.GetType() == mysql.TypeString || colInfo.GetType() == mysql.TypeVarString || colInfo.GetType() == mysql.TypeVarchar {
			var str string
			str, err = idxVals[i].ToString()
			idxVals[i].SetString(str, idxVals[i].Collation())
		} else if colInfo.GetType() == mysql.TypeEnum && (idxVals[i].Kind() == types.KindString || idxVals[i].Kind() == types.KindBytes || idxVals[i].Kind() == types.KindBinaryLiteral) {
			var str string
			var e types.Enum
			str, err = idxVals[i].ToString()
			if err != nil {
				return nil, kv.ErrNotExist
			}
			e, err = types.ParseEnumName(colInfo.FieldType.GetElems(), str, colInfo.FieldType.GetCollate())
			if err != nil {
				return nil, kv.ErrNotExist
			}
			idxVals[i].SetMysqlEnum(e, colInfo.FieldType.GetCollate())
		} else {
			// If a truncated error or an overflow error is thrown when converting the type of `idxVal[i]` to
			// the type of `colInfo`, the `idxVal` does not exist in the `idxInfo` for sure.
			idxVals[i], err = table.CastValue(ctx, idxVals[i], colInfo, true, false)
			if types.ErrOverflow.Equal(err) || types.ErrDataTooLong.Equal(err) ||
				types.ErrTruncated.Equal(err) || types.ErrTruncatedWrongVal.Equal(err) {
				return nil, kv.ErrNotExist
			}
		}
		if err != nil {
			return nil, err
		}
	}

	encodedIdxVals, err := codec.EncodeKey(sc.TimeZone(), nil, idxVals...)
	err = sc.HandleError(err)
	if err != nil {
		return nil, err
	}
	return encodedIdxVals, nil
}
