// Copyright 2023 Upbound Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package terraform

import (
	"sync"

	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/pkg/errors"
)

type ProviderHandle string

const (
	InvalidProviderHandle ProviderHandle = ""

	ttlBudget = 0.1
)

type ProviderScheduler interface {
	Start(ProviderHandle) (InUse, string, error)
	Stop(ProviderHandle) error
}

type InUse interface {
	Increment() error
	Decrement()
}

type noopInUse struct{}

func (noopInUse) Increment() error {
	return nil
}

func (noopInUse) Decrement() {}

type NoOpProviderScheduler struct{}

func NewNoOpProviderScheduler() NoOpProviderScheduler {
	return NoOpProviderScheduler{}
}

func (NoOpProviderScheduler) Start(ProviderHandle) (InUse, string, error) {
	return noopInUse{}, "", nil
}

func (NoOpProviderScheduler) Stop(ProviderHandle) error {
	return nil
}

type schedulerEntry struct {
	ProviderRunner
	inUse           int
	invocationCount int
}

type providerInUse struct {
	scheduler *SharedProviderScheduler
	handle    ProviderHandle
}

func (p *providerInUse) Increment() error {
	p.scheduler.mu.Lock()
	defer p.scheduler.mu.Unlock()
	r := p.scheduler.runners[p.handle]
	if r == nil {
		return errors.Errorf("cannot mark provider runner as in-use with handle: %s", p.handle)
	}
	r.inUse++
	r.invocationCount++
	return nil
}

func (p *providerInUse) Decrement() {
	p.scheduler.mu.Lock()
	defer p.scheduler.mu.Unlock()
	if p.scheduler.runners[p.handle].inUse == 0 {
		return
	}
	p.scheduler.runners[p.handle].inUse--
}

type SharedProviderScheduler struct {
	runnerOpts []SharedProviderOption
	runners    map[ProviderHandle]*schedulerEntry
	ttl        int
	mu         *sync.Mutex
	logger     logging.Logger
}

func NewSharedProviderScheduler(l logging.Logger, ttl int, opts ...SharedProviderOption) *SharedProviderScheduler {
	return &SharedProviderScheduler{
		runnerOpts: opts,
		mu:         &sync.Mutex{},
		runners:    make(map[ProviderHandle]*schedulerEntry),
		logger:     l,
		ttl:        ttl,
	}
}

func (s *SharedProviderScheduler) Start(h ProviderHandle) (InUse, string, error) {
	logger := s.logger.WithValues("handle", h, "ttl", s.ttl, "ttlBudget", ttlBudget)
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.runners[h]
	switch {
	case r != nil && (r.invocationCount < s.ttl || r.inUse > 0):
		if r.invocationCount > int(float64(s.ttl)*(1+ttlBudget)) {
			logger.Debug("Reuse budget has been exceeded. Caller will need to retry.")
			return nil, "", errors.Errorf("native provider reuse budget has been exceeded: invocationCount: %d, ttl: %d", r.invocationCount, s.ttl)
		}

		logger.Debug("Reusing the provider runner", "invocationCount", r.invocationCount)
		rc, err := r.Start()
		return &providerInUse{
			scheduler: s,
			handle:    h,
		}, rc, errors.Wrapf(err, "cannot use already started provider with handle: %s", h)
	case r != nil:
		logger.Debug("The provider runner has expired. Attempting to stop...", "invocationCount", r.invocationCount)
		if err := r.Stop(); err != nil {
			return nil, "", errors.Wrapf(err, "cannot schedule a new shared provider for handle: %s", h)
		}
	}

	runner := NewSharedProvider(s.runnerOpts...)
	r = &schedulerEntry{
		ProviderRunner: runner,
	}
	runner.logger = logger
	s.runners[h] = r
	logger.Debug("Starting new shared provider...")
	rc, err := s.runners[h].Start()
	return &providerInUse{
		scheduler: s,
		handle:    h,
	}, rc, errors.Wrapf(err, "cannot start the shared provider runner for handle: %s", h)
}

func (s *SharedProviderScheduler) Stop(ProviderHandle) error {
	// noop
	return nil
}

type WorkspaceProviderScheduler struct {
	runner ProviderRunner
	logger logging.Logger
}

func NewWorkspaceProviderScheduler(l logging.Logger, opts ...SharedProviderOption) *WorkspaceProviderScheduler {
	return &WorkspaceProviderScheduler{
		logger: l,
		runner: NewSharedProvider(opts...),
	}
}

func (s *WorkspaceProviderScheduler) Start(ProviderHandle) (InUse, string, error) {
	s.logger.Debug("Starting workspace scoped shared provider runner...")
	reattachConfig, err := s.runner.Start()
	return noopInUse{}, reattachConfig, errors.Wrap(err, "cannot start a workspace provider runner")
}

func (s *WorkspaceProviderScheduler) Stop(ProviderHandle) error {
	s.logger.Debug("Stopping workspace scoped shared provider runner...")
	return errors.Wrap(s.runner.Stop(), "cannot stop a workspace provider runner")
}
