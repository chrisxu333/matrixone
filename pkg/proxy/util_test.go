// Copyright 2021 - 2023 Matrix Origin
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

package proxy

import (
	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	respOK   = 0
	comQuery = 3
)

func makeSimplePacket(payload string) []byte {
	l := 1 + len(payload)
	data := make([]byte, l+4)
	data[4] = comQuery
	copy(data[5:], payload)
	data[0] = byte(l)
	data[1] = byte(l >> 8)
	data[2] = byte(l >> 16)
	data[3] = 0
	return data
}

func makeOKPacket() []byte {
	l := 1
	data := make([]byte, l+4)
	data[4] = respOK
	data[0] = byte(l)
	data[1] = byte(l >> 8)
	data[2] = byte(l >> 16)
	data[3] = 0
	return data
}

func packetLen(data []byte) (int32, error) {
	if len(data) < 3 {
		return 0, moerr.NewInternalErrorNoCtx("invalid data")
	}
	return int32(uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16), nil
}

func TestPickTunnels(t *testing.T) {
	ts := make(tunnelSet)
	res := pickTunnels(ts, 3)
	require.Equal(t, len(res), 0)

	t1 := &tunnel{}
	ts.add(t1)
	res = pickTunnels(ts, 3)
	require.Equal(t, len(res), 1)

	t2 := &tunnel{}
	ts.add(t2)
	t3 := &tunnel{}
	ts.add(t3)
	res = pickTunnels(ts, 2)
	require.Equal(t, len(res), 2)
}

func TestIsStmtBegin(t *testing.T) {
	stmt := []byte{'n', 'o', 't'}
	r := isStmtBegin(stmt)
	require.False(t, r)

	stmt = []byte{'n', 'o', 't', 'n', 'o', 't', 'n', 'o', 't', 'n', 'o', 't'}
	r = isStmtBegin(stmt)
	require.False(t, r)

	stmt = []byte{'b', 'e', 'g', 'i', 'n'}
	r = isStmtBegin(stmt)
	require.True(t, r)

	stmt = []byte{'B', 'E', 'G', 'I', 'N'}
	r = isStmtBegin(stmt)
	require.True(t, r)
}

func TestIsStmtCommit(t *testing.T) {
	stmt := []byte{'n', 'o', 't'}
	r := isStmtCommit(stmt)
	require.False(t, r)

	stmt = []byte{'n', 'o', 't', 'n', 'o', 't', 'n', 'o', 't', 'n', 'o', 't'}
	r = isStmtCommit(stmt)
	require.False(t, r)

	stmt = []byte{'c', 'o', 'm', 'm', 'i', 't'}
	r = isStmtCommit(stmt)
	require.True(t, r)

	stmt = []byte{'C', 'O', 'M', 'M', 'I', 'T'}
	r = isStmtCommit(stmt)
	require.True(t, r)
}

func TestIsStmtRollback(t *testing.T) {
	stmt := []byte{'n', 'o', 't'}
	r := isStmtRollback(stmt)
	require.False(t, r)

	stmt = []byte{'n', 'o', 't', 'n', 'o', 't', 'n', 'o', 't', 'n', 'o', 't'}
	r = isStmtRollback(stmt)
	require.False(t, r)

	stmt = []byte{'r', 'o', 'l', 'l', 'b', 'a', 'c', 'k'}
	r = isStmtRollback(stmt)
	require.True(t, r)

	stmt = []byte{'R', 'O', 'L', 'L', 'B', 'A', 'C', 'K'}
	r = isStmtRollback(stmt)
	require.True(t, r)
}

func TestSortSlice(t *testing.T) {
	var sorted = []any{"a", "b", "c", "d"}
	var s1 = []any{"c", "b", "a", "d"}
	var s2 = []any{"b", "a", "d", "c"}
	newS1 := sortSlice(s1)
	newS2 := sortSlice(s2)
	for i := 0; i < len(s1); i++ {
		require.Equal(t, sorted[i], newS1[i])
		require.Equal(t, sorted[i], newS2[i])
	}
}

func TestRawHash(t *testing.T) {
	label := labelInfo{
		Tenant: "t1",
		Labels: map[string]string{
			"k1": "v1",
		},
	}
	require.Equal(t, 32, len(rawHash(label)))
}