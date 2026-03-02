/*
Copyright 2026 Shane Utt.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeLabelUpdates(t *testing.T) {
	tests := []struct {
		name         string
		labels       []string
		hasMilestone bool
		wantAdd      []string
		wantRemove   []string
	}{
		{
			name:         "no milestone no labels adds needs-triage",
			labels:       []string{},
			hasMilestone: false,
			wantAdd:      []string{"triage/needs-triage"},
		},
		{
			name:         "no milestone with non-triage labels adds needs-triage",
			labels:       []string{"bug", "area/docs"},
			hasMilestone: false,
			wantAdd:      []string{"triage/needs-triage"},
		},
		{
			name:         "no milestone with accepted removes accepted and adds needs-triage",
			labels:       []string{"triage/accepted"},
			hasMilestone: false,
			wantAdd:      []string{"triage/needs-triage"},
			wantRemove:   []string{"triage/accepted"},
		},
		{
			name:         "no milestone with other triage label keeps it",
			labels:       []string{"triage/needs-information"},
			hasMilestone: false,
		},
		{
			name:         "no milestone with needs-triage and another triage removes needs-triage",
			labels:       []string{"triage/needs-triage", "triage/needs-information"},
			hasMilestone: false,
			wantRemove:   []string{"triage/needs-triage"},
		},
		{
			name:         "no milestone already has needs-triage only",
			labels:       []string{"triage/needs-triage"},
			hasMilestone: false,
		},
		{
			name:         "milestone without accepted adds accepted",
			labels:       []string{},
			hasMilestone: true,
			wantAdd:      []string{"triage/accepted"},
		},
		{
			name:         "milestone with needs-triage replaces with accepted",
			labels:       []string{"triage/needs-triage"},
			hasMilestone: true,
			wantAdd:      []string{"triage/accepted"},
			wantRemove:   []string{"triage/needs-triage"},
		},
		{
			name:         "milestone already accepted no changes",
			labels:       []string{"triage/accepted"},
			hasMilestone: true,
		},
		{
			name:         "milestone with multiple triage labels cleans up",
			labels:       []string{"triage/needs-triage", "triage/needs-information", "bug"},
			hasMilestone: true,
			wantAdd:      []string{"triage/accepted"},
			wantRemove:   []string{"triage/needs-triage", "triage/needs-information"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ComputeLabelUpdates(tt.labels, tt.hasMilestone)

			assert.Equal(t, tt.wantAdd, result.LabelsToAdd, "LabelsToAdd")
			assert.Equal(t, tt.wantRemove, result.LabelsToRemove, "LabelsToRemove")
		})
	}
}

func TestComputeDeclined(t *testing.T) {
	tests := []struct {
		name          string
		labels        []string
		hasMilestone  bool
		state         string
		wantNil       bool
		wantRemove    []string
		wantMilestone bool
		wantClose     bool
	}{
		{
			name:    "not declined returns nil",
			labels:  []string{"bug"},
			state:   "open",
			wantNil: true,
		},
		{
			name:          "declined open with milestone",
			labels:        []string{"triage/declined", "triage/needs-triage"},
			hasMilestone:  true,
			state:         "open",
			wantRemove:    []string{"triage/needs-triage"},
			wantMilestone: true,
			wantClose:     true,
		},
		{
			name:         "declined already closed no milestone",
			labels:       []string{"triage/declined"},
			hasMilestone: false,
			state:        "closed",
		},
		{
			name:         "declined open no other triage labels",
			labels:       []string{"triage/declined", "bug"},
			hasMilestone: false,
			state:        "open",
			wantClose:    true,
		},
		{
			name:          "declined with accepted and milestone",
			labels:        []string{"triage/declined", "triage/accepted"},
			hasMilestone:  true,
			state:         "open",
			wantRemove:    []string{"triage/accepted"},
			wantMilestone: true,
			wantClose:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ComputeDeclined(tt.labels, tt.hasMilestone, tt.state)

			if tt.wantNil {
				require.Nil(t, result)
				return
			}

			require.NotNil(t, result)
			assert.Equal(t, tt.wantRemove, result.LabelsToRemove, "LabelsToRemove")
			assert.Equal(t, tt.wantMilestone, result.RemoveMilestone, "RemoveMilestone")
			assert.Equal(t, tt.wantClose, result.CloseIssue, "CloseIssue")
		})
	}
}
