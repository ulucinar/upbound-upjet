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

const InvalidProviderHandle ProviderHandle = ""

type ProviderScheduler interface {
	Start(ProviderHandle) (InUse, string, error)
}

type InUse interface {
	Increment() error
	Decrement()
}

type NoOpProviderScheduler struct{}

func NewNoOpProviderScheduler() NoOpProviderScheduler {
	return NoOpProviderScheduler{}
}

func (NoOpProviderScheduler) Start(ProviderHandle) (InUse, string, error) {
	return nil, "", nil
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
	runnerOpts []SharedGRPCRunnerOption
	runners    map[ProviderHandle]*schedulerEntry
	ttl        int
	mu         *sync.Mutex
	logger     logging.Logger
}

func NewSharedProviderScheduler(l logging.Logger, ttl int, opts ...SharedGRPCRunnerOption) *SharedProviderScheduler {
	return &SharedProviderScheduler{
		runnerOpts: opts,
		mu:         &sync.Mutex{},
		runners:    make(map[ProviderHandle]*schedulerEntry),
		logger:     l,
		ttl:        ttl,
	}
}

func (s *SharedProviderScheduler) Start(h ProviderHandle) (InUse, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.runners[h]
	switch {
	case r != nil && (r.invocationCount < s.ttl || r.inUse > 0):
		s.logger.Debug("Reusing the provider runner", "handle", h, "invocationCount", r.invocationCount, "ttl", s.ttl)
		rc, err := r.Start()
		return &providerInUse{
			scheduler: s,
			handle:    h,
		}, rc, errors.Wrapf(err, "cannot use already started provider with handle: %s", h)
	case r != nil:
		s.logger.Debug("The provider runner has expired. Attempting to stop...", "handle", h, "invocationCount", r.invocationCount, "ttl", s.ttl)
		if err := r.Stop(); err != nil {
			return nil, "", errors.Wrapf(err, "cannot schedule a new Terraform provider for handle: %s", h)
		}
	}

	runner := NewSharedProvider(s.runnerOpts...)
	r = &schedulerEntry{
		ProviderRunner: runner,
	}
	runner.logger = s.logger.WithValues("handle", h)
	s.runners[h] = r
	rc, err := s.runners[h].Start()
	return &providerInUse{
		scheduler: s,
		handle:    h,
	}, rc, errors.Wrapf(err, "cannot start the scheduled provider runner for handle: %s", h)
}
