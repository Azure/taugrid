// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"sort"
	"sync"
)

var teamQuotaLocks sync.Map

func lockTeamQuota(keys ...string) func() {
	unique := map[string]struct{}{"capacity-pool": {}}
	for _, key := range keys {
		if key != "" {
			unique[key] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(unique))
	for key := range unique {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	locks := make([]*sync.Mutex, 0, len(sorted))
	for _, key := range sorted {
		value, _ := teamQuotaLocks.LoadOrStore(key, &sync.Mutex{})
		lock := value.(*sync.Mutex)
		lock.Lock()
		locks = append(locks, lock)
	}
	return func() {
		for index := len(locks) - 1; index >= 0; index-- {
			locks[index].Unlock()
		}
	}
}

func teamQuotaLockKey(namespace, team string) string {
	return namespace + "\x00" + team
}
