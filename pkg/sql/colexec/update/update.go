// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package update

import (
	"bytes"
	"fmt"
	"sync/atomic"

	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/sql/util"

	"github.com/matrixorigin/matrixone/pkg/container/nulls"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/container/vector"

	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

var nullRowid [16]byte

func String(arg any, buf *bytes.Buffer) {
	buf.WriteString("update rows")
}

func Prepare(_ *process.Process, _ any) error {
	return nil
}

// the bool return value means whether it completed its work or not
func Call(_ int, proc *process.Process, arg any) (bool, error) {
	p := arg.(*Argument)
	bat := proc.Reg.InputBatch

	// last batch of block
	if bat == nil {
		return true, nil
	}

	// empty batch
	if len(bat.Zs) == 0 {
		return false, nil
	}

	var affectedRows uint64 = 0
	batLen := batch.Length(bat)
	// Fill vector for constant value
	for i := range bat.Vecs {
		bat.Vecs[i] = bat.Vecs[i].ConstExpand(proc.Mp())
	}
	defer bat.Clean(proc.Mp())

	// do null check
	for i, updateCtx := range p.UpdateCtxs {
		tmpBat := &batch.Batch{}
		idx := updateCtx.HideKeyIdx
		tmpBat.Vecs = bat.Vecs[int(idx) : int(idx)+len(updateCtx.OrderAttrs)+1]
		// need to de duplicate
		tmpBat, _ = FilterBatch(tmpBat, batLen, proc)
		if tmpBat == nil {
			panic(any("internal error when filter Batch"))
		}
		tmpBat.Vecs = tmpBat.Vecs[1:]
		tmpBat.Attrs = append(tmpBat.Attrs, updateCtx.UpdateAttrs...)
		tmpBat.Attrs = append(tmpBat.Attrs, updateCtx.OtherAttrs...)
		batch.Reorder(tmpBat, updateCtx.OrderAttrs)

		for j := range tmpBat.Vecs {
			// Not-null check, for more information, please refer to the comments in func InsertValues
			if (p.TableDefVec[i].Cols[j].Primary && !p.TableDefVec[i].Cols[j].Typ.AutoIncr) || (p.TableDefVec[i].Cols[j].Default != nil && !p.TableDefVec[i].Cols[j].Default.NullAbility) {
				if nulls.Any(tmpBat.Vecs[j].Nsp) {
					tmpBat.Clean(proc.Mp())
					return false, moerr.NewConstraintViolation(proc.Ctx, fmt.Sprintf("Column '%s' cannot be null", tmpBat.Attrs[j]))
				}
			}
		}

		if updateCtx.CPkeyColDef != nil {
			names := util.SplitCompositePrimaryKeyColumnName(updateCtx.CPkeyColDef.Name)
			for _, name := range names {
				for i := range tmpBat.Vecs {
					if tmpBat.Attrs[i] == name {
						if nulls.Any(tmpBat.Vecs[i].Nsp) {
							tmpBat.Clean(proc.Mp())
							return false, moerr.NewConstraintViolation(proc.Ctx, fmt.Sprintf("Column '%s' cannot be null", updateCtx.OrderAttrs[i]))
						}
					}
				}
			}
		}

		tmpBat.Clean(proc.Mp())
	}

	// write data
	for i, updateCtx := range p.UpdateCtxs {
		tmpBat := &batch.Batch{}
		idx := updateCtx.HideKeyIdx
		tmpBat.Vecs = bat.Vecs[int(idx) : int(idx)+len(updateCtx.OrderAttrs)+1]
		// need to de duplicate
		var cnt uint64
		tmpBat, cnt = FilterBatch(tmpBat, batLen, proc)
		if tmpBat == nil {
			panic(any("internal error when filter Batch"))
		}

		delBat := &batch.Batch{}
		delBat.Vecs = []*vector.Vector{tmpBat.GetVector(0)}
		delBat.SetZs(delBat.GetVector(0).Length(), proc.Mp())

		tmpBat.Vecs = tmpBat.Vecs[1:]
		tmpBat.Attrs = append(tmpBat.Attrs, updateCtx.UpdateAttrs...)
		tmpBat.Attrs = append(tmpBat.Attrs, updateCtx.OtherAttrs...)
		batch.Reorder(tmpBat, updateCtx.OrderAttrs)
		tmpBat.SetZs(tmpBat.GetVector(0).Length(), proc.Mp())

		// in update, we can get a batch[b(update), b(old)]
		// we should use old b as delete info
		if updateCtx.UniqueIndexDef != nil {
			relIdx := 0
			for num := range updateCtx.UniqueIndexDef.IndexNames {
				if updateCtx.UniqueIndexDef.TableExists[num] {
					rel := updateCtx.UniqueIndexTables[relIdx]
					var attrs []string = nil
					attrs = append(attrs, updateCtx.UpdateAttrs...)
					attrs = append(attrs, updateCtx.OtherAttrs...)
					attrs = append(attrs, updateCtx.IndexAttrs...)
					oldBatch, rowNum := util.BuildUniqueKeyBatch(bat.Vecs[int(idx)+1:], attrs, updateCtx.UniqueIndexDef.Fields[num].Cols, proc)
					if rowNum != 0 {
						err := rel.Delete(proc.Ctx, oldBatch, updateCtx.UniqueIndexDef.Fields[num].Cols[0].Name)
						if err != nil {
							delBat.Clean(proc.Mp())
							tmpBat.Clean(proc.Mp())
							oldBatch.Clean(proc.Mp())
							return false, err
						}
					}
					oldBatch.Clean(proc.Mp())
					relIdx++
				}
			}
		}

		// delete old rows
		err := updateCtx.TableSource.Delete(proc.Ctx, delBat, updateCtx.HideKey)
		if err != nil {
			delBat.Clean(proc.Mp())
			tmpBat.Clean(proc.Mp())
			return false, err
		}
		delBat.Clean(proc.Mp())

		if err := colexec.UpdateInsertBatch(p.Engine, proc.Ctx, proc, p.TableDefVec[i].Cols, tmpBat, p.TableID[i], p.DBName[i], p.TblName[i]); err != nil {
			tmpBat.Clean(proc.Mp())
			return false, err
		}

		if updateCtx.UniqueIndexDef != nil {
			relIdx := 0
			for num := range updateCtx.UniqueIndexDef.IndexNames {
				if updateCtx.UniqueIndexDef.TableExists[num] {
					rel := updateCtx.UniqueIndexTables[relIdx]
					b, rowNum := util.BuildUniqueKeyBatch(tmpBat.Vecs, tmpBat.Attrs, updateCtx.UniqueIndexDef.Fields[num].Cols, proc)
					if rowNum != 0 {
						err = rel.Write(proc.Ctx, b)
						if err != nil {
							b.Clean(proc.Mp())
							tmpBat.Clean(proc.Mp())
							return false, err
						}
					}
					b.Clean(proc.Mp())
				}
			}
		}

		//fill cpkey column
		if updateCtx.CPkeyColDef != nil {
			err := util.FillCompositePKeyBatch(tmpBat, updateCtx.CPkeyColDef, proc)
			if err != nil {
				tmpBat.Clean(proc.Mp())
				return false, err
			}
		}
		tmpBat.SetZs(tmpBat.GetVector(0).Length(), proc.Mp())

		err = updateCtx.TableSource.Write(proc.Ctx, tmpBat)
		if err != nil {
			tmpBat.Clean(proc.Mp())
			return false, err
		}

		affectedRows += cnt

		tmpBat.Clean(proc.Mp())
	}

	atomic.AddUint64(&p.AffectedRows, affectedRows)
	return false, nil
}

func FilterBatch(bat *batch.Batch, batLen int, proc *process.Process) (*batch.Batch, uint64) {
	cnt := uint64(0)
	rbat := batch.NewWithSize(bat.VectorCount()) // new result batch
	for i := 0; i < bat.VectorCount(); i++ {
		rbat.SetVector(int32(i), vector.New(bat.GetVector(int32(i)).GetType()))
	}
	rows := vector.MustTCols[types.Rowid](bat.GetVector(0))
	for j, vec := range bat.Vecs {
		m := make(map[[16]byte]int)
		rvec := rbat.GetVector(int32(j))
		switch vec.GetType().Oid {
		case types.T_bool:
			vs := vector.GetFixedVectorValues[bool](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_int8:
			vs := vector.GetFixedVectorValues[int8](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_int16:
			vs := vector.GetFixedVectorValues[int16](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_int32:
			vs := vector.GetFixedVectorValues[int32](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_int64:
			vs := vector.GetFixedVectorValues[int64](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_uint8:
			vs := vector.GetFixedVectorValues[uint8](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_uint16:
			vs := vector.GetFixedVectorValues[uint16](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_uint32:
			vs := vector.GetFixedVectorValues[uint32](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_uint64:
			vs := vector.GetFixedVectorValues[uint64](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_float32:
			vs := vector.GetFixedVectorValues[float32](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_float64:
			vs := vector.GetFixedVectorValues[float64](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_date:
			vs := vector.GetFixedVectorValues[types.Date](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_time:
			vs := vector.GetFixedVectorValues[types.Time](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_datetime:
			vs := vector.GetFixedVectorValues[types.Datetime](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_timestamp:
			vs := vector.GetFixedVectorValues[types.Timestamp](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_decimal64:
			vs := vector.GetFixedVectorValues[types.Decimal64](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_decimal128:
			vs := vector.GetFixedVectorValues[types.Decimal128](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_TS:
			vs := vector.GetFixedVectorValues[types.TS](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_Rowid:
			vs := vector.GetFixedVectorValues[types.Rowid](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_uuid:
			vs := vector.GetFixedVectorValues[types.Uuid](vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		case types.T_char, types.T_varchar, types.T_blob, types.T_json, types.T_text:
			vs := vector.MustBytesCols(vec)
			if err := appendTuples(j == 0, &cnt, vs, vec.GetNulls(), rvec,
				proc, m, rows); err != nil {
				return nil, 0
			}
		default:
			return nil, 0
		}
	}
	rbat.InitZsOne(batLen)
	return rbat, cnt
}

func appendTuples[T any](flg bool, cnt *uint64, vs []T, nsp *nulls.Nulls, rvec *vector.Vector,
	proc *process.Process, m map[[16]byte]int, rows []types.Rowid) error {
	for i, row := range rows {
		if row == nullRowid {
			continue
		}
		if _, ok := m[row]; ok {
			continue
		}
		m[row] = 1
		if flg {
			(*cnt)++
		}
		if err := rvec.Append(vs[i], nsp.Contains(uint64(i)), proc.Mp()); err != nil {
			return err
		}
	}
	return nil
}

/* XXX the original code is preserved in the form of comments
func FilterBatch(bat *batch.Batch, batLen int, proc *process.Process) (*batch.Batch, uint64) {
	var cnt uint64 = 0

	newBat := &batch.Batch{}
	m := make(map[[16]byte]int, batLen)

	for _, vec := range bat.Vecs {
		v := vector.New(vec.Typ)
		vector.PreAlloc(v, 0, batLen, proc.Mp())
		newBat.Vecs = append(newBat.Vecs, v)
	}

	rows := bat.Vecs[0].Col.([]types.Rowid)
	for idx, row := range rows {
		if _, ok := m[row]; ok {
			continue
		}
		m[row] = 1
		cnt++

		for j, vec := range bat.Vecs {
			var val any
			if nulls.Contains(vec.Nsp, uint64(idx)) {
				nulls.Add(newBat.Vecs[j].Nsp, uint64(cnt)-1)
				val = getIndexValue(idx, vec, true)
			} else {
				val = getIndexValue(idx, vec, false)
			}

			err := newBat.Vecs[j].Append(val, false, proc.Mp())
			if err != nil {
				return nil, 0
			}
		}
	}
	newBat.Zs = make([]int64, batLen)
	return newBat, cnt
}

// XXX isn't this type switch super slow?
func getIndexValue(idx int, v *vector.Vector, isNull bool) any {
	switch v.Typ.Oid {
	case types.T_bool:
		if isNull {
			return false
		}
		col := v.Col.([]bool)
		return col[idx]
	case types.T_int8:
		if isNull {
			return int8(0)
		}
		col := v.Col.([]int8)
		return col[idx]
	case types.T_int16:
		if isNull {
			return int16(0)
		}
		col := v.Col.([]int16)
		return col[idx]
	case types.T_int32:
		if isNull {
			return int32(0)
		}
		col := v.Col.([]int32)
		return col[idx]
	case types.T_int64:
		if isNull {
			return int64(0)
		}
		col := v.Col.([]int64)
		return col[idx]
	case types.T_uint8:
		if isNull {
			return uint8(0)
		}
		col := v.Col.([]uint8)
		return col[idx]
	case types.T_uint16:
		if isNull {
			return uint16(0)
		}
		col := v.Col.([]uint16)
		return col[idx]
	case types.T_uint32:
		if isNull {
			return uint32(0)
		}
		col := v.Col.([]uint32)
		return col[idx]
	case types.T_uint64:
		if isNull {
			return uint64(0)
		}
		col := v.Col.([]uint64)
		return col[idx]
	case types.T_float32:
		if isNull {
			return float32(0)
		}
		col := v.Col.([]float32)
		return col[idx]
	case types.T_float64:
		if isNull {
			return float64(0)
		}
		col := v.Col.([]float64)
		return col[idx]
	case types.T_date:
		if isNull {
			return types.Date(0)
		}
		col := v.Col.([]types.Date)
		return col[idx]
	case types.T_datetime:
		if isNull {
			return types.Datetime(0)
		}
		col := v.Col.([]types.Datetime)
		return col[idx]
	case types.T_timestamp:
		if isNull {
			return types.Timestamp(0)
		}
		col := v.Col.([]types.Timestamp)
		return col[idx]
	case types.T_decimal64:
		if isNull {
			return types.Decimal64([8]byte{})
		}
		col := v.Col.([]types.Decimal64)
		return col[idx]
	case types.T_decimal128:
		if isNull {
			return types.Decimal128([16]byte{})
		}
		col := v.Col.([]types.Decimal128)
		return col[idx]
	case types.T_TS:
		if isNull {
			var ts types.TS
			return ts
		}
		col := v.Col.([]types.TS)
		return col[idx]
	case types.T_Rowid:
		if isNull {
			var z types.Rowid
			return z
		}
		col := v.Col.([]types.Rowid)
		return col[idx]
	case types.T_uuid:
		if isNull {
			return types.Uuid([16]byte{})
		}
		col := v.Col.([]types.Uuid)
		return col[idx]
	case types.T_char, types.T_varchar, types.T_blob, types.T_json:
		if isNull {
			// XXX: Why don't we return nil?
			return []byte{}
		}
		return v.GetBytes(int64(idx))
	default:
		return nil
	}
}
*/
