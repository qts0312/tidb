// Copyright 2019 PingCAP, Inc.
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

package expression

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/mock"
	"github.com/stretchr/testify/require"
)

type mockVecPlusIntBuiltinFunc struct {
	baseBuiltinFunc

	buf         *chunk.Column
	enableAlloc bool
}

func (p *mockVecPlusIntBuiltinFunc) allocBuf(n int) (*chunk.Column, error) {
	if p.enableAlloc {
		return p.bufAllocator.get()
	}
	if p.buf == nil {
		p.buf = chunk.NewColumn(types.NewFieldType(mysql.TypeLonglong), n)
	}
	return p.buf, nil
}

func (p *mockVecPlusIntBuiltinFunc) releaseBuf(buf *chunk.Column) {
	if p.enableAlloc {
		p.bufAllocator.put(buf)
	}
}

func (p *mockVecPlusIntBuiltinFunc) vecEvalInt(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	n := input.NumRows()
	buf, err := p.allocBuf(n)
	if err != nil {
		return err
	}
	defer p.releaseBuf(buf)
	if err := p.args[0].VecEvalInt(ctx, input, result); err != nil {
		return err
	}
	if err := p.args[1].VecEvalInt(ctx, input, buf); err != nil {
		return err
	}
	dst64s := result.Int64s()
	src64s := buf.Int64s()
	for i := range dst64s {
		dst64s[i] += src64s[i]
	}
	for i := range n {
		if buf.IsNull(i) && !result.IsNull(i) {
			result.SetNull(i, true)
		}
	}
	return nil
}

func genMockVecPlusIntBuiltinFunc(ctx BuildContext) (*mockVecPlusIntBuiltinFunc, *chunk.Chunk, *chunk.Column) {
	tp := types.NewFieldType(mysql.TypeLonglong)
	col1 := newColumn(0)
	col1.Index, col1.RetType = 0, tp
	col2 := newColumn(1)
	col2.Index, col2.RetType = 1, tp
	bf, err := newBaseBuiltinFuncWithTp(ctx, "", []Expression{col1, col2}, types.ETInt, types.ETInt, types.ETInt)
	if err != nil {
		panic(err)
	}
	plus := &mockVecPlusIntBuiltinFunc{bf, nil, false}
	input := chunk.New([]*types.FieldType{tp, tp}, 1024, 1024)
	buf := chunk.NewColumn(types.NewFieldType(mysql.TypeLonglong), 1024)
	for i := range 1024 {
		input.AppendInt64(0, int64(i))
		input.AppendInt64(1, int64(i))
	}
	return plus, input, buf
}

func TestMockVecPlusInt(t *testing.T) {
	ctx := mock.NewContext()
	plus, input, buf := genMockVecPlusIntBuiltinFunc(ctx)
	plus.enableAlloc = false
	require.NoError(t, plus.vecEvalInt(ctx, input, buf))
	for i := range 1024 {
		require.False(t, buf.IsNull(i))
		require.Equal(t, int64(i*2), buf.GetInt64(i))
	}

	plus.enableAlloc = true
	require.NoError(t, plus.vecEvalInt(ctx, input, buf))
	for i := range 1024 {
		require.False(t, buf.IsNull(i))
		require.Equal(t, int64(i*2), buf.GetInt64(i))
	}
}

func TestMockVecPlusIntParallel(t *testing.T) {
	ctx := mock.NewContext()
	plus, input, buf := genMockVecPlusIntBuiltinFunc(ctx)
	plus.enableAlloc = true // it's concurrency-safe if enableAlloc is true
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := buf.CopyConstruct(nil)
			for range 10 {
				require.NoError(t, plus.vecEvalInt(ctx, input, result))
				for i := range 1024 {
					require.False(t, result.IsNull(i))
					require.Equal(t, int64(i*2), result.GetInt64(i))
				}
			}
		}()
	}
	wg.Wait()
}

const (
	numColumnPoolOp = 4096
)

func BenchmarkColumnPoolGet(b *testing.B) {
	allocator := newLocalColumnPool()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range numColumnPoolOp {
			_, _ = allocator.get()
		}
	}
}

func BenchmarkColumnPoolGetParallel(b *testing.B) {
	allocator := newLocalColumnPool()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			for range numColumnPoolOp {
				_, _ = allocator.get()
			}
		}
	})
}

func BenchmarkColumnPoolGetPut(b *testing.B) {
	allocator := newLocalColumnPool()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for range numColumnPoolOp {
			col, _ := allocator.get()
			allocator.put(col)
		}
	}
}

func BenchmarkColumnPoolGetPutParallel(b *testing.B) {
	allocator := newLocalColumnPool()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			for range numColumnPoolOp {
				col, _ := allocator.get()
				allocator.put(col)
			}
		}
	})
}

func BenchmarkPlusIntBufAllocator(b *testing.B) {
	ctx := mock.NewContext()
	plus, input, buf := genMockVecPlusIntBuiltinFunc(ctx)
	names := []string{"enable", "disable"}
	enable := []bool{true, false}
	for i := range enable {
		b.Run(names[i], func(b *testing.B) {
			plus.enableAlloc = enable[i]
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := plus.vecEvalInt(ctx, input, buf); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type mockBuiltinDouble struct {
	baseBuiltinFunc

	evalType  types.EvalType
	enableVec bool
}

func (p *mockBuiltinDouble) vectorized() bool {
	return p.enableVec
}

func (p *mockBuiltinDouble) vecEvalInt(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	if err := p.args[0].VecEvalInt(ctx, input, result); err != nil {
		return err
	}
	i64s := result.Int64s()
	for i := range i64s {
		i64s[i] <<= 1
	}
	return nil
}

func (p *mockBuiltinDouble) vecEvalReal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	if err := p.args[0].VecEvalReal(ctx, input, result); err != nil {
		return err
	}
	f64s := result.Float64s()
	for i := range f64s {
		f64s[i] *= 2
	}
	return nil
}

func (p *mockBuiltinDouble) vecEvalString(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	var buf *chunk.Column
	var err error
	if buf, err = p.baseBuiltinFunc.bufAllocator.get(); err != nil {
		return err
	}
	if err := p.args[0].VecEvalString(ctx, input, buf); err != nil {
		return err
	}
	result.ReserveString(input.NumRows())
	for i := range input.NumRows() {
		str := buf.GetString(i)
		result.AppendString(str + str)
	}
	p.baseBuiltinFunc.bufAllocator.put(buf)
	return nil
}

func (p *mockBuiltinDouble) vecEvalDecimal(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	if err := p.args[0].VecEvalDecimal(ctx, input, result); err != nil {
		return err
	}
	ds := result.Decimals()
	for i := range ds {
		r := new(types.MyDecimal)
		if err := types.DecimalAdd(&ds[i], &ds[i], r); err != nil {
			return err
		}
		ds[i] = *r
	}
	return nil
}

func (p *mockBuiltinDouble) vecEvalTime(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	if err := p.args[0].VecEvalTime(ctx, input, result); err != nil {
		return err
	}
	ts := result.Times()
	for i := range ts {
		d, err := ts[i].ConvertToDuration()
		if err != nil {
			return err
		}
		if ts[i], err = ts[i].Add(typeCtx(ctx), d); err != nil {
			return err
		}
	}
	return nil
}

func (p *mockBuiltinDouble) vecEvalDuration(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	if err := p.args[0].VecEvalDuration(ctx, input, result); err != nil {
		return err
	}
	ds := result.GoDurations()
	for i := range ds {
		ds[i] *= 2
	}
	return nil
}

func (p *mockBuiltinDouble) vecEvalJSON(ctx EvalContext, input *chunk.Chunk, result *chunk.Column) error {
	var buf *chunk.Column
	var err error
	if buf, err = p.baseBuiltinFunc.bufAllocator.get(); err != nil {
		return err
	}
	if err := p.args[0].VecEvalJSON(ctx, input, buf); err != nil {
		return err
	}
	result.ReserveString(input.NumRows())
	for i := range input.NumRows() {
		j := buf.GetJSON(i)
		path, err := types.ParseJSONPathExpr("$.key")
		if err != nil {
			return err
		}
		ret, ok := j.Extract([]types.JSONPathExpression{path})
		if !ok {
			return errors.Errorf("path not found")
		}
		if err := j.UnmarshalJSON(fmt.Appendf(nil, `{"key":%v}`, 2*ret.GetInt64())); err != nil {
			return err
		}
		result.AppendJSON(j)
	}
	p.baseBuiltinFunc.bufAllocator.put(buf)
	return nil
}

func (p *mockBuiltinDouble) evalInt(ctx EvalContext, row chunk.Row) (int64, bool, error) {
	v, isNull, err := p.args[0].EvalInt(ctx, row)
	if err != nil {
		return 0, false, err
	}
	return v * 2, isNull, nil
}

func (p *mockBuiltinDouble) evalReal(ctx EvalContext, row chunk.Row) (float64, bool, error) {
	v, isNull, err := p.args[0].EvalReal(ctx, row)
	if err != nil {
		return 0, false, err
	}
	return v * 2, isNull, nil
}

func (p *mockBuiltinDouble) evalString(ctx EvalContext, row chunk.Row) (string, bool, error) {
	v, isNull, err := p.args[0].EvalString(ctx, row)
	if err != nil {
		return "", false, err
	}
	return v + v, isNull, nil
}

func (p *mockBuiltinDouble) evalDecimal(ctx EvalContext, row chunk.Row) (*types.MyDecimal, bool, error) {
	v, isNull, err := p.args[0].EvalDecimal(ctx, row)
	if err != nil {
		return nil, false, err
	}
	r := new(types.MyDecimal)
	if err := types.DecimalAdd(v, v, r); err != nil {
		return nil, false, err
	}
	return r, isNull, nil
}

func (p *mockBuiltinDouble) evalTime(ctx EvalContext, row chunk.Row) (types.Time, bool, error) {
	v, isNull, err := p.args[0].EvalTime(ctx, row)
	if err != nil {
		return types.ZeroTime, false, err
	}
	d, err := v.ConvertToDuration()
	if err != nil {
		return types.ZeroTime, false, err
	}
	v, err = v.Add(typeCtx(ctx), d)
	return v, isNull, err
}

func (p *mockBuiltinDouble) evalDuration(ctx EvalContext, row chunk.Row) (types.Duration, bool, error) {
	v, isNull, err := p.args[0].EvalDuration(ctx, row)
	if err != nil {
		return types.Duration{}, false, err
	}
	v, err = v.Add(v)
	return v, isNull, err
}

func (p *mockBuiltinDouble) evalJSON(ctx EvalContext, row chunk.Row) (types.BinaryJSON, bool, error) {
	j, isNull, err := p.args[0].EvalJSON(ctx, row)
	if err != nil {
		return types.BinaryJSON{}, false, err
	}
	if isNull {
		return types.BinaryJSON{}, true, nil
	}
	path, err := types.ParseJSONPathExpr("$.key")
	if err != nil {
		return types.BinaryJSON{}, false, err
	}
	ret, ok := j.Extract([]types.JSONPathExpression{path})
	if !ok {
		return types.BinaryJSON{}, true, err
	}
	if err := j.UnmarshalJSON(fmt.Appendf(nil, `{"key":%v}`, 2*ret.GetInt64())); err != nil {
		return types.BinaryJSON{}, false, err
	}
	return j, false, nil
}

func convertETType(eType types.EvalType) (mysqlType byte) {
	switch eType {
	case types.ETInt:
		mysqlType = mysql.TypeLonglong
	case types.ETReal:
		mysqlType = mysql.TypeDouble
	case types.ETDecimal:
		mysqlType = mysql.TypeNewDecimal
	case types.ETDuration:
		mysqlType = mysql.TypeDuration
	case types.ETJson:
		mysqlType = mysql.TypeJSON
	case types.ETString:
		mysqlType = mysql.TypeVarString
	case types.ETDatetime:
		mysqlType = mysql.TypeDatetime
	}
	return
}

func genMockRowDouble(ctx BuildContext, eType types.EvalType, enableVec bool) (builtinFunc, *chunk.Chunk, *chunk.Column, error) {
	mysqlType := convertETType(eType)
	tp := types.NewFieldType(mysqlType)
	col1 := newColumn(1)
	col1.Index = 0
	col1.RetType = tp
	bf, err := newBaseBuiltinFuncWithTp(ctx, "", []Expression{col1}, eType, eType)
	if err != nil {
		return nil, nil, nil, err
	}
	rowDouble := &mockBuiltinDouble{bf, eType, enableVec}
	input := chunk.New([]*types.FieldType{tp}, 1024, 1024)
	buf := chunk.NewColumn(types.NewFieldType(convertETType(eType)), 1024)
	for i := range 1024 {
		switch eType {
		case types.ETInt:
			input.AppendInt64(0, int64(i))
		case types.ETReal:
			input.AppendFloat64(0, float64(i))
		case types.ETDecimal:
			dec := new(types.MyDecimal)
			if err := dec.FromFloat64(float64(i)); err != nil {
				return nil, nil, nil, err
			}
			input.AppendMyDecimal(0, dec)
		case types.ETDuration:
			input.AppendDuration(0, types.Duration{Duration: time.Duration(i)})
		case types.ETJson:
			j := new(types.BinaryJSON)
			if err := j.UnmarshalJSON(fmt.Appendf(nil, `{"key":%v}`, i)); err != nil {
				return nil, nil, nil, err
			}
			input.AppendJSON(0, *j)
		case types.ETString:
			input.AppendString(0, fmt.Sprintf("%v", i))
		case types.ETDatetime:
			t := types.FromDate(i, 0, 0, 0, 0, 0, 0)
			input.AppendTime(0, types.NewTime(t, mysqlType, 0))
		}
	}
	return rowDouble, input, buf, nil
}

func checkVecEval(t *testing.T, eType types.EvalType, sel []int, result *chunk.Column) {
	if sel == nil {
		for i := range 1024 {
			sel = append(sel, i)
		}
	}
	switch eType {
	case types.ETInt:
		i64s := result.Int64s()
		require.Equal(t, len(sel), len(i64s))
		for i, j := range sel {
			require.Equal(t, int64(j*2), i64s[i])
		}
	case types.ETReal:
		f64s := result.Float64s()
		require.Equal(t, len(sel), len(f64s))
		for i, j := range sel {
			require.Equal(t, float64(j*2), f64s[i])
		}
	case types.ETDecimal:
		ds := result.Decimals()
		require.Equal(t, len(sel), len(ds))
		for i, j := range sel {
			dec := new(types.MyDecimal)
			require.NoError(t, dec.FromFloat64(float64(j)))
			rst := new(types.MyDecimal)
			require.NoError(t, types.DecimalAdd(dec, dec, rst))
			require.Equal(t, 0, rst.Compare(&ds[i]))
		}
	case types.ETDuration:
		ds := result.GoDurations()
		require.Equal(t, len(sel), len(ds))
		for i, j := range sel {
			require.Equal(t, time.Duration(j+j), ds[i])
		}
	case types.ETDatetime:
		ds := result.Times()
		require.Equal(t, len(sel), len(ds))
		for i, j := range sel {
			gt := types.FromDate(j, 0, 0, 0, 0, 0, 0)
			tt := types.NewTime(gt, convertETType(eType), 0)
			d, err := tt.ConvertToDuration()
			require.NoError(t, err)
			v, err := tt.Add(mock.NewContext().GetSessionVars().StmtCtx.TypeCtx(), d)
			require.NoError(t, err)
			require.Equal(t, 0, v.Compare(ds[i]))
		}
	case types.ETJson:
		for i, j := range sel {
			path, err := types.ParseJSONPathExpr("$.key")
			require.NoError(t, err)
			ret, ok := result.GetJSON(i).Extract([]types.JSONPathExpression{path})
			require.True(t, ok)
			require.Equal(t, int64(j*2), ret.GetInt64())
		}
	case types.ETString:
		for i, j := range sel {
			require.Equal(t, fmt.Sprintf("%v%v", j, j), result.GetString(i))
		}
	}
}

func vecEvalType(ctx EvalContext, f builtinFunc, eType types.EvalType, input *chunk.Chunk, result *chunk.Column) error {
	ctx = wrapEvalAssert(ctx, f)
	switch eType {
	case types.ETInt:
		return f.vecEvalInt(ctx, input, result)
	case types.ETReal:
		return f.vecEvalReal(ctx, input, result)
	case types.ETDecimal:
		return f.vecEvalDecimal(ctx, input, result)
	case types.ETDuration:
		return f.vecEvalDuration(ctx, input, result)
	case types.ETString:
		return f.vecEvalString(ctx, input, result)
	case types.ETDatetime:
		return f.vecEvalTime(ctx, input, result)
	case types.ETJson:
		return f.vecEvalJSON(ctx, input, result)
	}
	panic("not implement")
}

func TestDoubleRow2Vec(t *testing.T) {
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETDuration, types.ETString, types.ETDatetime, types.ETJson}
	for _, eType := range eTypes {
		ctx := mock.NewContext()
		rowDouble, input, result, err := genMockRowDouble(ctx, eType, false)
		require.NoError(t, err)
		require.NoError(t, vecEvalType(ctx, rowDouble, eType, input, result))
		checkVecEval(t, eType, nil, result)

		sel := []int{0}
		for {
			end := sel[len(sel)-1]
			gap := 1024 - end
			if gap < 10 {
				break
			}
			sel = append(sel, end+rand.Intn(gap-1)+1)
		}
		input.SetSel(sel)
		require.NoError(t, vecEvalType(ctx, rowDouble, eType, input, result))

		checkVecEval(t, eType, sel, result)
	}
}

func TestDoubleVec2Row(t *testing.T) {
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETDuration, types.ETString, types.ETDatetime, types.ETJson}
	for _, eType := range eTypes {
		mockCtx := mock.NewContext()
		rowDouble, input, result, err := genMockRowDouble(mockCtx, eType, true)
		ctx := wrapEvalAssert(mockCtx, rowDouble)
		result.Reset(eType)
		require.NoError(t, err)
		it := chunk.NewIterator4Chunk(input)
		for row := it.Begin(); row != it.End(); row = it.Next() {
			switch eType {
			case types.ETInt:
				v, _, err := rowDouble.evalInt(ctx, row)
				require.NoError(t, err)
				result.AppendInt64(v)
			case types.ETReal:
				v, _, err := rowDouble.evalReal(ctx, row)
				require.NoError(t, err)
				result.AppendFloat64(v)
			case types.ETDecimal:
				v, _, err := rowDouble.evalDecimal(ctx, row)
				require.NoError(t, err)
				result.AppendMyDecimal(v)
			case types.ETDuration:
				v, _, err := rowDouble.evalDuration(ctx, row)
				require.NoError(t, err)
				result.AppendDuration(v)
			case types.ETString:
				v, _, err := rowDouble.evalString(ctx, row)
				require.NoError(t, err)
				result.AppendString(v)
			case types.ETDatetime:
				v, _, err := rowDouble.evalTime(ctx, row)
				require.NoError(t, err)
				result.AppendTime(v)
			case types.ETJson:
				v, _, err := rowDouble.evalJSON(ctx, row)
				require.NoError(t, err)
				result.AppendJSON(v)
			}
		}
		checkVecEval(t, eType, nil, result)
	}
}

func evalRows(b *testing.B, ctx EvalContext, it *chunk.Iterator4Chunk, eType types.EvalType, result *chunk.Column, rowDouble builtinFunc) {
	switch eType {
	case types.ETInt:
		for i := 0; i < b.N; i++ {
			result.Reset(eType)
			for r := it.Begin(); r != it.End(); r = it.Next() {
				v, isNull, err := rowDouble.evalInt(ctx, r)
				if err != nil {
					b.Fatal(err)
				}
				if isNull {
					result.AppendNull()
				} else {
					result.AppendInt64(v)
				}
			}
		}
	case types.ETReal:
		for i := 0; i < b.N; i++ {
			result.Reset(eType)
			for r := it.Begin(); r != it.End(); r = it.Next() {
				v, isNull, err := rowDouble.evalReal(ctx, r)
				if err != nil {
					b.Fatal(err)
				}
				if isNull {
					result.AppendNull()
				} else {
					result.AppendFloat64(v)
				}
			}
		}
	case types.ETDecimal:
		for i := 0; i < b.N; i++ {
			result.Reset(eType)
			for r := it.Begin(); r != it.End(); r = it.Next() {
				v, isNull, err := rowDouble.evalDecimal(ctx, r)
				if err != nil {
					b.Fatal(err)
				}
				if isNull {
					result.AppendNull()
				} else {
					result.AppendMyDecimal(v)
				}
			}
		}
	case types.ETDuration:
		for i := 0; i < b.N; i++ {
			result.Reset(eType)
			for r := it.Begin(); r != it.End(); r = it.Next() {
				v, isNull, err := rowDouble.evalDuration(ctx, r)
				if err != nil {
					b.Fatal(err)
				}
				if isNull {
					result.AppendNull()
				} else {
					result.AppendDuration(v)
				}
			}
		}
	case types.ETString:
		for i := 0; i < b.N; i++ {
			result.Reset(eType)
			for r := it.Begin(); r != it.End(); r = it.Next() {
				v, isNull, err := rowDouble.evalString(ctx, r)
				if err != nil {
					b.Fatal(err)
				}
				if isNull {
					result.AppendNull()
				} else {
					result.AppendString(v)
				}
			}
		}
	case types.ETDatetime:
		for i := 0; i < b.N; i++ {
			result.Reset(eType)
			for r := it.Begin(); r != it.End(); r = it.Next() {
				v, isNull, err := rowDouble.evalTime(ctx, r)
				if err != nil {
					b.Fatal(err)
				}
				if isNull {
					result.AppendNull()
				} else {
					result.AppendTime(v)
				}
			}
		}
	case types.ETJson:
		for i := 0; i < b.N; i++ {
			result.Reset(eType)
			for r := it.Begin(); r != it.End(); r = it.Next() {
				v, isNull, err := rowDouble.evalJSON(ctx, r)
				if err != nil {
					b.Fatal(err)
				}
				if isNull {
					result.AppendNull()
				} else {
					result.AppendJSON(v)
				}
			}
		}
	}
}

func BenchmarkMockDoubleRow(b *testing.B) {
	typeNames := []string{"Int", "Real", "decimal", "Duration", "String", "Datetime", "JSON"}
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETDuration, types.ETString, types.ETDatetime, types.ETJson}
	for i, eType := range eTypes {
		b.Run(typeNames[i], func(b *testing.B) {
			ctx := mock.NewContext()
			rowDouble, input, result, _ := genMockRowDouble(ctx, eType, false)
			it := chunk.NewIterator4Chunk(input)
			b.ResetTimer()
			evalRows(b, ctx, it, eType, result, rowDouble)
		})
	}
}

func BenchmarkMockDoubleVec(b *testing.B) {
	typeNames := []string{"Int", "Real", "decimal", "Duration", "String", "Datetime", "JSON"}
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETDuration, types.ETString, types.ETDatetime, types.ETJson}
	for i, eType := range eTypes {
		b.Run(typeNames[i], func(b *testing.B) {
			ctx := mock.NewContext()
			rowDouble, input, result, _ := genMockRowDouble(ctx, eType, true)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := vecEvalType(ctx, rowDouble, eType, input, result); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestVectorizedCheck(t *testing.T) {
	con := &Constant{}
	require.True(t, con.Vectorized())
	col := &Column{}
	require.True(t, col.Vectorized())
	cor := CorrelatedColumn{Column: *col}
	require.True(t, cor.Vectorized())

	ctx := mock.NewContext()
	vecF, _, _, _ := genMockRowDouble(ctx, types.ETInt, true)
	sf := &ScalarFunction{Function: vecF}
	require.True(t, sf.Vectorized())

	rowF, _, _, _ := genMockRowDouble(ctx, types.ETInt, false)
	sf = &ScalarFunction{Function: rowF}
	require.False(t, sf.Vectorized())
}

func genFloat32Col() (*Column, *chunk.Chunk, *chunk.Column) {
	typeFloat := types.NewFieldType(mysql.TypeFloat)
	col := &Column{Index: 0, RetType: typeFloat}
	chk := chunk.NewChunkWithCapacity([]*types.FieldType{typeFloat}, 1024)
	for range 1024 {
		chk.AppendFloat32(0, rand.Float32())
	}
	result := chunk.NewColumn(typeFloat, 1024)
	return col, chk, result
}

func TestFloat32ColVec(t *testing.T) {
	col, chk, result := genFloat32Col()
	ctx := mock.NewContext()
	require.NoError(t, col.VecEvalReal(ctx, chk, result))
	it := chunk.NewIterator4Chunk(chk)
	i := 0
	for row := it.Begin(); row != it.End(); row = it.Next() {
		v, _, err := col.EvalReal(ctx, row)
		require.NoError(t, err)
		require.Equal(t, result.GetFloat64(i), v)
		i++
	}

	// set Sel
	n := chk.NumRows()
	sel := make([]int, n/2)
	for i := 0; i < n; i += 2 {
		sel = append(sel, i)
	}
	chk.SetSel(sel)
	require.NoError(t, col.VecEvalReal(ctx, chk, result))
	i = 0
	for row := it.Begin(); row != it.End(); row = it.Next() {
		v, _, err := col.EvalReal(ctx, row)
		require.NoError(t, err)
		require.Equal(t, result.GetFloat64(i), v)
		i++
	}

	require.NoError(t, col.VecEvalReal(ctx, chk, result))
}

func TestVecEvalBool(t *testing.T) {
	ctx := mock.NewContext()
	eTypes := []types.EvalType{types.ETReal, types.ETDecimal, types.ETString, types.ETTimestamp, types.ETDatetime, types.ETDuration}
	for numCols := 1; numCols <= 5; numCols++ {
		for range 16 {
			exprs, input := genVecEvalBool(numCols, nil, eTypes)
			selected, nulls, err := VecEvalBool(ctx, ctx.GetSessionVars().EnableVectorizedExpression, exprs, input, nil, nil)
			require.NoError(t, err)
			it := chunk.NewIterator4Chunk(input)
			i := 0
			for row := it.Begin(); row != it.End(); row = it.Next() {
				ok, null, err := EvalBool(ctx, exprs, row)
				require.NoError(t, err)
				require.Equal(t, nulls[i], null)
				require.Equal(t, selected[i], ok)
				i++
			}
		}
	}
}

func TestRowBasedFilterAndVectorizedFilter(t *testing.T) {
	ctx := mock.NewContext()
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETString, types.ETTimestamp, types.ETDatetime, types.ETDuration}
	for numCols := 1; numCols <= 5; numCols++ {
		for range 16 {
			exprs, input := genVecEvalBool(numCols, nil, eTypes)
			it := chunk.NewIterator4Chunk(input)
			isNull := make([]bool, it.Len())
			selected, nulls, err := rowBasedFilter(ctx, exprs, it, nil, isNull)
			require.NoError(t, err)
			selected2, nulls2, err2 := vectorizedFilter(ctx, ctx.GetSessionVars().EnableVectorizedExpression, exprs, it, nil, isNull)
			require.NoError(t, err2)
			length := it.Len()
			for i := range length {
				require.Equal(t, nulls[i], nulls2[i])
				require.Equal(t, selected[i], selected2[i])
			}
		}
	}
}

func TestVectorizedFilterConsiderNull(t *testing.T) {
	ctx := mock.NewContext()
	dafaultEnableVectorizedExpressionVar := ctx.GetSessionVars().EnableVectorizedExpression
	eTypes := []types.EvalType{types.ETInt, types.ETReal, types.ETDecimal, types.ETString, types.ETTimestamp, types.ETDatetime, types.ETDuration}
	for numCols := 1; numCols <= 5; numCols++ {
		for range 16 {
			exprs, input := genVecEvalBool(numCols, nil, eTypes)
			it := chunk.NewIterator4Chunk(input)
			isNull := make([]bool, it.Len())
			selected, nulls, err := VectorizedFilterConsiderNull(ctx, false, exprs, it, nil, isNull)
			require.NoError(t, err)
			selected2, nulls2, err2 := VectorizedFilterConsiderNull(ctx, true, exprs, it, nil, isNull)
			require.NoError(t, err2)
			length := it.Len()
			for i := range length {
				require.Equal(t, nulls[i], nulls2[i])
				require.Equal(t, selected[i], selected2[i])
			}

			// add test which sel is not nil
			randomSel := generateRandomSel()
			input.SetSel(randomSel)
			it2 := chunk.NewIterator4Chunk(input)
			isNull = isNull[:0]
			selected3, nulls, err := VectorizedFilterConsiderNull(ctx, false, exprs, it2, nil, isNull)
			require.NoError(t, err)
			ctx.GetSessionVars().EnableVectorizedExpression = true
			selected4, nulls2, err2 := VectorizedFilterConsiderNull(ctx, true, exprs, it2, nil, isNull)
			require.NoError(t, err2)
			for i := range length {
				require.Equal(t, nulls[i], nulls2[i])
				require.Equal(t, selected3[i], selected4[i])
			}

			unselected := make([]bool, length)
			// unselected[i] == false means that the i-th row is selected
			for i := range length {
				unselected[i] = true
			}
			for _, idx := range randomSel {
				unselected[idx] = false
			}
			for i := range selected2 {
				if selected2[i] && unselected[i] {
					selected2[i] = false
				}
			}
			for i := range length {
				require.Equal(t, selected4[i], selected2[i])
			}
		}
	}
	ctx.GetSessionVars().EnableVectorizedExpression = dafaultEnableVectorizedExpressionVar
}

func BenchmarkFloat32ColRow(b *testing.B) {
	col, chk, _ := genFloat32Col()
	ctx := mock.NewContext()
	it := chunk.NewIterator4Chunk(chk)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for row := it.Begin(); row != it.End(); row = it.Next() {
			if _, _, err := col.EvalReal(ctx, row); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkFloat32ColVec(b *testing.B) {
	col, chk, result := genFloat32Col()
	ctx := mock.NewContext()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := col.VecEvalReal(ctx, chk, result); err != nil {
			b.Fatal(err)
		}
	}
}
