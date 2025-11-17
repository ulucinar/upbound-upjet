// SPDX-FileCopyrightText: 2023 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package terraform

import (
	"sync"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
)

type OperationType string

const (
	CreateOperation OperationType = "create"
	UpdateOperation OperationType = "update"
	DeleteOperation OperationType = "delete"
)

// Operation is the representation of a single Terraform CLI operation.
type Operation struct {
	Type OperationType

	startTime *time.Time
	endTime   *time.Time
	err       error
	mu        sync.RWMutex
}

func (o *Operation) Phase() managed.AsyncPhase {
	switch o.Type {
	case CreateOperation:
		return managed.AsyncCreatePending
	case UpdateOperation:
		return managed.AsyncUpdatePending
	case DeleteOperation:
		return managed.AsyncDeletePending
	}
	return ""
}

func (o *Operation) CompletionPhase() managed.AsyncPhase {
	switch o.Type {
	case CreateOperation:
		if o.err == nil {
			return managed.AsyncCreateSucceeded
		} else {
			return managed.AsyncCreateFailed
		}
	case UpdateOperation:
		if o.err == nil {
			return managed.AsyncUpdateSucceeded
		} else {
			return managed.AsyncUpdateFailed
		}
	case DeleteOperation:
		if o.err == nil {
			return managed.AsyncDeleteSucceeded
		} else {
			return managed.AsyncDeleteFailed
		}
	}
	return ""
}

// MarkStart marks the operation as started atomically after checking
// no previous operation is already running.
// Returns `false` if a previous operation is still in progress.
func (o *Operation) MarkStart(t OperationType) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.startTime != nil && o.endTime == nil {
		return false
	}
	now := time.Now()
	o.Type = t
	o.startTime = &now
	o.endTime = nil
	return true
}

// MarkEnd marks the operation as ended.
func (o *Operation) MarkEnd() {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	o.endTime = &now
}

// Flush cleans the operation information including the registered error from
// the last reconciliation.
// Deprecated: Please use Clear, which allows optionally preserving the error
// from the last reconciliation to implement proper SYNC status condition for
// the asynchronous external clients.
func (o *Operation) Flush() {
	o.Clear(false)
}

// Clear clears the operation information optionally preserving the last
// registered error from the last reconciliation.
func (o *Operation) Clear(preserveError bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Type = ""
	o.startTime = nil
	o.endTime = nil
	if !preserveError {
		o.err = nil
	}
}

// IsEnded returns whether the operation has ended, regardless of its result.
func (o *Operation) IsEnded() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.endTime != nil
}

// IsRunning returns whether there is an ongoing operation.
func (o *Operation) IsRunning() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.startTime != nil && o.endTime == nil
}

// StartTime returns the start time of the current operation.
func (o *Operation) StartTime() time.Time {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return *o.startTime
}

// EndTime returns the end time of the current operation.
func (o *Operation) EndTime() time.Time {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return *o.endTime
}

// SetError records the given error on the current operation.
func (o *Operation) SetError(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.err = err
}

// Error returns the recorded error of the current operation.
func (o *Operation) Error() error {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.err
}
