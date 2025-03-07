// Copyright 2019-2021 The Inspektor Gadget authors
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

package types

import (
	"fmt"
	"sort"
	"syscall"
)

type SortBy int

const (
	ALL SortBy = iota
	SENT
	RECEIVED
)

const (
	SortByAll      = "all"
	SortBySent     = "sent"
	SortByReceived = "received"
)

var SortBySlice = []string{SortByAll, SortBySent, SortByReceived}

const (
	MaxRowsDefault  = 20
	IntervalDefault = 1
	SortByDefault   = ALL
)

const (
	IntervalParam = "interval"
	MaxRowsParam  = "max_rows"
	SortByParam   = "sort_by"
	PidParam      = "pid"
	FamilyParam   = "family"
)

func (s SortBy) String() string {
	if int(s) < 0 || int(s) >= len(SortBySlice) {
		return "INVALID"
	}

	return SortBySlice[int(s)]
}

func ParseSortBy(sortby string) (SortBy, error) {
	for i, v := range SortBySlice {
		if v == sortby {
			return SortBy(i), nil
		}
	}
	return ALL, fmt.Errorf("%q is not a valid sort by value", sortby)
}

func ParseFilterByFamily(family string) (int32, error) {
	switch family {
	case "4":
		return syscall.AF_INET, nil
	case "6":
		return syscall.AF_INET6, nil
	default:
		return -1, fmt.Errorf("IP version is either 4 or 6, %s was given", family)
	}
}

// Event is the information the gadget sends to the client each capture
// interval
type Event struct {
	Error string `json:"error,omitempty"`

	// Node where the event comes from.
	Node string `json:"node,omitempty"`

	Stats []Stats `json:"stats,omitempty"`
}

// Stats represents the operations performed on a single file
type Stats struct {
	Node      string `json:"node,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Container string `json:"container,omitempty"`

	Saddr     string `json:"saddr,omitempty"`
	Daddr     string `json:"daddr,omitempty"`
	MountNsID uint64 `json:"mountnsid,omitempty"`
	Pid       int32  `json:"pid,omitempty"`
	Comm      string `json:"comm,omitempty"`
	Sport     uint16 `json:"sport,omitempty"`
	Dport     uint16 `json:"dport,omitempty"`
	Family    uint16 `json:"family,omitempty"`
	Sent      uint64 `json:"sent,omitempty"`
	Received  uint64 `json:"received,omitempty"`
}

func SortStats(stats []Stats, sortBy SortBy) {
	sort.Slice(stats, func(i, j int) bool {
		a := stats[i]
		b := stats[j]

		switch sortBy {
		case SENT:
			return a.Sent > b.Sent
		case RECEIVED:
			return a.Received > b.Received
		default:
			return a.Sent > b.Sent && a.Received > b.Received
		}
	})
}
