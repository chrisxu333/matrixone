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

package hashbuild

import (
	"bytes"

	"github.com/matrixorigin/matrixone/pkg/common/hashmap"
	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/container/index"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/sql/colexec"
	"github.com/matrixorigin/matrixone/pkg/sql/plan"
	"github.com/matrixorigin/matrixone/pkg/vm/process"
)

func String(_ any, buf *bytes.Buffer) {
	buf.WriteString(" hash build ")
}

func Prepare(proc *process.Process, arg any) error {
	var err error

	ap := arg.(*Argument)
	ap.ctr = new(container)
	if ap.NeedHashMap {
		if ap.ctr.mp, err = hashmap.NewStrMap(false, ap.Ibucket, ap.Nbucket, proc.Mp()); err != nil {
			return err
		}
		ap.ctr.vecs = make([]*vector.Vector, len(ap.Conditions))
		ap.ctr.evecs = make([]evalVector, len(ap.Conditions))
	}
	ap.ctr.bat = batch.NewWithSize(len(ap.Typs))
	ap.ctr.bat.Zs = proc.Mp().GetSels()
	for i, typ := range ap.Typs {
		ap.ctr.bat.Vecs[i] = vector.New(typ)
	}

	return nil
}

func Call(idx int, proc *process.Process, arg any) (bool, error) {
	anal := proc.GetAnalyze(idx)
	anal.Start()
	defer anal.Stop()
	ap := arg.(*Argument)
	ctr := ap.ctr
	for {
		switch ctr.state {
		case Build:
			if err := ctr.build(ap, proc, anal); err != nil {
				ap.Free(proc, true)
				return false, err
			}
			ctr.state = End
		default:
			if ctr.bat != nil {
				if ap.NeedHashMap {
					ctr.bat.Ht = hashmap.NewJoinMap(ctr.sels, nil, ctr.mp, ctr.hasNull, ctr.idx)
				}
				proc.SetInputBatch(ctr.bat)
				ctr.bat = nil
			} else {
				proc.SetInputBatch(nil)
			}
			ap.Free(proc, false)
			return true, nil
		}
	}
}

func (ctr *container) build(ap *Argument, proc *process.Process, anal process.Analyze) error {
	var err error

	for {
		bat := <-proc.Reg.MergeReceivers[0].Ch
		if bat == nil {
			break
		}
		if bat.Length() == 0 {
			continue
		}
		anal.Input(bat)
		anal.Alloc(int64(bat.Size()))
		if ctr.bat, err = ctr.bat.Append(proc.Ctx, proc.Mp(), bat); err != nil {
			return err
		}
		bat.Clean(proc.Mp())
	}
	if ctr.bat == nil || ctr.bat.Length() == 0 || !ap.NeedHashMap {
		return nil
	}
	ctr.cleanEvalVectors(proc.Mp())
	if err = ctr.evalJoinCondition(ctr.bat, ap.Conditions, proc); err != nil {
		return err
	}

	if ctr.idx != nil {
		return ctr.indexBuild()
	}

	itr := ctr.mp.NewIterator()
	count := ctr.bat.Length()
	for i := 0; i < count; i += hashmap.UnitLimit {
		n := count - i
		if n > hashmap.UnitLimit {
			n = hashmap.UnitLimit
		}
		rows := ctr.mp.GroupCount()
		vals, zvals, err := itr.Insert(i, n, ctr.vecs)
		if err != nil {
			return err
		}
		for k, v := range vals[:n] {
			if zvals[k] == 0 {
				ctr.hasNull = true
				continue
			}
			if v == 0 {
				continue
			}
			if v > rows {
				ctr.sels = append(ctr.sels, make([]int64, 0, 8))
			}
			ai := int64(v) - 1
			ctr.sels[ai] = append(ctr.sels[ai], int64(i+k))
		}
	}
	return nil
}

func (ctr *container) indexBuild() error {
	// e.g. original data = ["a", "b", "a", "c", "b", "c", "a", "a"]
	//      => dictionary = ["a"->1, "b"->2, "c"->3]
	//      => poses = [1, 2, 1, 3, 2, 3, 1, 1]
	// sels = [[0, 2, 6, 7], [1, 4], [3, 5]]
	ctr.sels = make([][]int64, index.MaxLowCardinality)
	poses := vector.MustTCols[uint16](ctr.idx.GetPoses())
	for k, v := range poses {
		if v == 0 {
			continue
		}
		bucket := int(v) - 1
		if len(ctr.sels[bucket]) == 0 {
			ctr.sels[bucket] = make([]int64, 0, 64)
		}
		ctr.sels[bucket] = append(ctr.sels[bucket], int64(k))
	}
	return nil
}

func (ctr *container) evalJoinCondition(bat *batch.Batch, conds []*plan.Expr, proc *process.Process) error {
	for i, cond := range conds {
		vec, err := colexec.EvalExpr(bat, proc, cond)
		if err != nil || vec.ConstExpand(proc.Mp()) == nil {
			ctr.cleanEvalVectors(proc.Mp())
			return err
		}
		ctr.vecs[i] = vec
		ctr.evecs[i].vec = vec
		ctr.evecs[i].needFree = true
		for j := range bat.Vecs {
			if bat.Vecs[j] == vec {
				ctr.evecs[i].needFree = false
				break
			}
		}

		// 1. multiple equivalent conditions are not considered currently
		// 2. do not want the condition to be an expression
		if len(conds) == 1 && !ctr.evecs[i].needFree {
			if idx, ok := ctr.vecs[i].Index().(*index.LowCardinalityIndex); ok {
				ctr.idx = idx.Dup()
			}
		}
	}
	return nil
}
