// Copyright 2022 Matrix Origin
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

package cnservice

import (
	"github.com/matrixorigin/matrixone/pkg/hakeeper"
	"github.com/matrixorigin/matrixone/pkg/hakeeper/operator"
	pb "github.com/matrixorigin/matrixone/pkg/pb/logservice"
)

func Check(cfg hakeeper.Config, infos pb.CNState, user pb.TaskTableUser, currentTick uint64) (operators []*operator.Operator) {
	if user.Username == "" {
		return
	}
	working, _ := parseCNStores(cfg, infos, currentTick)
	for _, store := range working {
		if !infos.Stores[store].TaskServiceCreated {
			operators = append(operators, operator.CreateTaskServiceOp("",
				store, pb.CNService, user))
		}
	}
	return operators
}

// parseCNStores returns all expired stores' ids.
func parseCNStores(cfg hakeeper.Config, infos pb.CNState, currentTick uint64) ([]string, []string) {
	working := make([]string, 0)
	expired := make([]string, 0)
	for uuid, storeInfo := range infos.Stores {
		if cfg.CNStoreExpired(storeInfo.Tick, currentTick) {
			expired = append(expired, uuid)
		} else {
			working = append(working, uuid)
		}
	}

	return working, expired
}
