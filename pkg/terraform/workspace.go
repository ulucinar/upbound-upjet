// Copyright 2021 Upbound Inc.
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
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/afero"
	k8sExec "k8s.io/utils/exec"

	"github.com/crossplane/crossplane-runtime/pkg/logging"

	"github.com/upbound/upjet/pkg/metrics"
	"github.com/upbound/upjet/pkg/resource/json"
	tferrors "github.com/upbound/upjet/pkg/terraform/errors"
)

const (
	defaultAsyncTimeout = 1 * time.Hour
)

// WorkspaceOption allows you to configure Workspace objects.
type WorkspaceOption func(*Workspace)

// WithLogger sets the logger of Workspace.
func WithLogger(l logging.Logger) WorkspaceOption {
	return func(w *Workspace) {
		w.logger = l
	}
}

// WithExecutor sets the executor of Workspace.
func WithExecutor(e k8sExec.Interface) WorkspaceOption {
	return func(w *Workspace) {
		w.executor = e
	}
}

// WithLastOperation sets the Last Operation of Workspace.
func WithLastOperation(lo *Operation) WorkspaceOption {
	return func(w *Workspace) {
		w.LastOperation = lo
	}
}

// WithAferoFs lets you set the fs of WorkspaceStore.
func WithAferoFs(fs afero.Fs) WorkspaceOption {
	return func(ws *Workspace) {
		ws.fs = afero.Afero{Fs: fs}
	}
}

func WithFilterFn(filterFn func(string) string) WorkspaceOption {
	return func(w *Workspace) {
		w.filterFn = filterFn
	}
}

// NewWorkspace returns a new Workspace object that operates in the given
// directory.
func NewWorkspace(dir string, opts ...WorkspaceOption) *Workspace {
	w := &Workspace{
		LastOperation: &Operation{},
		dir:           dir,
		logger:        logging.NewNopLogger(),
		fs:            afero.Afero{Fs: afero.NewOsFs()},
	}
	for _, f := range opts {
		f(w)
	}
	return w
}

// CallbackFn is the type of accepted function that can be called after an async
// operation is completed.
type CallbackFn func(error, context.Context) error

// Workspace runs Terraform operations in its directory and holds the information
// about their statuses.
type Workspace struct {
	// LastOperation contains information about the last operation performed.
	LastOperation *Operation

	dir string
	env []string

	logger        logging.Logger
	executor      k8sExec.Interface
	providerInUse InUse
	fs            afero.Afero

	filterFn func(string) string
}

// ApplyAsync makes a terraform apply call without blocking and calls the given
// function once that apply call finishes.
func (w *Workspace) ApplyAsync(callback CallbackFn) error {
	if !w.LastOperation.MarkStart("apply") {
		return errors.Errorf("%s operation that started at %s is still running", w.LastOperation.Type, w.LastOperation.StartTime().String())
	}
	ctx, cancel := context.WithDeadline(context.TODO(), w.LastOperation.StartTime().Add(defaultAsyncTimeout))
	go func() {
		defer cancel()
		out, err := w.runTF(ctx, metrics.ModeASync, "apply", "-auto-approve", "-input=false", "-lock=false", "-json")
		if err != nil {
			err = tferrors.NewApplyFailed(out)
		}
		w.LastOperation.MarkEnd()
		w.logger.Debug("apply async ended", "out", w.filterFn(string(out)))
		defer func() {
			if cErr := callback(err, ctx); cErr != nil {
				w.logger.Info("callback failed", "error", cErr.Error())
			}
		}()
	}()
	return nil
}

// ApplyResult contains the state after the apply operation.
type ApplyResult struct {
	State *json.StateV4
}

// Apply makes a blocking terraform apply call.
func (w *Workspace) Apply(ctx context.Context) (ApplyResult, error) {
	if w.LastOperation.IsRunning() {
		return ApplyResult{}, errors.Errorf("%s operation that started at %s is still running", w.LastOperation.Type, w.LastOperation.StartTime().String())
	}
	out, err := w.runTF(ctx, metrics.ModeSync, "apply", "-auto-approve", "-input=false", "-lock=false", "-json")
	w.logger.Debug("apply ended", "out", w.filterFn(string(out)))
	if err != nil {
		return ApplyResult{}, tferrors.NewApplyFailed(out)
	}
	raw, err := w.fs.ReadFile(filepath.Join(w.dir, "terraform.tfstate"))
	if err != nil {
		return ApplyResult{}, errors.Wrap(err, "cannot read terraform state file")
	}
	s := &json.StateV4{}
	if err := json.JSParser.Unmarshal(raw, s); err != nil {
		return ApplyResult{}, errors.Wrap(err, "cannot unmarshal tfstate file")
	}
	return ApplyResult{State: s}, nil
}

// DestroyAsync makes a non-blocking terraform destroy call. It doesn't accept
// a callback because destroy operations are not time sensitive as ApplyAsync
// where you might need to store the server-side computed information as soon
// as possible.
func (w *Workspace) DestroyAsync(callback CallbackFn) error {
	switch {
	// Destroy call is idempotent and can be called repeatedly.
	case w.LastOperation.Type == "destroy":
		return nil
	// We cannot run destroy until current non-destroy operation is completed.
	// TODO(muvaf): Gracefully terminate the ongoing apply operation?
	case !w.LastOperation.MarkStart("destroy"):
		return errors.Errorf("%s operation that started at %s is still running", w.LastOperation.Type, w.LastOperation.StartTime().String())
	}
	ctx, cancel := context.WithDeadline(context.TODO(), w.LastOperation.StartTime().Add(defaultAsyncTimeout))
	go func() {
		defer cancel()
		out, err := w.runTF(ctx, metrics.ModeASync, "destroy", "-auto-approve", "-input=false", "-lock=false", "-json")
		if err != nil {
			err = tferrors.NewDestroyFailed(out)
		}
		w.LastOperation.MarkEnd()
		w.logger.Debug("destroy async ended", "out", w.filterFn(string(out)))
		defer func() {
			if cErr := callback(err, ctx); cErr != nil {
				w.logger.Info("callback failed", "error", cErr.Error())
			}
		}()
	}()
	return nil
}

// Destroy makes a blocking terraform destroy call.
func (w *Workspace) Destroy(ctx context.Context) error {
	if w.LastOperation.IsRunning() {
		return errors.Errorf("%s operation that started at %s is still running", w.LastOperation.Type, w.LastOperation.StartTime().String())
	}
	out, err := w.runTF(ctx, metrics.ModeSync, "destroy", "-auto-approve", "-input=false", "-lock=false", "-json")
	w.logger.Debug("destroy ended", "out", w.filterFn(string(out)))
	if err != nil {
		return tferrors.NewDestroyFailed(out)
	}
	return nil
}

// RefreshResult contains information about the current state of the resource.
type RefreshResult struct {
	Exists          bool
	ASyncInProgress bool
	State           *json.StateV4
}

// Refresh makes a blocking terraform apply -refresh-only call where only the state file
// is changed with the current state of the resource.
func (w *Workspace) Refresh(ctx context.Context) (RefreshResult, error) {
	switch {
	case w.LastOperation.IsRunning():
		return RefreshResult{
			ASyncInProgress: w.LastOperation.Type == "apply" || w.LastOperation.Type == "destroy",
		}, nil
	case w.LastOperation.IsEnded():
		defer w.LastOperation.Flush()
	}
	out, err := w.runTF(ctx, metrics.ModeSync, "apply", "-refresh-only", "-auto-approve", "-input=false", "-lock=false", "-json")
	w.logger.Debug("refresh ended", "out", w.filterFn(string(out)))
	if err != nil {
		return RefreshResult{}, tferrors.NewRefreshFailed(out)
	}
	raw, err := w.fs.ReadFile(filepath.Join(w.dir, "terraform.tfstate"))
	if err != nil {
		return RefreshResult{}, errors.Wrap(err, "cannot read terraform state file")
	}
	s := &json.StateV4{}
	if err := json.JSParser.Unmarshal(raw, s); err != nil {
		return RefreshResult{}, errors.Wrap(err, "cannot unmarshal tfstate file")
	}
	return RefreshResult{
		Exists: s.GetAttributes() != nil,
		State:  s,
	}, nil
}

// PlanResult returns a summary of comparison between desired and current state
// of the resource.
type PlanResult struct {
	Exists   bool
	UpToDate bool
}

// Plan makes a blocking terraform plan call.
func (w *Workspace) Plan(ctx context.Context) (PlanResult, error) {
	// The last operation is still ongoing.
	if w.LastOperation.IsRunning() {
		return PlanResult{}, errors.Errorf("%s operation that started at %s is still running", w.LastOperation.Type, w.LastOperation.StartTime().String())
	}
	out, err := w.runTF(ctx, metrics.ModeSync, "plan", "-refresh=false", "-input=false", "-lock=false", "-json")
	w.logger.Debug("plan ended", "out", w.filterFn(string(out)))
	if err != nil {
		return PlanResult{}, tferrors.NewPlanFailed(out)
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, `"type":"change_summary"`) {
			line = l
			break
		}
	}
	if line == "" {
		return PlanResult{}, errors.Errorf("cannot find the change summary line in plan log: %s", string(out))
	}
	type plan struct {
		Changes struct {
			Add    float64 `json:"add,omitempty"`
			Change float64 `json:"change,omitempty"`
		} `json:"changes,omitempty"`
	}
	p := &plan{}
	if err := json.JSParser.Unmarshal([]byte(line), p); err != nil {
		return PlanResult{}, errors.Wrap(err, "cannot unmarshal change summary json")
	}
	return PlanResult{
		Exists:   p.Changes.Add == 0,
		UpToDate: p.Changes.Change == 0,
	}, nil
}

func (w *Workspace) runTF(ctx context.Context, execMode metrics.ExecMode, args ...string) ([]byte, error) {
	if len(args) < 1 {
		return nil, errors.New("args cannot be empty")
	}
	if err := w.providerInUse.Increment(); err != nil {
		return nil, errors.Wrap(err, "cannot increment in-use counter for the shared provider")
	}
	defer w.providerInUse.Decrement()
	cmd := w.executor.CommandContext(ctx, "terraform", args...)
	cmd.SetEnv(append(os.Environ(), w.env...))
	cmd.SetDir(w.dir)
	metrics.CLIExecutions.WithLabelValues(args[0], execMode.String()).Inc()
	start := time.Now()
	defer func() {
		metrics.CLITime.WithLabelValues(args[0], execMode.String()).Observe(time.Since(start).Seconds())
		metrics.CLIExecutions.WithLabelValues(args[0], execMode.String()).Dec()
	}()
	return cmd.CombinedOutput()
}
